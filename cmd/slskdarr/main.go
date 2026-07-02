// Command slskdarr is the daemon entrypoint: it loads config, opens the store,
// wires the clients and engine, starts the observability server, and runs until
// it receives SIGINT/SIGTERM (graceful shutdown via context cancellation).
package main

import (
	"context"
	"flag"
	"log/slog"
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

func main() {
	configPath := flag.String("config", "/config/config.toml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	peers := slskd.New(cfg.Slskd.URL, cfg.Slskd.APIKey)
	reconciler := engine.NewReconciler(peers, st)
	lidarrClient := lidarr.New(cfg.Lidarr.URL, cfg.Lidarr.APIKey)
	scorer := matcher.NewWeighted(cfg.Engine.Weights, cfg.Engine.MinBitrate)
	discoverer := engine.NewDiscoverer(engine.DiscovererParams{
		Music: lidarrClient, Peers: peers, Store: st, Ranker: scorer,
		CompleteDir:      cfg.Paths.SlskdCompleteDir,
		SearchTimeout:    cfg.Engine.SearchTimeout.Duration,
		TransferDeadline: cfg.Engine.TransferDeadline.Duration,
		CandidateBackoff:       cfg.Engine.CandidateBackoff.Duration,
		FailedCandidateBackoff: cfg.Engine.FailedCandidateBackoff.Duration,
		FailedRetryAfter:       cfg.Engine.FailedRetryAfter.Duration,
		MaxCandidates:    cfg.Engine.MaxCandidatesPerAlbum,
		Batch:            cfg.Engine.MaxConcurrentSearches,
		MaxActive:        cfg.Engine.MaxConcurrentActive,
		Logger:           logger,
	})
	reg := prometheus.NewRegistry()
	metrics := observ.NewMetrics(reg)
	eng := engine.New(engine.Params{
		Reconciler: reconciler,
		Discoverer: discoverer,
		StatusPoll: cfg.Slskd.StatusPollInterval.Duration,
		LidarrPoll: cfg.Lidarr.PollInterval.Duration,
		Logger:     logger,
		Metrics:    metrics,
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
	srv := &http.Server{Addr: cfg.Observ.ListenAddr, Handler: observ.NewServer(reg, statusFn, jobsFn, cancelFn, cfg.Engine.FailedRetryAfter.Duration, cfg.Engine.MaxCandidatesPerAlbum)}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("status server", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("slskdarr started", "status_addr", cfg.Observ.ListenAddr)
	if err := eng.Run(ctx); err != nil {
		logger.Error("engine stopped", "err", err)
	}
	_ = srv.Shutdown(context.Background())
	logger.Info("slskdarr stopped cleanly")
}
