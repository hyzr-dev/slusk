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

	"github.com/samuelenocsson/slskdarr/internal/config"
	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
	"github.com/samuelenocsson/slskdarr/internal/observ"
	"github.com/samuelenocsson/slskdarr/internal/pipeline"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

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
	flag.Parse()

	if *healthcheck {
		if err := runHealthcheck(*configPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
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
	peers := slskd.New(cfg.Slskd.URL, cfg.Slskd.APIKey)
	lidarrClient := lidarr.New(cfg.Lidarr.URL, cfg.Lidarr.APIKey)
	scorer := matcher.NewWeighted(cfg.Pipeline.Weights, cfg.Pipeline.MinBitrate)
	reg := prometheus.NewRegistry()
	metrics := observ.NewMetrics(reg)

	wantedSync := pipeline.NewWantedSync(pipeline.WantedSyncParams{
		Music:             lidarrClient,
		Store:             st,
		Interval:          cfg.Pipeline.WantedSyncInterval.Duration,
		FailedReviveAfter: cfg.Pipeline.FailedReviveAfter.Duration,
		Logger:            logger,
	})
	discovery := pipeline.NewDiscovery(pipeline.DiscoveryParams{
		Store:                 st,
		Peers:                 peers,
		Music:                 lidarrClient,
		Ranker:                scorer,
		WantedSource:          wantedSync,
		SearchTimeout:         cfg.Pipeline.SearchTimeout.Duration,
		MaxCandidates:         cfg.Pipeline.MaxCandidatesPerAlbum,
		MaxCandidateFileRatio: cfg.Pipeline.MaxCandidateFileRatio,
		MaxRetries:            cfg.Pipeline.MaxRetries,
		BackoffBase:           cfg.Pipeline.BackoffBase.Duration,
		BackoffCap:            cfg.Pipeline.BackoffCap.Duration,
		Interval:              cfg.Pipeline.DiscoveryInterval.Duration,
		Logger:                logger,
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
	// cancelFn cancels a job locally even if the remote slskd cancel call
	// fails: the job's local state must advance to cancelled regardless, since
	// any stale slskd-side entry gets cleaned up by the next reconcile pass.
	cancelFn := func(ctx context.Context, jobID int64) (observ.CancelResult, error) {
		view, found, err := st.JobWithTransfer(ctx, jobID)
		if err != nil {
			return observ.CancelResultFailed, err
		}
		if !found {
			return observ.CancelResultNotFound, nil
		}
		if view.Transfer != nil && view.Transfer.SlskdID != "" {
			if err := peers.Cancel(ctx, view.Transfer.Username, view.Transfer.SlskdID); err != nil {
				logger.Warn("slskd cancel failed, still advancing job state", "job_id", jobID, "err", err)
			}
		}
		if err := st.AdvanceJobState(ctx, jobID, core.StateCancelled, time.Now()); err != nil {
			return observ.CancelResultFailed, err
		}
		return observ.CancelResultOK, nil
	}
	// Liveness only requires modules to keep attempting work. Readiness also
	// requires successful work and fails after sustained errors.
	liveFn := func() bool { return runner.Live() }
	readyFn := func() bool { return runner.Ready() }
	modulesFn := func() map[string]observ.ModuleStatus {
		statuses := runner.Health()
		out := make(map[string]observ.ModuleStatus, len(statuses))
		for name, status := range statuses {
			out[name] = observ.ModuleStatus{
				LastAttempt: status.LastAttempt, LastCompleted: status.LastCompleted,
				LastSuccess: status.LastSuccess, LastErrorAt: status.LastErrorAt,
				LastError: status.LastError, ConsecutiveFailures: status.ConsecutiveFailures,
				StaleDeadline: status.StaleDeadline, Live: status.Live, Ready: status.Ready,
			}
		}
		return out
	}
	// retryFn manually revives a FAILED job from the dashboard: NotFound if no
	// such job exists, Conflict if it exists but is not currently FAILED (the
	// dashboard button raced a state change).
	retryFn := func(ctx context.Context, jobID int64) (observ.RetryResult, error) {
		_, found, err := st.JobWithTransfer(ctx, jobID)
		if err != nil {
			return observ.RetryResultOK, err // result is ignored by the handler when err != nil
		}
		if !found {
			return observ.RetryResultNotFound, nil
		}
		ok, err := st.RetryFailedJob(ctx, jobID, time.Now())
		if err != nil {
			return observ.RetryResultOK, err // result is ignored by the handler when err != nil
		}
		if !ok {
			return observ.RetryResultConflict, nil
		}
		return observ.RetryResultOK, nil
	}
	handler := observ.NewServerWithReadiness(reg, statusFn, jobsFn, cancelFn,
		jobDetailFn, jobEventsFn, recentEventsFn, peersFn, liveFn, readyFn, modulesFn, retryFn,
		cfg.Pipeline.FailedReviveAfter.Duration, cfg.Pipeline.MaxCandidatesPerAlbum)
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

	logger.Info("slskdarr started", "status_addr", cfg.Observ.ListenAddr)
	outcome := runRuntime(ctx, srv, listener, runner, lifecycleShutdownTimeout)
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
