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
	"sync"
	"sync/atomic"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/soulseek/soul/server"
)

const (
	defaultDialTimeout     = 10 * time.Second
	defaultPingInterval    = 5 * time.Minute
	defaultBackoffBase     = 5 * time.Second
	defaultBackoffCap      = 10 * time.Minute
	tcpKeepAliveInterval   = time.Minute
	defaultPeerInitTimeout = 10 * time.Second
	// defaultListenAddr is used only when Config.ListenAddr is left blank,
	// which production configuration never does (see config.SoulseekConfig);
	// it exists so tests that don't care about the peer listener don't have
	// to set one.
	defaultListenAddr = "127.0.0.1:0"
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
}

// Client manages one connection to the Soulseek server, reconnecting with
// backoff after transient failures. The zero value is not usable; construct
// with New.
type Client struct {
	cfg    Config
	logger *slog.Logger

	status atomic.Pointer[Status]

	// mu serializes writes to the server connection (the vendored server.Write
	// takes an io.Writer directly; nothing about it is safe for concurrent
	// use) and guards serverConn itself. serverConn is non-nil only while
	// serveConnected is running for the current connection.
	mu         sync.Mutex
	serverConn net.Conn

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

	c := &Client{cfg: cfg, logger: logger, pendingAddrs: make(map[string][]chan addrResult)}
	c.status.Store(&Status{State: StateDisconnected})
	return c
}

// serverMessage constrains sendToServer to the handful of server message
// types this package actually sends, rather than reusing server package's
// own (unexported) message[M] constraint.
type serverMessage[M any] interface {
	*server.Ping | *server.SetListenPort | *server.ConnectToPeer | *server.CantConnectToPeer
	Serialize(M) ([]byte, error)
}

// sendToServer serializes writes to the server connection behind c.mu, since
// multiple goroutines (the ping ticker, peer-connection establishment) may
// write concurrently. It reports an error if no server connection is
// currently established.
func sendToServer[M serverMessage[M]](c *Client, msg M) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.serverConn == nil {
		return errors.New("soulseek: not connected to server")
	}

	_, err := server.Write(c.serverConn, msg)
	return err
}

// Status returns a point-in-time snapshot of the client's connection state.
func (c *Client) Status() Status {
	return *c.status.Load()
}

// Run starts the peer listener, then dials the server, logs in, and serves
// the connection until ctx is cancelled or a terminal error occurs. On a
// transient server-connection failure it reconnects after an exponential
// backoff; the peer listener itself is started exactly once and lives across
// every reconnect. Run returns nil only when ctx is cancelled; it returns a
// non-nil error for a terminal failure (the peer listener failing to start,
// invalid credentials, outdated protocol version, or the account logging in
// elsewhere), and never reconnects afterward.
func (c *Client) Run(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen for peer connections on %s: %w", c.cfg.ListenAddr, err)
	}
	c.listenPort = ln.Addr().(*net.TCPAddr).Port

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		c.acceptPeers(ctx, ln)
	}()
	defer func() {
		_ = ln.Close()
		<-acceptDone
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}

		err := c.connectAndServe(ctx)
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
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
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
	c.mu.Lock()
	c.serverConn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.serverConn = nil
		c.mu.Unlock()
		c.failAllAddrWaiters(errNoServerConnection)
	}()

	if err := sendToServer(c, &server.SetListenPort{Port: c.listenPort, ObfuscatedPort: 0}); err != nil {
		return fmt.Errorf("write set listen port: %w", err)
	}

	readErrs := make(chan error, 1)
	go func() {
		readErrs <- c.readLoop(conn)
	}()

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

		case err := <-readErrs:
			return err
		}
	}
}

// readLoop reads messages from conn until it fails or handleMessage reports
// a terminal condition (Relogged).
func (c *Client) readLoop(conn net.Conn) error {
	for {
		message, _, code, err := server.Read(conn)
		if err != nil {
			return fmt.Errorf("read message: %w", err)
		}

		if err := c.handleMessage(code, message); err != nil {
			return err
		}
	}
}

// handleMessage dispatches one server message. Everything not explicitly
// understood is logged at debug level and dropped.
func (c *Client) handleMessage(code server.Code, reader io.Reader) error {
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
		c.handleConnectToPeer(*msg)
		return nil

	case server.CodeCantConnectToPeer:
		msg := &server.CantConnectToPeer{}
		if err := msg.Deserialize(reader); err != nil {
			return fmt.Errorf("deserialize cant connect to peer: %w", err)
		}
		c.handleCantConnectToPeer(*msg)
		return nil
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
