// Package soulseek owns the connection lifecycle to the central Soulseek
// server: dialing, logging in, keeping the connection alive with periodic
// pings, and reconnecting with exponential backoff after a transient
// failure. It is built on top of the vendored protocol message layer in
// internal/soulseek/soul.
package soulseek

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul"
	"github.com/samuelenocsson/slusk/internal/soulseek/soul/server"
)

const (
	defaultDialTimeout  = 10 * time.Second
	defaultPingInterval = 5 * time.Minute
	// defaultServerWriteTimeout bounds a single write to the central server so a
	// stalled (e.g. zero-window) server cannot pin serverWriteMu, and the ping
	// ticker's own write cannot wedge the serveConnected select that must reach
	// ctx.Done(). Server control messages are tiny, so a healthy link never
	// approaches this; it only trips on a genuinely wedged connection.
	defaultServerWriteTimeout       = 30 * time.Second
	defaultBackoffBase              = 5 * time.Second
	defaultBackoffCap               = 10 * time.Minute
	tcpKeepAliveInterval            = time.Minute
	defaultPeerInitTimeout          = 10 * time.Second
	defaultPeerDialTimeout          = 10 * time.Second
	defaultEstablishTimeout         = 30 * time.Second
	defaultFileIdleTimeout          = 60 * time.Second
	defaultUploadNegotiationTimeout = 60 * time.Second
	// defaultPlaceInQueueInterval is how often runDownload polls a peer for a
	// queued download's current queue position while waiting for its
	// TransferRequest.
	defaultPlaceInQueueInterval = 60 * time.Second
	// defaultDownloadNegotiationTimeout bounds how long runDownload waits for
	// a TransferRequest's matching F connection to arrive once
	// TransferResponse has been sent.
	defaultDownloadNegotiationTimeout = 60 * time.Second
	// defaultDownloadQueueTimeout bounds how long a queued download waits for
	// the peer to send its TransferRequest before giving up.
	defaultDownloadQueueTimeout = 10 * time.Minute
	// defaultListenAddr is used only when Config.ListenAddr is left blank,
	// which production configuration never does (see config.SoulseekConfig);
	// it exists so tests that don't care about the peer listener don't have
	// to set one.
	defaultListenAddr = "127.0.0.1:0"
	// defaultGluetunTimeout bounds one gluetun control-server fetch.
	defaultGluetunTimeout = 5 * time.Second
	// defaultShareScanLogInterval is how often scanShares logs progress
	// during a share rescan.
	defaultShareScanLogInterval = 30 * time.Second
	// defaultShareMetaCacheTimeout bounds a single ShareMetaCache Load/Save
	// call (issue #197).
	defaultShareMetaCacheTimeout = 60 * time.Second
	// defaultUploadMinThroughput is the minimum sustained upload throughput,
	// in bytes/second, an upload slot's peer must maintain (see #108).
	defaultUploadMinThroughput = 1024
	// defaultUploadThroughputSampleInterval is how often an in-flight
	// upload's cumulative throughput is sampled against
	// uploadMinThroughput (see #108).
	defaultUploadThroughputSampleInterval = 15 * time.Second
	// defaultThroughputInterval is how often sampleThroughput records aligned
	// native download and upload throughput samples (see throughput.go).
	defaultThroughputInterval = time.Second
)

// errRelogged is returned by Run when the server reports that the account
// logged in from elsewhere (a Relogged message), which the Soulseek
// protocol uses to kick the previous connection. This is terminal: Run does
// not reconnect after it.
var errRelogged = errors.New("soulseek: account logged in elsewhere (relogged)")

// errNoServerConnection fails any in-flight server round-trip (currently:
// GetPeerAddress waiters and pending peer-connection dances, see peers.go)
// when the server connection is torn down while they were waiting on it.
var errNoServerConnection = errors.New("soulseek: server connection lost")

// Config configures a Client. Address, Username and Password are required;
// the remaining fields are internal test seams with production defaults
// applied by New.
type Config struct {
	// Address is the Soulseek server's host:port, e.g. "server.slsknet.org:2242".
	Address string
	// Username and Password authenticate the login handshake.
	Username string
	Password string

	// ListenAddr is the host:port this Client listens on for incoming peer
	// connections, advertised to the server via SetListenPort after login.
	// Defaults to "127.0.0.1:0" (a random loopback port) when blank, which
	// is only appropriate for tests: production configuration always
	// supplies a real, routable ListenAddr (see config.SoulseekConfig).
	ListenAddr string

	// GluetunControlURL, when nonempty, makes trySetup fetch the forwarded
	// port from {GluetunControlURL}/v1/portforward and substitute it for the
	// port in ListenAddr before binding the peer listener.
	GluetunControlURL string
	// GluetunAPIKey, when nonempty, is sent as X-API-Key on the gluetun
	// control-server request.
	GluetunAPIKey string

	// DownloadDir is the local root directory native downloads (issue #55)
	// are written under, in the same completeDir/<leaf>/<basename> layout
	// slskd produces (see downloadDestPath) so the Importing module's
	// AlbumFolder scan finds them in the same place either way. Production
	// wiring (issue #57) sets it from config.PathsConfig.SlskdCompleteDir;
	// tests and the manual probe set it directly.
	DownloadDir string

	// SharedFolders are explicitly named public roots backed by private local
	// directories. UploadSlots defaults to 2.
	SharedFolders []SharedFolder
	UploadSlots   int

	// MessageSink, when non-nil, receives incoming private messages (issue #183). Left nil,
	// incoming messages are logged and deliberately NOT acked, so the server keeps them and
	// redelivers at the next login rather than the client silently destroying mail it has
	// nowhere to put.
	MessageSink MessageSink

	// UploadSink, when non-nil, receives every finished upload so the history survives a
	// restart (issue #325). Left nil, uploads are served exactly as before and simply
	// leave no trace once the process exits.
	UploadSink UploadSink

	// ShareMetaCache, when non-nil, lets scanShares skip reopening an
	// unchanged audio file's technical metadata across restarts (issue #197).
	// Left nil, every mp3/flac is read on every scan, matching the client's
	// behavior before this cache existed.
	ShareMetaCache ShareMetaCache

	// AllowPrivatePeerAddresses permits dialing server-supplied peer
	// addresses in RFC 1918 / ULA private ranges (threat T12: the central
	// server supplies raw IP:port for peers, which is otherwise untrusted
	// input). Loopback and link-local addresses are always refused
	// regardless of this flag. Defaults to false (private addresses
	// blocked); set true to reach peers on your own LAN.
	AllowPrivatePeerAddresses bool

	// allowLoopbackPeerDial carves out loopback addresses from the T12 guard
	// (see validateDialAddr in addrguard.go). Only this package's own test
	// helpers set it, since the test suite necessarily runs its fake peers
	// on 127.0.0.1; production configuration never sets it.
	allowLoopbackPeerDial bool

	// dialTimeout bounds establishing the TCP connection. Default 10s.
	dialTimeout time.Duration
	// pingInterval is how often a keepalive Ping is sent once connected.
	// Default 5m.
	pingInterval time.Duration
	// backoffBase and backoffCap bound the exponential reconnect backoff.
	// Defaults 5s and 10m.
	backoffBase time.Duration
	backoffCap  time.Duration
	// peerInitTimeout bounds how long an accepted peer connection has to
	// send its first (PeerInit or PierceFirewall) frame. Default 10s.
	peerInitTimeout time.Duration
	// fileIdleTimeout bounds how long an F (file transfer) connection may go
	// without any data arriving before it is treated as stalled. It resets
	// on every read (see progressReader in fileconn.go) rather than bounding
	// the transfer's total duration, so a slow but steady peer sending a
	// large file is never cut off - only silence trips it. Default 60s.
	fileIdleTimeout time.Duration
	// fileInitTimeout bounds how long an accepted or mirror-dialed F
	// connection has to send its TransferInit frame. Default equals
	// peerInitTimeout (10s).
	fileInitTimeout time.Duration
	// uploadMinThroughput is the minimum sustained throughput, in
	// bytes/second, a peer must maintain while draining an upload slot's F
	// connection; a slower peer has its connection closed so it can no
	// longer occupy the slot indefinitely (slow-loris DoS, see #108).
	// Default 1024 B/s.
	uploadMinThroughput int
	// uploadThroughputSampleInterval is how often an in-flight upload's
	// cumulative throughput is sampled against uploadMinThroughput (#108).
	// Default 15s.
	uploadThroughputSampleInterval time.Duration
	// peerDialTimeout bounds a single outbound peer TCP dial attempt (both
	// the direct path in ConnectPeer and the mirror dial-back in
	// handleConnectToPeer). Default 10s.
	peerDialTimeout time.Duration
	// establishTimeout bounds the whole of ConnectPeer: resolving the peer's
	// address, the direct dial attempt, and - if that fails - the indirect
	// NAT-traversal fallback. Default 30s.
	establishTimeout time.Duration
	// inboundPeerLimit caps accepted sockets globally, including handshakes
	// and retained inbound sessions. Default 128.
	inboundPeerLimit int
	// sessionWriteQueue bounds serialized writes per internal session.
	sessionWriteQueue int
	// peerIdleTimeout retires ordinary retained P sessions. Default 2m.
	peerIdleTimeout time.Duration
	// inboundPeerSessionLifetime is an absolute lifetime for retained inbound
	// ordinary P sessions, even while they remain active. Default 10m.
	inboundPeerSessionLifetime time.Duration
	// parentCandidateTimeout bounds one direct D candidate's opportunity to
	// provide valid metadata and a search. Default 10s.
	parentCandidateTimeout time.Duration
	// placeInQueueInterval is how often runDownload polls a peer for a
	// queued download's current queue position while waiting for its
	// TransferRequest. Default 60s.
	placeInQueueInterval time.Duration
	// downloadNegotiationTimeout bounds how long runDownload waits for a
	// TransferRequest's matching F connection to arrive once
	// TransferResponse has been sent. Default 60s.
	downloadNegotiationTimeout time.Duration
	// downloadQueueTimeout bounds how long a queued download waits for the
	// peer to send its TransferRequest before giving up. Default 10m.
	downloadQueueTimeout time.Duration
	// uploadNegotiationTimeout bounds waiting for TransferResponse.
	uploadNegotiationTimeout time.Duration
	// serverWriteTimeout bounds a single write to the central server connection
	// so a stalled server cannot pin serverWriteMu (and the ping ticker's write
	// cannot wedge shutdown) forever. Default 30s.
	serverWriteTimeout time.Duration
	// gluetunTimeout bounds one control-server fetch so a hung gluetun cannot
	// stall the startup backoff rhythm. Default 5s.
	gluetunTimeout time.Duration
	// shareScanLogInterval is how often scan progress is logged during a
	// share rescan. Default 30s; test seam.
	shareScanLogInterval time.Duration
	// shareMetaCacheTimeout bounds a single ShareMetaCache Load/Save call, so
	// a stalled store cannot hang a share scan indefinitely. Default 60s;
	// test seam.
	shareMetaCacheTimeout time.Duration
	// shareScanHook, when non-nil, is invoked at the start of every share
	// scan (see scanShares) instead of nothing, letting tests block or fail
	// the scan deterministically without touching the filesystem. Always nil
	// in production.
	shareScanHook func(context.Context) error
	// throughputInterval is how often sampleThroughput records aligned native
	// download and upload samples (see throughput.go). Default 1s; test seam.
	throughputInterval time.Duration
}

// Client manages one connection to the Soulseek server, reconnecting with
// backoff after transient failures. The zero value is not usable; construct
// with New.
type Client struct {
	cfg    Config
	logger *slog.Logger

	status atomic.Pointer[Status]

	// mu guards the current central-server session identity. serverWriteMu
	// serializes writes separately, so network I/O never occurs under mu.
	mu               sync.Mutex
	serverWriteMu    sync.Mutex
	serverConn       net.Conn
	serverCancel     context.CancelFunc
	serverGeneration uint64

	// presence owns the bounded, memory-only conversation watch set and the
	// statuses learned for the current server generation.
	presence *presenceTracker

	// addrMu guards pendingAddrs, the GetPeerAddress waiters registered by
	// in-flight ConnectPeer calls (see peers.go), keyed by username.
	addrMu       sync.Mutex
	pendingAddrs map[string][]chan addrResult

	// listenPort is the actual bound port of the peer listener started once
	// by Run (see listener.go), advertised to the server via SetListenPort.
	// It is written exactly once, before Run's reconnect loop starts, and
	// only ever read afterward (by the same goroutine chain, via
	// serveConnected), so it needs no synchronization.
	listenPort int
	// boundListenAddr holds the peer listener's bound address (host:port),
	// for Status(). Set once by Run.
	boundListenAddr atomic.Pointer[string]

	// pendingMu guards pending, the in-flight indirect (NAT-traversal)
	// ConnectPeer attempts (see peers.go), keyed by the token sent in the
	// server.ConnectToPeer request.
	pendingMu sync.Mutex
	pending   map[soul.Token]*pendingAttempt

	// peerConns counts currently-open public peer connections and private
	// sessions, for Status().
	peerConns atomic.Int64

	tokens       *tokenAllocator
	sessions     *sessionRegistry
	searches     *searchRegistry
	tree         *distributedTree
	sessionHooks sessionHooks
	inboundSlots chan struct{}

	// excludedPhrases holds the server's most recently pushed excluded-search-
	// phrase list (code 160, re-sent on every login): well-behaved peers never
	// answer a search whose terms cover one of these phrases, so Search checks
	// against it before writing to the wire (see search.go). Server-global and
	// re-pushed on every login, so it needs no server-generation coupling -
	// same style as the shares snapshot below.
	excludedPhrases atomic.Pointer[[]string]

	// incoming carries received private messages from readLoop to
	// runMessageWorker (see messages.go). It is deliberately not
	// generation-scoped: a persisted message outlives the connection it
	// arrived on, and each entry carries its own generation so a stale ack
	// is refused rather than misapplied to a newer session.
	incoming chan incomingMessage

	shares atomic.Pointer[shareSnapshot]
	// shareScanSem is the share-scan lock, as a capacity-1 semaphore rather
	// than a plain mutex: TriggerRescanShares needs to claim it without
	// blocking (tryAcquireShareScan) and ShareReport needs to read whether it
	// is currently held (len(shareScanSem) > 0), neither of which a
	// sync.Mutex supports.
	shareScanSem chan struct{}
	// announceMu serializes every SharedFoldersFiles announcement to the
	// server (login-time in serveConnected, the initial background scan,
	// SIGHUP rescans): announceShares reads the currently published snapshot
	// stats and sends them as one critical section under it, so the wire
	// order of announcements always matches publish order. It also guards
	// announcedGeneration/announcedStats, which double as the dedup state
	// (skip re-announcing directory/file counts the current server
	// generation has already been told - see announceCurrentShares for why
	// the comparison is by count, not the whole ShareStats value) and the
	// login gate (hold scan/rescan announcements back
	// until the generation's mandatory login-time announcement has gone
	// out; see announceCurrentShares). Lock ordering: announceMu ->
	// serverWriteMu -> mu; nothing acquires announceMu while holding either
	// of those.
	announceMu          sync.Mutex
	announcedGeneration uint64
	announcedStats      ShareStats
	// shareWorkers bounds CPU-bound share work (search match, folder-contents
	// build); deliverWorkers bounds the network-bound search-response delivery
	// that opens a session to the searcher. Separate pools so a slow delivery
	// never holds a match slot. See maxShareWorkers / maxDeliverWorkers.
	shareWorkers           chan struct{}
	deliverWorkers         chan struct{}
	searchDeliveryMu       sync.Mutex
	searchDeliveries       map[searchDeliveryKey]time.Time
	searchDeliveryInFlight map[searchDeliveryKey]struct{}
	uploads                *uploadManager

	// downloads is the in-memory registry of in-flight native downloads
	// (issue #55): this group (D) defines the type and the F-connection
	// handoff it feeds; Group E wires Enqueue/ListDownloads/Cancel/Remove and
	// the P-session download hooks on top without changing its shape.
	downloads *downloadRegistry

	// throughput aggregates aligned download and upload byte-throughput
	// samples; see throughput.go and the sampler started by Run.
	throughput *throughputMeter

	lifeMu       sync.Mutex
	lifeCtx      context.Context
	lifeCancel   context.CancelFunc
	lifeWG       sync.WaitGroup
	lifeActive   bool
	lifeStopping bool

	handshakeMu    sync.Mutex
	handshakeConns map[net.Conn]struct{}

	establishMu sync.Mutex
	establishes map[sessionKey]*sessionEstablishment
}

// New constructs a Client. Zero-valued test-seam fields in cfg are filled
// with production defaults.
func New(cfg Config, logger *slog.Logger) *Client {
	if cfg.dialTimeout <= 0 {
		cfg.dialTimeout = defaultDialTimeout
	}
	if cfg.pingInterval <= 0 {
		cfg.pingInterval = defaultPingInterval
	}
	if cfg.backoffBase <= 0 {
		cfg.backoffBase = defaultBackoffBase
	}
	if cfg.backoffCap <= 0 {
		cfg.backoffCap = defaultBackoffCap
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.peerInitTimeout <= 0 {
		cfg.peerInitTimeout = defaultPeerInitTimeout
	}
	if cfg.fileIdleTimeout <= 0 {
		cfg.fileIdleTimeout = defaultFileIdleTimeout
	}
	if cfg.fileInitTimeout <= 0 {
		cfg.fileInitTimeout = cfg.peerInitTimeout
	}
	if cfg.uploadMinThroughput <= 0 {
		cfg.uploadMinThroughput = defaultUploadMinThroughput
	}
	if cfg.uploadThroughputSampleInterval <= 0 {
		cfg.uploadThroughputSampleInterval = defaultUploadThroughputSampleInterval
	}
	if cfg.peerDialTimeout <= 0 {
		cfg.peerDialTimeout = defaultPeerDialTimeout
	}
	if cfg.establishTimeout <= 0 {
		cfg.establishTimeout = defaultEstablishTimeout
	}
	if cfg.inboundPeerLimit <= 0 {
		cfg.inboundPeerLimit = defaultInboundPeerLimit
	}
	if cfg.sessionWriteQueue <= 0 {
		cfg.sessionWriteQueue = defaultSessionWriteQueue
	}
	if cfg.peerIdleTimeout <= 0 {
		cfg.peerIdleTimeout = defaultPeerIdleTimeout
	}
	if cfg.inboundPeerSessionLifetime <= 0 {
		cfg.inboundPeerSessionLifetime = defaultInboundPeerSessionLifetime
	}
	if cfg.parentCandidateTimeout <= 0 {
		cfg.parentCandidateTimeout = defaultParentCandidateTimeout
	}
	if cfg.placeInQueueInterval <= 0 {
		cfg.placeInQueueInterval = defaultPlaceInQueueInterval
	}
	if cfg.downloadNegotiationTimeout <= 0 {
		cfg.downloadNegotiationTimeout = defaultDownloadNegotiationTimeout
	}
	if cfg.downloadQueueTimeout <= 0 {
		cfg.downloadQueueTimeout = defaultDownloadQueueTimeout
	}
	if cfg.uploadNegotiationTimeout <= 0 {
		cfg.uploadNegotiationTimeout = defaultUploadNegotiationTimeout
	}
	if cfg.serverWriteTimeout <= 0 {
		cfg.serverWriteTimeout = defaultServerWriteTimeout
	}
	if cfg.gluetunTimeout <= 0 {
		cfg.gluetunTimeout = defaultGluetunTimeout
	}
	if cfg.shareScanLogInterval <= 0 {
		cfg.shareScanLogInterval = defaultShareScanLogInterval
	}
	if cfg.shareMetaCacheTimeout <= 0 {
		cfg.shareMetaCacheTimeout = defaultShareMetaCacheTimeout
	}
	if cfg.UploadSlots <= 0 {
		cfg.UploadSlots = 2
	}
	if cfg.throughputInterval <= 0 {
		cfg.throughputInterval = defaultThroughputInterval
	}

	if logger != nil {
		logger = logger.With("component", "soulseek")
	}

	c := &Client{
		cfg:                    cfg,
		logger:                 logger,
		pendingAddrs:           make(map[string][]chan addrResult),
		pending:                make(map[soul.Token]*pendingAttempt),
		presence:               newPresenceTracker(),
		tokens:                 newTokenAllocator(),
		sessions:               newSessionRegistry(nil),
		searches:               newSearchRegistry(),
		inboundSlots:           make(chan struct{}, cfg.inboundPeerLimit),
		handshakeConns:         make(map[net.Conn]struct{}),
		establishes:            make(map[sessionKey]*sessionEstablishment),
		downloads:              newDownloadRegistry(),
		throughput:             newThroughputMeter(),
		shareWorkers:           make(chan struct{}, maxShareWorkers),
		deliverWorkers:         make(chan struct{}, maxDeliverWorkers),
		searchDeliveries:       make(map[searchDeliveryKey]time.Time),
		searchDeliveryInFlight: make(map[searchDeliveryKey]struct{}),
		shareScanSem:           make(chan struct{}, 1),
		incoming:               make(chan incomingMessage, incomingMessageQueue),
	}
	c.shares.Store(emptyShareSnapshot())
	c.uploads = newUploadManager(c, cfg.UploadSlots)
	c.tree = newDistributedTree(c)
	c.tree.onSearch = c.handleDistributedShareSearch
	c.sessionHooks = composedSessionHooks{
		c.tree,
		&searchSessionHooks{searches: c.searches},
		&downloadSessionHooks{downloads: c.downloads, logger: logger},
		&sharingSessionHooks{c: c},
		&uploadSessionHooks{c: c, uploads: c.uploads},
	}
	c.status.Store(&Status{State: StateDisconnected})
	return c
}

// serverMessage constrains sendToServer to the handful of server message
// types this package actually sends, rather than reusing server package's
// own (unexported) message[M] constraint.
type serverMessage[M any] interface {
	*server.Ping | *server.SetListenPort | *server.ConnectToPeer | *server.CantConnectToPeer | *server.GetPeerAddress | *server.GetUserStats |
		*server.HaveNoParent | *server.AcceptChildren | *server.BranchLevel | *server.BranchRoot | *server.FileSearch | *server.SharedFoldersFiles |
		*server.MessageUser | *server.MessageAcked | *server.WatchUser | *server.UnwatchUser
	Serialize(M) ([]byte, error)
}

// writeServerLocked writes msg to the server connection, bounding it with the
// standard write deadline. The caller MUST hold serverWriteMu. Every server
// writer goes through here so the deadline is applied consistently: net.Conn
// write deadlines are absolute and outlive the write, so a writer that skipped
// setting one would inherit a prior writer's now-expired deadline and fail its
// own write immediately with i/o timeout (Search is the other direct writer).
// serverWriteTimeout bounds a stalled (e.g. zero-window) server so it cannot pin
// serverWriteMu - and, for the ping ticker's write, wedge shutdown - forever.
// Server messages are tiny, so a healthy link never approaches the deadline; a
// timeout surfaces as a write error the caller tears the connection down on.
func writeServerLocked[M serverMessage[M]](c *Client, conn net.Conn, msg M) error {
	if c.cfg.serverWriteTimeout > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(c.cfg.serverWriteTimeout)); err != nil {
			return err
		}
	}
	_, err := server.Write(conn, msg)
	return err
}

// sendToServer serializes writes to the server connection behind c.mu, since
// multiple goroutines (the ping ticker, peer-connection establishment) may
// write concurrently. It reports an error if no server connection is
// currently established.
func sendToServer[M serverMessage[M]](c *Client, msg M) error {
	return sendToServerGeneration(c, 0, msg)
}

// sendToServerGeneration additionally rejects a stale server-originated
// worker when generation is non-zero. A write error cancels and closes that
// exact server session so its work cannot survive into a replacement.
func sendToServerGeneration[M serverMessage[M]](c *Client, generation uint64, msg M) error {
	c.serverWriteMu.Lock()
	c.mu.Lock()
	if c.serverConn == nil || (generation != 0 && generation != c.serverGeneration) {
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		return errors.New("soulseek: not connected to requested server generation")
	}
	conn := c.serverConn
	cancel := c.serverCancel
	c.mu.Unlock()

	err := writeServerLocked(c, conn, msg)
	c.serverWriteMu.Unlock()
	if err != nil {
		if cancel != nil {
			cancel()
		}
		_ = conn.Close()
	}
	return err
}

// Status returns a point-in-time snapshot of the client's connection state.
// PeerConns and ListenAddr are composed in here from their own atomics
// rather than stored in the Status snapshots written by record*, so a
// concurrent peer-connection open/close never races a read-modify-write
// against the connection-lifecycle state.
func (c *Client) Status() Status {
	s := *c.status.Load()
	s.PeerConns = int(c.peerConns.Load())
	if addr := c.boundListenAddr.Load(); addr != nil {
		s.ListenAddr = *addr
	}
	return s
}

// Run performs one-time startup (the peer-listener bind, after resolving the
// gluetun forwarded port when configured), then dials the server, logs in,
// and serves the connection until ctx is cancelled or a terminal error
// occurs. Startup no longer includes the initial share scan: it now runs
// concurrently in the background (see runInitialShareScan), and the client
// answers browse/search requests with an empty share list until it
// completes. Both startup and the server connection are resilient to
// transient failures: a transiently-held listen port (bind) or a dropped
// server connection are retried with exponential backoff rather than
// stopping soulseek for the life of the process. The peer listener, once
// bound, lives across every server reconnect. Run returns nil when ctx is
// cancelled; it returns a non-nil error only for a terminal server failure
// (invalid credentials, outdated protocol version, or the account logging in
// elsewhere), after which it never reconnects.
func (c *Client) Run(ctx context.Context) error {
	ln := c.retryStartup(ctx)
	if ln == nil {
		return nil // ctx cancelled during startup — a clean shutdown, not a failure
	}
	c.listenPort = ln.Addr().(*net.TCPAddr).Port
	boundAddr := ln.Addr().String()
	c.boundListenAddr.Store(&boundAddr)

	runCtx, err := c.beginLifecycle(ctx)
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer c.stopLifecycle(ln)
	if !c.startTracked(func() { c.uploads.dispatch(runCtx) }) {
		_ = ln.Close()
		return errors.New("soulseek: lifecycle stopped before upload dispatcher start")
	}
	if !c.startTracked(func() { c.acceptPeers(runCtx, ln) }) {
		_ = ln.Close()
		return errors.New("soulseek: lifecycle stopped before listener start")
	}
	if !c.startTracked(func() { c.runInitialShareScan(runCtx) }) {
		_ = ln.Close()
		return errors.New("soulseek: lifecycle stopped before initial share scan start")
	}
	if !c.startTracked(func() { c.sampleThroughput(runCtx) }) {
		_ = ln.Close()
		return errors.New("soulseek: lifecycle stopped before throughput sampler start")
	}
	// The message worker spans reconnects rather than living inside
	// serveConnected: a message only needs a live connection for its ack, and
	// each queued entry carries the generation it arrived on so a stale ack is
	// refused instead of landing in a newer session.
	if c.cfg.MessageSink != nil {
		if !c.startTracked(func() { c.runMessageWorker(runCtx) }) {
			_ = ln.Close()
			return errors.New("soulseek: lifecycle stopped before message worker start")
		}
	}

	for {
		if runCtx.Err() != nil {
			return nil
		}

		err := c.connectAndServe(runCtx)
		if err == nil {
			return nil
		}

		if isTerminalErr(err) {
			c.recordFailed(err)
			return err
		}

		failures := c.recordTransientFailure(err)

		// nextBackoff takes retries as a 0-based count so the first retry
		// waits exactly backoffBase; failures is 1 on the first transient
		// failure, hence the -1.
		wait := nextBackoff(failures-1, c.cfg.backoffBase, c.cfg.backoffCap)
		c.logger.Warn("soulseek connection failed; reconnecting",
			"err", err, "backoff", wait, "consecutive_failures", failures)
		select {
		case <-runCtx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// retryStartup performs the one-time pre-connect setup — the peer-listener
// bind — retrying transient failures with the same exponential backoff the
// server reconnect loop uses. Previously a failing bind returned straight out
// of Run, and the single caller (cmd) never retried, so one boot-time hiccup
// (a briefly-held port) killed soulseek for the whole process life. There is
// no clean terminal classification for this — a genuinely bad config is
// caught by config.Validate first — so, like the reconnect loop, it is
// treated as transient and retried until it succeeds or ctx is cancelled.
// Returns nil only when ctx is cancelled during startup.
func (c *Client) retryStartup(ctx context.Context) net.Listener {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		ln, err := c.trySetup(ctx)
		if err == nil {
			return ln
		}
		wait := nextBackoff(attempt, c.cfg.backoffBase, c.cfg.backoffCap)
		c.logger.Warn("soulseek startup failed; retrying",
			"err", err, "backoff", wait, "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// trySetup resolves the peer listener address (fetching the forwarded port
// from gluetun first, when configured) and binds the peer listener,
// returning the bound listener or the first error. Both steps are still
// retried by retryStartup with its existing backoff. The initial share scan
// no longer gates this: it runs concurrently once Run starts serving the
// connection (see runInitialShareScan), so a slow or stalled scan can never
// delay connecting to the server.
func (c *Client) trySetup(ctx context.Context) (net.Listener, error) {
	addr := c.cfg.ListenAddr
	if c.cfg.GluetunControlURL != "" {
		port, err := c.fetchGluetunPort(ctx)
		if err != nil {
			return nil, fmt.Errorf("gluetun forwarded port: %w", err)
		}
		host, _, err := net.SplitHostPort(c.cfg.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("split listen addr %s: %w", c.cfg.ListenAddr, err)
		}
		addr = net.JoinHostPort(host, strconv.Itoa(port))
		c.logger.Info("gluetun forwarded port fetched", "port", port, "listen_addr", addr)
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for peer connections on %s: %w", addr, err)
	}
	return ln, nil
}

// isTerminalErr reports whether err should stop Run from reconnecting.
func isTerminalErr(err error) bool {
	return errors.Is(err, server.ErrInvalidPass) ||
		errors.Is(err, server.ErrInvalidUsername) ||
		errors.Is(err, server.ErrInvalidVersion) ||
		errors.Is(err, errRelogged)
}

// connectAndServe dials, logs in, and serves one connection. It returns nil
// only when ctx is cancelled cleanly; any other return is an error to be
// classified as terminal or transient by the caller.
func (c *Client) connectAndServe(ctx context.Context) error {
	c.recordAttempt()

	dialer := net.Dialer{Timeout: c.cfg.dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.cfg.Address)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.cfg.Address, err)
	}
	defer conn.Close()

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetKeepAlive(true); err != nil {
			return fmt.Errorf("enable tcp keepalive: %w", err)
		}
		if err := tcpConn.SetKeepAlivePeriod(tcpKeepAliveInterval); err != nil {
			return fmt.Errorf("set tcp keepalive period: %w", err)
		}
	}

	if err := c.login(ctx, conn); err != nil {
		return err
	}

	c.recordConnected()
	c.logger.Info("logged in to soulseek server",
		"address", c.cfg.Address, "username", c.cfg.Username)

	return c.serveConnected(ctx, conn)
}

// login sends the Login message and waits for the server's response. The
// handshake is bounded by a deadline (reusing dialTimeout, so a server that
// accepts the TCP connection but never speaks the protocol cannot block Run
// indefinitely) and by ctx (so a shutdown during a stalled handshake closes
// the connection and returns promptly instead of waiting out the deadline).
func (c *Client) login(ctx context.Context, conn net.Conn) error {
	if err := conn.SetDeadline(time.Now().Add(c.cfg.dialTimeout)); err != nil {
		return fmt.Errorf("set login deadline: %w", err)
	}

	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopWatch:
		}
	}()

	login := &server.Login{Username: c.cfg.Username, Password: c.cfg.Password}
	if _, err := server.Write(conn, login); err != nil {
		return fmt.Errorf("write login: %w", err)
	}

	message, _, code, err := server.Read(conn)
	if err != nil {
		return fmt.Errorf("read login response: %w", err)
	}
	if code != server.CodeLogin {
		return fmt.Errorf("unexpected message code %d while awaiting login response", code)
	}

	response := &server.Login{}
	if err := response.Deserialize(message); err != nil {
		return fmt.Errorf("deserialize login response: %w", err)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear login deadline: %w", err)
	}
	return nil
}

// serveConnected keeps the connection alive with periodic pings and reads
// incoming messages until ctx is cancelled or a read fails. It owns
// c.serverConn for the duration of the connection: set once at the top, so
// sendToServer (and any goroutine it starts, e.g. peer dials) can write to
// the server, and cleared on every exit path, at which point any pending
// GetPeerAddress waiter is failed rather than left to time out.
func (c *Client) serveConnected(ctx context.Context, conn net.Conn) error {
	serverCtx, serverCancel := context.WithCancel(ctx)
	c.serverWriteMu.Lock()
	c.mu.Lock()
	c.serverGeneration++
	generation := c.serverGeneration
	c.serverConn = conn
	c.serverCancel = serverCancel
	c.mu.Unlock()
	c.serverWriteMu.Unlock()
	defer func() {
		// Presence learned from a dead session must become unknown immediately.
		// The generation guard prevents delayed cleanup from touching a newer
		// connection while retaining the desired set for reconnect replay.
		c.presence.invalidate(generation)
		// Invalidate generation-owned work before clearing its state. Closing
		// the socket also unblocks an in-progress serialized write.
		serverCancel()
		_ = conn.Close()
		c.serverWriteMu.Lock()
		c.mu.Lock()
		if c.serverConn == conn && c.serverGeneration == generation {
			c.serverConn = nil
			c.serverCancel = nil
		}
		c.mu.Unlock()
		c.serverWriteMu.Unlock()
		c.tree.deactivate(generation)
		c.sessions.CloseGeneration("D", generation, errNoServerConnection)
		c.searches.failGeneration(generation, errNoServerConnection)
		c.failAllAddrWaiters(errNoServerConnection)
		c.failAllPendingAttempts(errNoServerConnection)
	}()

	if err := sendToServerGeneration(c, generation, &server.SetListenPort{Port: c.listenPort, ObfuscatedPort: 0}); err != nil {
		return fmt.Errorf("write set listen port: %w", err)
	}
	if err := c.tree.activate(generation); err != nil {
		return fmt.Errorf("initialize distributed tree: %w", err)
	}
	// Upload capacity is generation-scoped tree state. Request our own current
	// stats once for every authenticated server connection so AcceptChildren
	// can be driven by an actual server response rather than unsolicited data.
	if err := sendToServerGeneration(c, generation, &server.GetUserStats{Username: c.cfg.Username}); err != nil {
		return fmt.Errorf("request own user stats: %w", err)
	}
	// Soulseek expects an explicit index count on every authenticated session,
	// including download-only clients with an empty index. The helper reads
	// the currently published stats at send time under announceMu, so this
	// announcement and a concurrent background-scan or SIGHUP-rescan
	// announcement can interleave in any order without the server ending up
	// on stale counts.
	if err := c.announceSharesOnLogin(); err != nil {
		return fmt.Errorf("announce shared folders and files: %w", err)
	}

	readErrs := make(chan error, 1)
	if !c.startTracked(func() { readErrs <- c.readLoop(serverCtx, conn) }) {
		return errors.New("soulseek: lifecycle stopping")
	}
	c.presence.activate(generation)

	ticker := time.NewTicker(c.cfg.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close()
			<-readErrs
			return nil

		case <-ticker.C:
			if err := sendToServer(c, &server.Ping{}); err != nil {
				_ = conn.Close()
				<-readErrs
				return fmt.Errorf("write ping: %w", err)
			}

		case <-c.presence.syncNeeded:
			if err := c.syncConversationPresence(generation); err != nil {
				_ = conn.Close()
				<-readErrs
				return fmt.Errorf("synchronize conversation presence: %w", err)
			}

		case err := <-readErrs:
			return err
		}
	}
}

func (c *Client) syncConversationPresence(generation uint64) error {
	actions := c.presence.syncActions(generation)
	for _, username := range actions.unwatch {
		if err := sendToServerGeneration(c, generation, &server.UnwatchUser{Username: username}); err != nil {
			return err
		}
		c.presence.acknowledgeUnwatch(generation, username)
	}
	for _, username := range actions.watch {
		if err := sendToServerGeneration(c, generation, &server.WatchUser{Username: username}); err != nil {
			return err
		}
		c.presence.acknowledgeWatch(generation, username)
	}
	return nil
}

// readLoop reads messages from conn until it fails or handleMessage reports
// a terminal condition (Relogged). ctx (Run's shutdown ctx) is threaded down to
// handleConnectToPeer so shutdown can cancel an in-flight dial-back.
func (c *Client) readLoop(ctx context.Context, conn net.Conn) error {
	for {
		message, _, code, err := server.Read(conn)
		if err != nil {
			return fmt.Errorf("read message: %w", err)
		}

		if err := c.handleMessage(ctx, code, message); err != nil {
			return err
		}
	}
}

// handleMessage dispatches one server message. Everything not explicitly
// understood is logged at debug level and dropped.
func (c *Client) handleMessage(ctx context.Context, code server.Code, reader io.Reader) error {
	switch code {
	case server.CodeRelogged:
		relogged := &server.Relogged{}
		if err := relogged.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize relogged: %w", err)
		}
		return errRelogged

	case server.CodeGetPeerAddress:
		msg := &server.GetPeerAddress{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize get peer address: %w", err)
		}
		c.deliverAddr(msg.Username, addrResult{msg: *msg})
		return nil

	case server.CodeConnectToPeer:
		msg := &server.ConnectToPeer{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize connect to peer: %w", err)
		}
		c.mu.Lock()
		generation := c.serverGeneration
		c.mu.Unlock()
		c.handleConnectToPeer(ctx, generation, *msg)
		return nil

	case server.CodeCantConnectToPeer:
		msg := &server.CantConnectToPeer{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize cant connect to peer: %w", err)
		}
		c.handleCantConnectToPeer(*msg)
		return nil

	case server.CodeFileSearch:
		msg := &server.FileSearch{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize incoming file search: %w", err)
		}
		c.respondToSearch(msg.Username, msg.Token, msg.SearchQuery)
		return nil

	case server.CodePossibleParents:
		msg := &server.PossibleParents{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize possible parents: %w", err)
		}
		generation := c.currentServerGeneration()
		c.tree.offerParents(ctx, generation, msg.Parents)
		return nil

	case server.CodeResetDistributed:
		msg := &server.ResetDistributed{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize reset distributed: %w", err)
		}
		c.tree.reset(c.currentServerGeneration())
		return nil

	case server.CodeEmbeddedMessage:
		msg := &server.EmbeddedMessage{}
		if err := msg.Deserialize(reader); err != nil {
			// Embedded distributed messages are optional compatibility input.
			// Ignore malformed wrappers without dropping the server session.
			if c.logger != nil {
				c.logger.Debug("ignore malformed embedded distributed message", "err", err)
			}
			return nil
		}
		if err := c.tree.handleServerEmbedded(c.currentServerGeneration(), *msg); err != nil {
			return fmt.Errorf("handle embedded distributed message: %w", err)
		}
		return nil

	case server.CodeExcludedSearchPhrases:
		msg := &server.ExcludedSearchPhrases{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize excluded search phrases: %w", err)
		}
		phrases := msg.Phrases
		c.excludedPhrases.Store(&phrases)
		if c.logger != nil {
			c.logger.Info("received excluded search phrases", "count", len(phrases))
			c.logger.Debug("excluded search phrases", "phrases", phrases)
		}
		return nil

	case server.CodeParentMinSpeed:
		msg := &server.ParentMinSpeed{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize parent minimum speed: %w", err)
		}
		c.tree.updateParentMinSpeed(c.currentServerGeneration(), msg.MinSpeed)
		return nil

	case server.CodeParentSpeedRatio:
		msg := &server.ParentSpeedRatio{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize parent speed ratio: %w", err)
		}
		c.tree.updateParentRatio(c.currentServerGeneration(), msg.SpeedRatio)
		return nil

	case server.CodeWatchUser:
		msg := &server.WatchUser{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize watch user: %w", err)
		}
		generation := c.currentServerGeneration()
		if msg.Username == c.cfg.Username && msg.Exists {
			c.tree.updateUploadSpeed(generation, msg.AverageSpeed)
		}
		c.presence.updateWatch(generation, *msg)
		return nil

	case server.CodeGetUserStatus:
		msg := &server.GetUserStatus{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize get user status: %w", err)
		}
		c.presence.updateStatus(c.currentServerGeneration(), msg.Username, msg.Status)
		return nil

	case server.CodeGetUserStats:
		msg := &server.GetUserStats{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize user stats: %w", err)
		}
		if msg.Username == c.cfg.Username {
			c.tree.updateUploadSpeed(c.currentServerGeneration(), msg.Speed)
		}
		return nil

	case server.CodeMessageUser:
		return c.handleIncomingPrivateMessage(reader)
	}

	if c.logger != nil {
		c.logger.Debug("dropping unhandled soulseek message", "code", code)
	}
	return nil
}

func (c *Client) recordAttempt() {
	prev := *c.status.Load()
	prev.State = StateConnecting
	prev.LastAttempt = time.Now()
	c.status.Store(&prev)
}

func (c *Client) recordConnected() {
	prev := *c.status.Load()
	prev.State = StateConnected
	prev.LastConnectedAt = time.Now()
	prev.ConsecutiveFailures = 0
	c.status.Store(&prev)
}

func (c *Client) recordFailed(err error) {
	prev := *c.status.Load()
	prev.State = StateFailed
	prev.LastError = err.Error()
	prev.LastErrorAt = time.Now()
	prev.ConsecutiveFailures++
	c.status.Store(&prev)
}

// recordTransientFailure records the failure and returns the updated
// consecutive-failure count, for the caller to compute the next backoff.
func (c *Client) recordTransientFailure(err error) int {
	prev := *c.status.Load()
	prev.State = StateDisconnected
	prev.LastError = err.Error()
	prev.LastErrorAt = time.Now()
	prev.ConsecutiveFailures++
	c.status.Store(&prev)
	return prev.ConsecutiveFailures
}

// nextBackoff returns base * 2^retries, capped at maxBackoff. retries is
// 0-based (the first retry -> retries 0 -> wait exactly base). The exponent
// is clamped so 1<<retries never overflows an int on any platform, since
// callers may pass arbitrarily large retry counts.
//
// Copied from internal/pipeline/backoff.go rather than exported from there,
// since pipeline is a separate scheduling concern and this package should
// not depend on it.
func (c *Client) currentServerGeneration() uint64 {
	c.mu.Lock()
	generation := c.serverGeneration
	if c.serverConn == nil {
		generation = 0
	}
	c.mu.Unlock()
	return generation
}

func (c *Client) isServerGenerationActive(generation uint64) bool {
	c.mu.Lock()
	active := c.serverConn != nil && c.serverGeneration == generation
	c.mu.Unlock()
	return active
}

func (c *Client) beginLifecycle(parent context.Context) (context.Context, error) {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.lifeActive {
		return nil, errors.New("soulseek: Run already active")
	}
	// A new lifecycle never inherits queue or token routing state from a prior
	// run, including a prior run that stopped during negotiation.
	c.uploads.reset()
	c.lifeCtx, c.lifeCancel = context.WithCancel(parent)
	c.lifeActive = true
	c.lifeStopping = false
	return c.lifeCtx, nil
}

func (c *Client) lifecycleContext() context.Context {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.lifeCtx != nil {
		return c.lifeCtx
	}
	return context.Background()
}

func (c *Client) startTracked(fn func()) bool {
	c.lifeMu.Lock()
	if !c.lifeActive || c.lifeStopping {
		c.lifeMu.Unlock()
		return false
	}
	c.lifeWG.Add(1)
	c.lifeMu.Unlock()
	go func() {
		defer c.lifeWG.Done()
		// Every tracked goroutine processes untrusted peer/server input. A
		// panic in any of them (a decode bug, a nil deref) would otherwise
		// unwind past the goroutine and crash the whole daemon, killing every
		// other connection and in-flight transfer. Contain it here so one
		// hostile or buggy peer only loses its own session; fn's own deferred
		// cleanup (session Close, lease release) still runs during unwinding.
		defer func() {
			if r := recover(); r != nil && c.logger != nil {
				c.logger.Error("soulseek: recovered from panic in tracked goroutine",
					"panic", r, "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
	return true
}

func (c *Client) stopLifecycle(ln net.Listener) {
	c.lifeMu.Lock()
	if !c.lifeActive || c.lifeStopping {
		c.lifeMu.Unlock()
		return
	}
	c.lifeStopping = true
	cancel := c.lifeCancel
	c.lifeMu.Unlock()

	// Cancellation precedes reset/close of generation-owned work.
	cancel()
	_ = ln.Close()
	c.closeHandshakes()
	c.sessions.CloseAll(context.Canceled)
	c.searches.failAll(context.Canceled)
	c.failAllAddrWaiters(context.Canceled)
	c.failAllPendingAttempts(context.Canceled)
	c.lifeWG.Wait()
	// A final reset closes the race where a P-session hook enqueued work after
	// the dispatcher observed cancellation but before sessions were closed.
	c.uploads.reset()

	c.lifeMu.Lock()
	c.lifeActive = false
	c.lifeCtx = nil
	c.lifeCancel = nil
	c.lifeMu.Unlock()
}

func nextBackoff(retries int, base, maxBackoff time.Duration) time.Duration {
	const maxExponent = 32 // 1<<32 * any realistic base already exceeds maxBackoff
	exp := retries
	if exp > maxExponent {
		exp = maxExponent
	}
	d := base * time.Duration(1<<exp)
	if d > maxBackoff || d < 0 { // d<0 guards against overflow wrap-around
		return maxBackoff
	}
	return d
}
