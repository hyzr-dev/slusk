// Command slskdarr is the daemon entrypoint: it loads config, opens the store,
// wires the clients and engine, starts the observability server, and runs until
// it receives SIGINT/SIGTERM (graceful shutdown via context cancellation).
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
	"github.com/samuelenocsson/slskdarr/internal/engine"
	"github.com/samuelenocsson/slskdarr/internal/lidarr"
	"github.com/samuelenocsson/slskdarr/internal/matcher"
	"github.com/samuelenocsson/slskdarr/internal/observ"
	"github.com/samuelenocsson/slskdarr/internal/slskd"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// livenessStalePollFactor bounds how many missed StatusPoll ticks /healthz
// tolerates before reporting the reconcile loop as stalled. The runtime image
// is distroless (no shell/curl), so Docker's HEALTHCHECK execs this same
// binary with --healthcheck to hit that endpoint locally.
const livenessStalePollFactor = 4

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
	reconciler := engine.NewReconciler(peers, st, cfg.Engine.MaxTransferRetries, cfg.Engine.StallTimeout.Duration)
	lidarrClient := lidarr.New(cfg.Lidarr.URL, cfg.Lidarr.APIKey)
	scorer := matcher.NewWeighted(cfg.Engine.Weights, cfg.Engine.MinBitrate)
	discoverer := engine.NewDiscoverer(engine.DiscovererParams{
		Music: lidarrClient, Peers: peers, Store: st, Ranker: scorer,
		CompleteDir:            cfg.Paths.SlskdCompleteDir,
		SearchTimeout:          cfg.Engine.SearchTimeout.Duration,
		TransferDeadline:       cfg.Engine.TransferDeadline.Duration,
		CandidateBackoff:       cfg.Engine.CandidateBackoff.Duration,
		FailedCandidateBackoff: cfg.Engine.FailedCandidateBackoff.Duration,
		FailedRetryAfter:       cfg.Engine.FailedRetryAfter.Duration,
		ImportConfirmTimeout:   cfg.Engine.ImportConfirmTimeout.Duration,
		MaxCandidates:          cfg.Engine.MaxCandidatesPerAlbum,
		Batch:                  cfg.Engine.Batch,
		MaxActive:              cfg.Engine.MaxConcurrentActive,
		MaxInflightPerPeer:     cfg.Engine.MaxInflightPerPeer,
		MaxCandidateFileRatio:  cfg.Engine.MaxCandidateFileRatio,
		MaxTransferRetries:     cfg.Engine.MaxTransferRetries,
		Logger:                 logger,
	})
	reg := prometheus.NewRegistry()
	metrics := observ.NewMetrics(reg)
	eng := engine.New(engine.Params{
		Reconciler:   reconciler,
		Discoverer:   discoverer,
		StatusPoll:   cfg.Slskd.StatusPollInterval.Duration,
		LidarrPoll:   cfg.Lidarr.PollInterval.Duration,
		TickInterval: cfg.Engine.TickInterval.Duration,
		Logger:       logger,
		Metrics:      metrics,
		EventPruner:  st,
	})
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
	// staleAfter tolerates a few missed StatusPoll ticks (e.g. a slow slskd
	// response) before /healthz reports the reconcile loop as stalled -
	// it's driven by StatusPoll alone, so an empty Lidarr wanted list (nothing
	// to discover) never counts as "stuck": reconcileOnce still runs every
	// StatusPoll tick regardless of whether there's anything to reconcile.
	staleAfter := cfg.Slskd.StatusPollInterval.Duration * livenessStalePollFactor
	healthyFn := func() bool { return eng.Healthy(staleAfter) }
	srv := &http.Server{Addr: cfg.Observ.ListenAddr, Handler: observ.NewServer(reg, statusFn, jobsFn, cancelFn,
		jobDetailFn, jobEventsFn, recentEventsFn, peersFn, healthyFn,
		cfg.Engine.FailedRetryAfter.Duration, cfg.Engine.MaxCandidatesPerAlbum)}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("status server", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A crash can leave DOWNLOADING jobs whose attempt is already fully
	// terminal in the transfers table (the Reconciler keeps reconciling
	// transfers against slskd independently of the discovery loop) but were
	// never picked up by advanceDownloading before the process died. Sweeping
	// them once at startup - unbounded by Batch - drains that backlog
	// immediately instead of over dozens of ticks, and unblocks
	// max_concurrent_active if it had pinned the scheduler at capacity on
	// zombie rows.
	sweepCtx, sweepCancel := context.WithTimeout(ctx, 2*time.Minute)
	resolved, err := discoverer.SweepStaleDownloads(sweepCtx, time.Now().UTC())
	sweepCancel()
	if err != nil {
		logger.Error("sweep stale downloads failed", "err", err)
	} else if resolved > 0 {
		logger.Info("swept stale downloading jobs", "resolved", resolved)
	}

	logger.Info("slskdarr started", "status_addr", cfg.Observ.ListenAddr)
	if err := eng.Run(ctx); err != nil {
		logger.Error("engine stopped", "err", err)
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
