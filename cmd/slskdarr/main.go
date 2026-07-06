// Command slskdarr is the daemon entrypoint: it loads config, opens the store,
// wires the clients and the pipeline modules, starts the observability
// server, and runs until it receives SIGINT/SIGTERM (graceful shutdown via
// context cancellation).
package main

import (
	"context"
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

	st, err := store.Open(cfg.Store.DSN)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

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
	runner := pipeline.NewRunner(logger, cfg.Pipeline.TickTimeout.Duration,
		wantedSync, discovery, selecting, downloading, importing)

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
	// healthyFn/modulesFn surface the Runner's per-module liveness: healthy
	// only once every module has ticked at least once and none is stale
	// beyond its own staleness window (see pipeline.Runner.Healthy).
	healthyFn := func() bool { return runner.Healthy() }
	modulesFn := func() map[string]time.Time { return runner.Health() }
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
	srv := &http.Server{Addr: cfg.Observ.ListenAddr, Handler: observ.NewServer(reg, statusFn, jobsFn, cancelFn,
		jobDetailFn, jobEventsFn, recentEventsFn, peersFn, healthyFn, modulesFn, retryFn,
		cfg.Pipeline.FailedReviveAfter.Duration, cfg.Pipeline.MaxCandidatesPerAlbum)}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("status server", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("slskdarr started", "status_addr", cfg.Observ.ListenAddr)
	if err := runner.Run(ctx); err != nil {
		logger.Error("pipeline runner stopped", "err", err)
	}
	_ = srv.Shutdown(context.Background())
	logger.Info("slskdarr stopped cleanly")
}

// runHealthcheck loads the config to find the observ listen port, then GETs
// its own /healthz over loopback and returns nil only on a 200 response. This
// is invoked as `slskdarr --healthcheck` by the Dockerfile's HEALTHCHECK,
// since the distroless runtime image has no shell, curl, or wget for Docker
// to exec directly.
func runHealthcheck(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	_, port, err := net.SplitHostPort(cfg.Observ.ListenAddr)
	if err != nil {
		return fmt.Errorf("parse observ.listen_addr %q: %w", cfg.Observ.ListenAddr, err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%s/healthz", port)
	client := &http.Client{Timeout: 5 * time.Second}
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
