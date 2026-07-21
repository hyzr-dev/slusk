// Command slskdarr is the daemon entrypoint: it loads config, opens the store,
// wires the clients and the pipeline modules, starts the observability
// server, and runs until it receives SIGINT/SIGTERM (graceful shutdown via
// context cancellation).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/samuelenocsson/slskdarr/internal/app"
	"github.com/samuelenocsson/slskdarr/internal/config"
	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
	"github.com/samuelenocsson/slskdarr/internal/observ"
	"github.com/samuelenocsson/slskdarr/internal/pipeline"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
	"github.com/samuelenocsson/slskdarr/internal/soulseek"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// peerBackend combines the two port interfaces every peer-facing pipeline
// module (plus app.Jobs' TransferCanceller) needs, so main can wire
// a single value regardless of which backend (slskd or native soulseek) is
// selected. Both embedded interfaces declare Cancel with an identical
// signature; overlapping methods in embedded interfaces are legal since Go
// 1.14.
type peerBackend interface {
	pipeline.PeerSearcher
	pipeline.PeerNetwork
}

const (
	startupTimeout           = 30 * time.Second
	lifecycleShutdownTimeout = 10 * time.Second
	httpReadHeaderTimeout    = 5 * time.Second
	httpReadTimeout          = 15 * time.Second
	httpWriteTimeout         = 30 * time.Second
	httpIdleTimeout          = 60 * time.Second
	healthcheckTimeout       = 5 * time.Second
)

func main() {
	configPath := flag.String("config", "/config/config.toml", "path to config file")
	healthcheck := flag.Bool("healthcheck", false, "check the running instance's /healthz and exit 0 (healthy) or 1 (unhealthy); used by Docker HEALTHCHECK since the distroless image has no shell/curl")
	migrateDestructive := flag.Bool("migrate-destructive", false, "apply pending destructive database migrations (see internal/store/migrate.go) and exit 0, without starting the normal app pipeline")
	flag.Parse()

	if *healthcheck {
		if err := runHealthcheck(*configPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// A LevelVar lets config raise the level after load: the logger must exist
	// before config is read (to report a load failure), yet its verbosity is
	// config-driven. Zero value is LevelInfo, so pre-config logs use info.
	var logLevel slog.LevelVar
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel}))
	// Make slog.Default() fallbacks (store migrations, module log() helpers)
	// emit JSON too, instead of Go's plain-text default handler.
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	logLevel.Set(cfg.Observ.SlogLevel())

	if *migrateDestructive {
		if err := runMigrateDestructive(cfg, logger); err != nil {
			logger.Error("apply destructive migrations", "err", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	startupCtx, cancelStartup := context.WithTimeout(ctx, startupTimeout)
	defer cancelStartup()

	st, err := store.OpenContext(startupCtx, cfg.Store.DSN)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	lidarrClient := lidarr.New(cfg.Lidarr.URL, cfg.Lidarr.APIKey,
		lidarr.WithManualImportTimeout(cfg.Pipeline.ManualImportTimeout.Duration))
	w := cfg.Pipeline.Weights
	scorer := matcher.NewWeighted(matcher.Weights{
		Format: w.Format, Bitrate: w.Bitrate, Reliability: w.Reliability,
		FileCount: w.FileCount, KnownUser: w.KnownUser,
	}, cfg.Pipeline.MinBitrate)
	reg := prometheus.NewRegistry()
	metrics := observ.NewMetrics(reg)

	var soulClient *soulseek.Client
	if cfg.Soulseek.Enabled() {
		soulClient = newSoulseekClient(cfg.Soulseek, cfg.Paths.SlskdCompleteDir, logger)
	}

	// Backend selection: config.Validate already guarantees soulClient != nil
	// when cfg.Pipeline.Backend is BackendSoulseek, so the slskd client is not
	// even constructed in that case.
	var peers peerBackend
	switch cfg.Pipeline.Backend {
	case config.BackendSlskd:
		peers = slskd.New(cfg.Slskd.URL, cfg.Slskd.APIKey)
	case config.BackendSoulseek:
		peers = soulClient
	}

	wantedSync := pipeline.NewWantedSync(pipeline.WantedSyncParams{
		Music:             lidarrClient,
		Store:             st,
		Interval:          cfg.Pipeline.WantedSyncInterval.Duration,
		FailedReviveAfter: cfg.Pipeline.FailedReviveAfter.Duration,
		Logger:            logger,
	})
	discovery := pipeline.NewDiscovery(pipeline.DiscoveryParams{
		Store:         st,
		Peers:         peers,
		Music:         lidarrClient,
		Ranker:        scorer,
		WantedSource:  wantedSync,
		SearchTimeout: cfg.Pipeline.SearchTimeout.Duration,
		MaxCandidates: cfg.Pipeline.MaxCandidatesPerAlbum,
		MaxRetries:    cfg.Pipeline.MaxRetries,
		BackoffBase:   cfg.Pipeline.BackoffBase.Duration,
		BackoffCap:    cfg.Pipeline.BackoffCap.Duration,
		Interval:      cfg.Pipeline.DiscoveryInterval.Duration,
		Logger:        logger,
	})
	selecting := pipeline.NewSelecting(pipeline.SelectingParams{
		Store:              st,
		Peers:              peers,
		MaxActive:          cfg.Pipeline.MaxActive,
		MaxRetries:         cfg.Pipeline.MaxRetries,
		BackoffBase:        cfg.Pipeline.BackoffBase.Duration,
		BackoffCap:         cfg.Pipeline.BackoffCap.Duration,
		CandidateTTL:       cfg.Pipeline.CandidateTTL.Duration,
		MaxInflightPerPeer: cfg.Pipeline.MaxInflightPerPeer,
		MaxTransferRetries: cfg.Pipeline.MaxTransferRetries,
		TransferDeadline:   cfg.Pipeline.TransferDeadline.Duration,
		Interval:           cfg.Pipeline.SelectingInterval.Duration,
		Logger:             logger,
	})
	downloading := pipeline.NewDownloading(pipeline.DownloadingParams{
		Store:              st,
		Network:            peers,
		Peers:              peers,
		Logger:             logger,
		Metrics:            metrics,
		MaxActive:          cfg.Pipeline.MaxActive,
		MaxTransferRetries: cfg.Pipeline.MaxTransferRetries,
		StallTimeout:       cfg.Pipeline.StallTimeout.Duration,
		MaxInflightPerPeer: cfg.Pipeline.MaxInflightPerPeer,
		TransferDeadline:   cfg.Pipeline.TransferDeadline.Duration,
		Interval:           cfg.Pipeline.DownloadingInterval.Duration,
	})
	importing := pipeline.NewImporting(pipeline.ImportingParams{
		Store:                st,
		Music:                lidarrClient,
		Peers:                peers,
		Logger:               logger,
		CompleteDir:          cfg.Paths.SlskdCompleteDir,
		MaxActive:            cfg.Pipeline.MaxActive,
		StuckAfter:           cfg.Pipeline.StuckAfter.Duration,
		ImportConfirmTimeout: cfg.Pipeline.ImportConfirmTimeout.Duration,
		Interval:             cfg.Pipeline.ImportingInterval.Duration,
		RetryCooldown:        cfg.Pipeline.ImportRetryCooldown.Duration,
	})
	runner, err := pipeline.NewRunner(logger, cfg.Pipeline.TickTimeout.Duration,
		wantedSync, discovery, selecting, downloading, importing)
	if err != nil {
		logger.Error("configure pipeline runner", "err", err)
		os.Exit(1)
	}

	statusFn := func(ctx context.Context) (observ.StatusReport, error) {
		active, err := st.ActiveTransfers(ctx)
		if err != nil {
			return observ.StatusReport{}, err
		}
		return observ.StatusReport{Active: len(active)}, nil
	}
	jobsFn := func(ctx context.Context) ([]core.JobView, error) {
		return st.ListJobsWithTransfer(ctx)
	}
	jobDetailFn := func(ctx context.Context, jobID int64) (core.JobDetail, bool, error) {
		return st.JobDetail(ctx, jobID)
	}
	jobEventsFn := func(ctx context.Context, jobID int64) ([]core.JobEvent, error) {
		return st.JobEvents(ctx, jobID)
	}
	recentEventsFn := func(ctx context.Context, limit int) ([]core.JobEvent, error) {
		return st.RecentEvents(ctx, limit)
	}
	peersFn := func(ctx context.Context) ([]core.PeerRow, error) {
		return st.Peers(ctx)
	}
	jobs := &app.Jobs{Store: st, Peers: peers, Logger: logger.With("component", "app")}
	// Liveness only requires modules to keep attempting work. Readiness also
	// requires successful work and fails after sustained errors.
	liveFn := func() bool { return runner.Live() }
	readyFn := func() bool { return runner.Ready() }
	modulesFn := func() map[string]observ.ModuleStatus {
		statuses := runner.Health()
		out := make(map[string]observ.ModuleStatus, len(statuses)+1)
		for name, status := range statuses {
			out[name] = observ.ModuleStatus{
				LastAttempt: status.LastAttempt, LastCompleted: status.LastCompleted,
				LastSuccess: status.LastSuccess, LastErrorAt: status.LastErrorAt,
				LastError: status.LastError, ConsecutiveFailures: status.ConsecutiveFailures,
				StaleDeadline: status.StaleDeadline, Live: status.Live, Ready: status.Ready,
			}
		}
		// The soulseek connection is not a pipeline.Runner module - it does
		// not gate liveFn/readyFn - but its status is still surfaced here so
		// it shows up alongside the pipeline modules on /status.
		if soulClient != nil {
			soulStatus := soulClient.Status()
			out["soulseek"] = observ.ModuleStatus{
				LastAttempt:         soulStatus.LastAttempt,
				LastSuccess:         soulStatus.LastConnectedAt,
				LastErrorAt:         soulStatus.LastErrorAt,
				LastError:           soulStatus.LastError,
				ConsecutiveFailures: soulStatus.ConsecutiveFailures,
				Live:                soulStatus.State != soulseek.StateFailed,
				Ready:               soulStatus.State == soulseek.StateConnected,
			}
		}
		return out
	}
	// The settings view's display config never changes at runtime (it's read
	// once at startup, same as the rest of cfg), so the ConfigFunc closes over
	// a single fixed value instead of re-reading the file.
	appConfig := observ.NewAppConfig(cfg.Lidarr.URL, cfg.Lidarr.APIKey,
		cfg.Pipeline.WantedSyncInterval.Duration.String(), cfg.Pipeline.MaxActive)
	configFn := func() observ.AppConfig { return appConfig }
	handler := observ.NewServerWithReadiness(reg, statusFn, jobsFn, jobs.Cancel,
		jobDetailFn, jobEventsFn, recentEventsFn, peersFn, liveFn, readyFn, modulesFn, jobs.Retry,
		cfg.Pipeline.FailedReviveAfter.Duration, cfg.Pipeline.MaxCandidatesPerAlbum, configFn)
	var authenticator observ.Authenticator
	if cfg.Observ.AuthToken != "" {
		authenticator = observ.NewTokenAuthenticator(cfg.Observ.AuthToken)
	}
	srv := &http.Server{
		Addr:              cfg.Observ.ListenAddr,
		Handler:           observ.ProtectPrivateEndpoints(handler, authenticator),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
	listener, err := listenHTTP(startupCtx, cfg.Observ.ListenAddr)
	if err != nil {
		logger.Error("listen status server", "err", err)
		_ = st.Close()
		os.Exit(1)
	}
	cancelStartup()

	var soulDone chan error
	soulCtx, soulCancel := context.WithCancel(ctx)
	defer soulCancel()
	if soulClient != nil {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)
		go runShareRescanLoop(soulCtx, hup, soulClient, logger)
		soulDone = make(chan error, 1)
		go func() {
			err := soulClient.Run(soulCtx)
			if err != nil {
				logger.Error("soulseek connection stopped permanently", "err", err)
			}
			soulDone <- err
		}()
	}

	logger.Info("slskdarr started", "status_addr", cfg.Observ.ListenAddr)
	outcome := runRuntime(ctx, srv, listener, runner, lifecycleShutdownTimeout)
	// runRuntime may return on an abnormal exit (runner or HTTP server
	// stopping without a signal) while ctx is still live, which would
	// otherwise leave the soulseek client reconnecting indefinitely and
	// force this join to burn the full shutdown timeout. Cancel its
	// context explicitly so the join below is prompt in that case too.
	soulCancel()
	if soulDone != nil {
		select {
		case <-soulDone:
		case <-time.After(lifecycleShutdownTimeout):
			logger.Error("soulseek connection did not stop within the shutdown timeout")
		}
	}
	closeErr := closeStoreAfterRuntime(outcome, st.Close)
	if outcome.err != nil || closeErr != nil {
		logger.Error("slskdarr stopped with error", "runtime_err", outcome.err, "close_store_err", closeErr,
			"store_close_safe", outcome.storeCloseSafe)
		os.Exit(1)
	}
	logger.Info("slskdarr stopped cleanly")
}

func listenHTTP(ctx context.Context, addr string) (net.Listener, error) {
	return (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
}

type runtimeRunner interface {
	Run(context.Context) error
}

type runtimeOutcome struct {
	err            error
	storeCloseSafe bool
}

// closeStoreAfterRuntime closes the shared store only after all pipeline
// modules and HTTP handlers have relinquished ownership of it.
func closeStoreAfterRuntime(outcome runtimeOutcome, closeStore func() error) error {
	if !outcome.storeCloseSafe {
		return nil
	}
	return closeStore()
}

// runRuntime ties the HTTP listener and pipeline runner into one lifecycle.
// An unexpected listener failure cancels the pipeline and is returned as a
// fatal error. Signal-driven shutdown is bounded even if a module or HTTP
// connection does not cooperate. storeCloseSafe is false if bounded shutdown
// expires while a module or handler may still own the shared store.
func runRuntime(ctx context.Context, srv *http.Server, listener net.Listener, runner runtimeRunner, shutdownTimeout time.Duration) runtimeOutcome {
	runtimeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	serverDone := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverDone <- err
	}()
	runnerDone := make(chan error, 1)
	go func() { runnerDone <- runner.Run(runtimeCtx) }()

	var runtimeErr error
	storeCloseSafe := true
	serverFinished, runnerFinished := false, false
	select {
	case <-ctx.Done():
	case err := <-serverDone:
		serverFinished = true
		if err != nil {
			runtimeErr = fmt.Errorf("status server failed: %w", err)
		} else if ctx.Err() == nil {
			runtimeErr = errors.New("status server stopped unexpectedly")
		}
	case err := <-runnerDone:
		runnerFinished = true
		if err != nil {
			runtimeErr = err
			if errors.Is(err, pipeline.ErrShutdownTimeout) {
				storeCloseSafe = false
			}
		} else if ctx.Err() == nil {
			runtimeErr = errors.New("pipeline runner stopped unexpectedly")
		}
	}
	cancel()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		storeCloseSafe = false
		runtimeErr = errors.Join(runtimeErr, fmt.Errorf("shutdown status server: %w", err))
	}
	if !serverFinished {
		select {
		case err := <-serverDone:
			if err != nil {
				runtimeErr = errors.Join(runtimeErr, fmt.Errorf("status server failed: %w", err))
			}
		case <-shutdownCtx.Done():
			storeCloseSafe = false
			runtimeErr = errors.Join(runtimeErr, errors.New("status server shutdown timed out"))
		}
	}
	if !runnerFinished {
		select {
		case err := <-runnerDone:
			runtimeErr = errors.Join(runtimeErr, err)
			if errors.Is(err, pipeline.ErrShutdownTimeout) {
				storeCloseSafe = false
			}
		case <-shutdownCtx.Done():
			storeCloseSafe = false
			runtimeErr = errors.Join(runtimeErr, errors.New("pipeline shutdown timed out"))
		}
	}
	return runtimeOutcome{err: runtimeErr, storeCloseSafe: storeCloseSafe}
}

// runHealthcheck loads the config to find the observ listener, then GETs its
// own /healthz and returns nil only on a 200 response. Wildcard listeners are
// probed on the matching loopback family; specific listeners retain their
// configured host. This is invoked as `slskdarr --healthcheck` by the
// Dockerfile's HEALTHCHECK, since the distroless runtime image has no shell,
// curl, or wget for Docker to exec directly.
func runHealthcheck(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	url, err := healthcheckURL(cfg.Observ.ListenAddr)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return nil
}

// runMigrateDestructive opens the store (which, as always, applies pending
// routine migrations) and then applies any pending destructive migrations
// (internal/store/migrate.go), logging what was applied. It never starts the
// pipeline runner or HTTP server - the process exits immediately afterward.
func runMigrateDestructive(cfg config.Config, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()

	st, err := store.OpenContext(ctx, cfg.Store.DSN)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	if err := st.ApplyDestructiveMigrations(ctx, logger); err != nil {
		return fmt.Errorf("apply destructive migrations: %w", err)
	}
	return nil
}

func healthcheckURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("parse observ.listen_addr %q: %w", listenAddr, err)
	}

	probeHost := host
	ip := net.ParseIP(host)
	if host == "" || (ip != nil && ip.IsUnspecified()) {
		probeHost = "127.0.0.1"
		if ip != nil && ip.To4() == nil {
			probeHost = "::1"
		}
	}
	return "http://" + net.JoinHostPort(probeHost, port) + "/healthz", nil
}
