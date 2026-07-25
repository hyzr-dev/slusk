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
	// throughputRecorderInterval is how often runThroughputRecorder drains
	// completed per-minute download-throughput rollups into the store (issue
	// #157). At least twice a minute, so soulseek.throughputPendingCap never
	// nears its cap under normal operation.
	throughputRecorderInterval = 30 * time.Second
)

// ensureWritableDir verifies dir exists (creating it if needed) and is actually
// writable by creating and removing a probe file — MkdirAll alone returns nil
// for an existing but unwritable dir. It gives the native soulseek backend a
// loud startup failure instead of a silent per-download "mkdir: permission
// denied" when paths.slskd_complete_dir is unmounted or owned by another user.
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".slskdarr-write-probe-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

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
	// restartCtx is a child of the signal context, so requesting a restart
	// (after a settings-view config update) cancels it exactly like a signal
	// would, driving the same graceful shutdown path (see runRuntime below).
	// Every derived context below uses restartCtx rather than ctx directly.
	restartCtx, requestRestart := context.WithCancel(ctx)
	defer requestRestart()
	startupCtx, cancelStartup := context.WithTimeout(restartCtx, startupTimeout)
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
		sink := &messageSink{store: st, logger: logger}
		soulClient = newSoulseekClient(cfg.Soulseek, cfg.Paths.SlskdCompleteDir, sink, logger)
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
		// The native backend writes completed downloads to this dir itself (the
		// slskd backend only read it), so fail fast with a clear message if it is
		// missing or not writable — otherwise every download dies with an opaque
		// per-transfer "mkdir ...: permission denied" and nothing ever completes.
		if err := ensureWritableDir(cfg.Paths.SlskdCompleteDir); err != nil {
			logger.Error("paths.slskd_complete_dir is not writable; the native soulseek backend downloads into it and needs it mounted writable by this user",
				"dir", cfg.Paths.SlskdCompleteDir, "err", err)
			os.Exit(1)
		}
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
		Metrics:       metrics,
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
		orphaned, err := st.CountJobsInStates(ctx, core.StateOrphaned)
		if err != nil {
			return observ.StatusReport{}, err
		}
		return observ.StatusReport{Active: len(active), Orphaned: orphaned}, nil
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
	chartsFn := func(ctx context.Context) (observ.ChartsData, error) {
		passes, err := st.RecentSearchPasses(ctx, observ.ChartsRecentPasses)
		if err != nil {
			return observ.ChartsData{}, err
		}
		// Aligned to the 24 whole-hour buckets the Overview chart displays
		// (see observ.toChartsDTO): starting mid-hour would query a partial
		// first hour that the zero-fill then silently discards.
		since := time.Now().UTC().Truncate(time.Hour).Add(-23 * time.Hour)
		counts, err := st.CompletedByHour(ctx, since)
		if err != nil {
			return observ.ChartsData{}, err
		}
		return observ.ChartsData{Passes: passes, CompletedByHour: counts}, nil
	}
	peersFn := func(ctx context.Context) ([]core.PeerRow, error) {
		return st.Peers(ctx)
	}
	jobs := &app.Jobs{Store: st, Peers: peers, Logger: logger.With("component", "app")}
	// createJobFn converts observ's core.CandidateFile request shape into
	// store.ManualJobFile: observ deliberately does not import internal/store,
	// so the conversion happens here at the wiring boundary instead.
	createJobFn := func(ctx context.Context, title, artist, peer string, files []core.CandidateFile) (core.JobView, error) {
		manualFiles := make([]store.ManualJobFile, len(files))
		for i, f := range files {
			manualFiles[i] = store.ManualJobFile{Filename: f.Filename, Size: f.Size}
		}
		return jobs.Create(ctx, title, artist, peer, manualFiles)
	}
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
	// a single fixed value instead of re-reading the file. Writable is probed
	// once here too: a settings update always restarts the process (see
	// configWriter/restartFn below), so this can never go stale mid-process.
	writable := config.ProbeWritable(*configPath)
	appConfig := buildAppConfig(cfg, writable)
	configFn := func() observ.AppConfig { return appConfig }
	// configWriter converts a validated settings-view update into config.Settings
	// and applies it to the on-disk file; ErrNotWritable and ErrValidationFailed
	// are remapped to observ's own sentinels so observ can report the right
	// status code (409, 422) without importing internal/config.
	// restartFn schedules a graceful shutdown (see restartCtx above) so the
	// container's restart policy brings the process back up with the new config.
	configWriter := func(u observ.ConfigUpdate) error {
		err := config.ApplySettings(*configPath, settingsFromConfigUpdate(u))
		switch {
		case errors.Is(err, config.ErrNotWritable):
			return observ.ErrConfigNotWritable
		case errors.Is(err, config.ErrValidationFailed):
			return &observ.ConfigValidationError{Message: err.Error()}
		default:
			return err
		}
	}
	restartFn := func() {
		logger.Info("restarting to apply configuration change")
		requestRestart()
	}
	// Live queue-position/speed on the job detail page comes from the native
	// backend's in-memory ListDownloads snapshot. The slskd backend leaves those
	// two fields zero, so there we skip the call entirely rather than pay a slskd
	// round-trip per detail poll for data that would only be zeros.
	liveTransfersFn := func(ctx context.Context) ([]core.RemoteTransfer, error) { return nil, nil }
	if cfg.Pipeline.Backend == config.BackendSoulseek {
		liveTransfersFn = func(ctx context.Context) ([]core.RemoteTransfer, error) {
			return peers.ListDownloads(ctx)
		}
	}
	// throughputFn backs the Overview view's live sparkline (issue #157): only
	// the native soulseek client tracks byte throughput, so this stays nil
	// (registerCharts then serves an empty series) on every other backend,
	// mirroring liveTransfersFn's own backend gate above.
	var throughputFn observ.ThroughputFunc
	if cfg.Pipeline.Backend == config.BackendSoulseek {
		throughputFn = func(ctx context.Context) ([]core.ThroughputSample, error) {
			return soulClient.ThroughputSamples(), nil
		}
	}
	// Connection tests for the settings view probe the loaded config, not any
	// request payload. Lidarr is always configured; the Soulseek probe reports
	// the current login state (a passive read of the background Run loop, not an
	// active re-login) and is left nil when the native client is disabled so its
	// endpoint answers "not enabled" rather than a misleading failure.
	connectionTester := observ.ConnectionTester{
		Lidarr: func(ctx context.Context) error { return lidarrClient.Ping(ctx) },
	}
	if soulClient != nil {
		connectionTester.Soulseek = func(ctx context.Context) error {
			st := soulClient.Status()
			switch st.State {
			case soulseek.StateConnected:
				return nil
			case soulseek.StateFailed:
				if st.LastError != "" {
					return fmt.Errorf("soulseek login failed: %s", st.LastError)
				}
				return fmt.Errorf("soulseek login failed")
			default:
				if st.LastError != "" {
					return fmt.Errorf("soulseek not connected (%s): %s", st.State, st.LastError)
				}
				return fmt.Errorf("soulseek not connected (%s)", st.State)
			}
		}
	}
	// The native Soulseek client shares files whenever it runs, independent of
	// which backend downloads (cfg.Pipeline.Backend); keyed on soulClient !=
	// nil, not the backend, so a slskd-download+native-share configuration
	// still gets working share stats/rescan endpoints.
	var sharesFn observ.SharesFunc
	var rescanSharesFn observ.RescanSharesFunc
	if soulClient != nil {
		sharesFn = func() observ.ShareStatsReport {
			report := soulClient.ShareReport()
			folders := make([]observ.ShareFolderStats, len(report.Folders))
			for i, f := range report.Folders {
				folders[i] = observ.ShareFolderStats{
					Name:        f.Name,
					Path:        f.Path,
					Directories: f.Directories,
					Files:       f.Files,
					TotalBytes:  f.TotalBytes,
				}
			}
			return observ.ShareStatsReport{
				Directories:  report.Directories,
				Files:        report.Files,
				TotalBytes:   report.TotalBytes,
				IndexedAt:    report.IndexedAt,
				ScanDuration: report.ScanDuration,
				Scanning:     report.Scanning,
				Folders:      folders,
			}
		}
		rescanSharesFn = func() error {
			err := soulClient.TriggerRescanShares()
			switch {
			case errors.Is(err, soulseek.ErrShareScanInProgress):
				return observ.ErrShareScanInProgress
			default:
				return err
			}
		}
	}
	// Conversations/Thread stay wired even when soulClient is nil, so message
	// history remains readable with no Soulseek backend configured — only
	// sending requires a live client.
	conversationsFn := st.Conversations
	threadFn := st.Thread
	markReadFn := func(ctx context.Context, username string) (int, error) {
		n, err := st.MarkConversationRead(ctx, username, time.Now())
		return int(n), err
	}
	var sendMessageFn observ.SendMessageFunc
	if soulClient != nil {
		// Send-then-persist, deliberately in that order: only record a message
		// once it has actually gone out. The residual failure mode (send
		// succeeds, RecordOutgoingMessage fails) surfaces as a 502 for a
		// message that WAS delivered - worse would be the reverse ordering,
		// which risks a "sent" row in the store for a message that never left.
		// There is no pending/retry state by design (see issue #183).
		sendMessageFn = func(ctx context.Context, username, body string) (core.PrivateMessage, error) {
			if err := soulClient.SendPrivateMessage(ctx, username, body); err != nil {
				return core.PrivateMessage{}, err
			}
			return st.RecordOutgoingMessage(ctx, username, body, time.Now())
		}
	}
	handler := observ.NewServer(observ.ServerDeps{
		Registry:         reg,
		Status:           statusFn,
		Jobs:             jobsFn,
		Cancel:           jobs.Cancel,
		Retry:            jobs.Retry,
		SearchJob:        jobs.ForceSearch,
		DeleteJob:        jobs.Delete,
		CreateJob:        createJobFn,
		JobDetail:        jobDetailFn,
		JobEvents:        jobEventsFn,
		RecentEvents:     recentEventsFn,
		Peers:            peersFn,
		Live:             liveFn,
		Ready:            readyFn,
		Modules:          modulesFn,
		FailedRetryAfter: cfg.Pipeline.FailedReviveAfter.Duration,
		MaxCandidates:    cfg.Pipeline.MaxCandidatesPerAlbum,
		Config:           configFn,
		ConfigWriter:     configWriter,
		Restart:          restartFn,
		ConnectionTester: connectionTester,
		LiveTransfers:    liveTransfersFn,
		Charts:           chartsFn,
		Shares:           sharesFn,
		RescanShares:     rescanSharesFn,
		Throughput:       throughputFn,
		Conversations:    conversationsFn,
		Thread:           threadFn,
		Send:             sendMessageFn,
		MarkRead:         markReadFn,
	})
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
	var throughputDone chan struct{}
	soulCtx, soulCancel := context.WithCancel(restartCtx)
	defer soulCancel()
	if soulClient != nil {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)
		go runShareRescanLoop(soulCtx, hup, soulClient, logger)
		if cfg.Pipeline.Backend == config.BackendSoulseek {
			// Deliberately not a pipeline.Runner module: Runner modules feed
			// Live()/Ready(), and a failed throughput write must never make the
			// daemon unready.
			throughputDone = make(chan struct{})
			go func() {
				runThroughputRecorder(soulCtx, soulClient, st, throughputRecorderInterval, logger)
				close(throughputDone)
			}()
		}
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
	outcome := runRuntime(restartCtx, srv, listener, runner, lifecycleShutdownTimeout)
	// runRuntime may return on an abnormal exit (runner or HTTP server
	// stopping without a signal) while restartCtx is still live, which would
	// otherwise leave the soulseek client reconnecting indefinitely and
	// force this join to burn the full shutdown timeout. Cancel its
	// context explicitly so the join below is prompt in that case too.
	//
	// Both soulDone and throughputDone MUST be joined here, before
	// closeStoreAfterRuntime below: the throughput recorder's shutdown flush
	// (see runThroughputRecorder's ctx.Done branch) writes to st, so closing
	// st before that flush completes races it — the write either hits a
	// closed DB or the final partial minute is silently dropped (issue #157
	// F1).
	shutdownSoulseek(logger, soulCancel, soulDone, throughputDone, lifecycleShutdownTimeout)
	closeErr := closeStoreAfterRuntime(outcome, st.Close)
	if outcome.err != nil || closeErr != nil {
		logger.Error("slskdarr stopped with error", "runtime_err", outcome.err, "close_store_err", closeErr,
			"store_close_safe", outcome.storeCloseSafe)
		os.Exit(1)
	}
	logger.Info("slskdarr stopped cleanly")
}

// buildAppConfig renders the settings view's read model from the loaded
// config. Every ...Configured boolean is computed here, once, from the real
// secret value; none of those values are ever copied into the observ.AppConfig
// this returns, which is the mechanism (not a convention) that keeps secrets
// out of the settings view's marshalled JSON.
func buildAppConfig(cfg config.Config, writable bool) observ.AppConfig {
	sharedFolders := make([]observ.SharedFolderView, len(cfg.Soulseek.SharedFolders))
	for i, f := range cfg.Soulseek.SharedFolders {
		sharedFolders[i] = observ.SharedFolderView{Name: f.Name, Path: f.Path}
	}
	return observ.AppConfig{
		Lidarr: observ.LidarrView{
			URL:              cfg.Lidarr.URL,
			APIKeyConfigured: cfg.Lidarr.APIKey != "",
		},
		Slskd: observ.SlskdView{
			URL:              cfg.Slskd.URL,
			APIKeyConfigured: cfg.Slskd.APIKey != "",
		},
		Pipeline: observ.PipelineView{
			Backend:               cfg.Pipeline.Backend,
			MaxCandidatesPerAlbum: cfg.Pipeline.MaxCandidatesPerAlbum,
			MaxActive:             cfg.Pipeline.MaxActive,
			MaxRetries:            cfg.Pipeline.MaxRetries,
			MaxInflightPerPeer:    cfg.Pipeline.MaxInflightPerPeer,
			MaxTransferRetries:    cfg.Pipeline.MaxTransferRetries,
			MinBitrate:            cfg.Pipeline.MinBitrate,
			TransferDeadline:      cfg.Pipeline.TransferDeadline.Duration.String(),
			StallTimeout:          cfg.Pipeline.StallTimeout.Duration.String(),
			SearchTimeout:         cfg.Pipeline.SearchTimeout.Duration.String(),
			BackoffBase:           cfg.Pipeline.BackoffBase.Duration.String(),
			BackoffCap:            cfg.Pipeline.BackoffCap.Duration.String(),
			CandidateTTL:          cfg.Pipeline.CandidateTTL.Duration.String(),
			FailedReviveAfter:     cfg.Pipeline.FailedReviveAfter.Duration.String(),
			StuckAfter:            cfg.Pipeline.StuckAfter.Duration.String(),
			TickTimeout:           cfg.Pipeline.TickTimeout.Duration.String(),
			ImportConfirmTimeout:  cfg.Pipeline.ImportConfirmTimeout.Duration.String(),
			WantedSyncInterval:    cfg.Pipeline.WantedSyncInterval.Duration.String(),
			DiscoveryInterval:     cfg.Pipeline.DiscoveryInterval.Duration.String(),
			SelectingInterval:     cfg.Pipeline.SelectingInterval.Duration.String(),
			DownloadingInterval:   cfg.Pipeline.DownloadingInterval.Duration.String(),
			ImportingInterval:     cfg.Pipeline.ImportingInterval.Duration.String(),
			ManualImportTimeout:   cfg.Pipeline.ManualImportTimeout.Duration.String(),
			ImportRetryCooldown:   cfg.Pipeline.ImportRetryCooldown.Duration.String(),
			Weights: observ.WeightsView{
				Format:      cfg.Pipeline.Weights.Format,
				Bitrate:     cfg.Pipeline.Weights.Bitrate,
				Reliability: cfg.Pipeline.Weights.Reliability,
				FileCount:   cfg.Pipeline.Weights.FileCount,
				KnownUser:   cfg.Pipeline.Weights.KnownUser,
			},
		},
		Soulseek: observ.SoulseekView{
			Enabled:                   cfg.Soulseek.Enabled(),
			ServerAddress:             cfg.Soulseek.ServerAddress,
			Username:                  cfg.Soulseek.Username,
			PasswordConfigured:        cfg.Soulseek.Password != "",
			ListenAddr:                cfg.Soulseek.ListenAddr,
			UploadSlots:               cfg.Soulseek.UploadSlots,
			AllowPrivatePeerAddresses: cfg.Soulseek.AllowPrivatePeerAddresses,
			Gluetun: observ.GluetunView{
				ControlURL:       cfg.Soulseek.Gluetun.ControlURL,
				APIKeyConfigured: cfg.Soulseek.Gluetun.APIKey != "",
			},
			SharedFolders: sharedFolders,
		},
		Store: observ.StoreView{DSNConfigured: cfg.Store.DSN != ""},
		Observ: observ.ObservView{
			ListenAddr:          cfg.Observ.ListenAddr,
			AuthTokenConfigured: cfg.Observ.AuthToken != "",
			LogLevel:            cfg.Observ.LogLevel,
		},
		Paths:    observ.PathsView{SlskdCompleteDir: cfg.Paths.SlskdCompleteDir},
		Writable: writable,
	}
}

// settingsFromConfigUpdate converts a validated settings-view update into the
// shape internal/config.ApplySettings understands. observ deliberately does
// not import internal/config (see internal/observ/config.go's package
// comment), so this trivial field-by-field mapping lives here instead.
func settingsFromConfigUpdate(u observ.ConfigUpdate) config.Settings {
	sharedFolders := make([]config.SharedFolderConfig, len(u.Soulseek.SharedFolders))
	for i, f := range u.Soulseek.SharedFolders {
		sharedFolders[i] = config.SharedFolderConfig{Name: f.Name, Path: f.Path}
	}
	return config.Settings{
		Lidarr: config.LidarrSettings{URL: u.Lidarr.URL, APIKey: u.Lidarr.APIKey},
		Slskd:  config.SlskdSettings{URL: u.Slskd.URL, APIKey: u.Slskd.APIKey},
		Pipeline: config.PipelineSettings{
			Backend:               u.Pipeline.Backend,
			MaxCandidatesPerAlbum: u.Pipeline.MaxCandidatesPerAlbum,
			MaxActive:             u.Pipeline.MaxActive,
			MaxRetries:            u.Pipeline.MaxRetries,
			MaxInflightPerPeer:    u.Pipeline.MaxInflightPerPeer,
			MaxTransferRetries:    u.Pipeline.MaxTransferRetries,
			MinBitrate:            u.Pipeline.MinBitrate,
			TransferDeadline:      u.Pipeline.TransferDeadline,
			StallTimeout:          u.Pipeline.StallTimeout,
			SearchTimeout:         u.Pipeline.SearchTimeout,
			BackoffBase:           u.Pipeline.BackoffBase,
			BackoffCap:            u.Pipeline.BackoffCap,
			CandidateTTL:          u.Pipeline.CandidateTTL,
			FailedReviveAfter:     u.Pipeline.FailedReviveAfter,
			StuckAfter:            u.Pipeline.StuckAfter,
			TickTimeout:           u.Pipeline.TickTimeout,
			ImportConfirmTimeout:  u.Pipeline.ImportConfirmTimeout,
			WantedSyncInterval:    u.Pipeline.WantedSyncInterval,
			DiscoveryInterval:     u.Pipeline.DiscoveryInterval,
			SelectingInterval:     u.Pipeline.SelectingInterval,
			DownloadingInterval:   u.Pipeline.DownloadingInterval,
			ImportingInterval:     u.Pipeline.ImportingInterval,
			ManualImportTimeout:   u.Pipeline.ManualImportTimeout,
			ImportRetryCooldown:   u.Pipeline.ImportRetryCooldown,
			Weights: config.WeightsSettings{
				Format:      u.Pipeline.Weights.Format,
				Bitrate:     u.Pipeline.Weights.Bitrate,
				Reliability: u.Pipeline.Weights.Reliability,
				FileCount:   u.Pipeline.Weights.FileCount,
				KnownUser:   u.Pipeline.Weights.KnownUser,
			},
		},
		Soulseek: config.SoulseekSettings{
			ServerAddress:             u.Soulseek.ServerAddress,
			Username:                  u.Soulseek.Username,
			Password:                  u.Soulseek.Password,
			ListenAddr:                u.Soulseek.ListenAddr,
			UploadSlots:               u.Soulseek.UploadSlots,
			AllowPrivatePeerAddresses: u.Soulseek.AllowPrivatePeerAddresses,
			Gluetun: config.GluetunSettings{
				ControlURL: u.Soulseek.Gluetun.ControlURL,
				APIKey:     u.Soulseek.Gluetun.APIKey,
			},
			SharedFolders: sharedFolders,
		},
		Store: config.StoreSettings{DSN: u.Store.DSN},
		Observ: config.ObservSettings{
			ListenAddr: u.Observ.ListenAddr,
			AuthToken:  u.Observ.AuthToken,
			LogLevel:   u.Observ.LogLevel,
		},
		Paths: config.PathsSettings{SlskdCompleteDir: u.Paths.SlskdCompleteDir},
	}
}

func listenHTTP(ctx context.Context, addr string) (net.Listener, error) {
	return (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
}

type runtimeRunner interface {
	Run(context.Context) error
}

// shutdownSoulseek cancels the soulseek client's context and waits for both
// the soulseek connection goroutine (soulDone) and the throughput recorder
// (throughputDone) to stop, each bounded by shutdownTimeout, before
// returning. Extracted from main's run body so the ordering it guarantees —
// both goroutines joined before the caller closes the store, see issue #157
// F1 — is independently testable. Either channel may be nil (soulClient ==
// nil, or the pipeline backend isn't soulseek), in which case that wait is
// skipped.
func shutdownSoulseek(logger *slog.Logger, soulCancel context.CancelFunc, soulDone chan error, throughputDone chan struct{}, shutdownTimeout time.Duration) {
	soulCancel()
	if soulDone != nil {
		select {
		case <-soulDone:
		case <-time.After(shutdownTimeout):
			logger.Error("soulseek connection did not stop within the shutdown timeout")
		}
	}
	if throughputDone != nil {
		select {
		case <-throughputDone:
		case <-time.After(shutdownTimeout):
			logger.Error("throughput recorder did not stop within the shutdown timeout")
		}
	}
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
