package soulseek

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul/server"
)

// watchGluetunPort re-fetches the forwarded port every GluetunPollInterval for
// as long as ctx lives, moving the peer listener whenever gluetun starts
// reporting a different one (issue #395). Before this existed the port was
// fetched exactly once at startup, so a VPN reconnect that rotated it left the
// client listening on a port no peer could reach — silently, since nothing
// about that state looks like a failure from the inside.
//
// Started by Run only when GluetunControlURL is set.
func (c *Client) watchGluetunPort(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.GluetunPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refreshGluetunPort(ctx)
		}
	}
}

// refreshGluetunPort performs one poll: fetch, compare, and rebind if the port
// moved. Every failure mode short of "gluetun reports a different, bindable
// port" leaves the current listener untouched — a control server that is down,
// unauthorized, or still reporting port 0 is not evidence that the port we are
// bound to has stopped working, and tearing a healthy listener down over it
// would turn a cosmetic outage into a real one.
func (c *Client) refreshGluetunPort(ctx context.Context) {
	current := int(c.listenPort.Load())
	port, err := c.fetchGluetunPort(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Warn("gluetun forwarded port refresh failed; keeping current peer listener",
				"err", err, "listen_port", current)
		}
		return
	}
	if port == current {
		return
	}
	if err := c.rebindPeerListener(ctx, port); err != nil {
		if ctx.Err() == nil {
			c.logger.Warn("gluetun forwarded port changed but rebinding failed; keeping current peer listener",
				"err", err, "listen_port", current, "gluetun_port", port)
		}
		return
	}
	c.logger.Info("gluetun forwarded port changed; peer listener rebound",
		"previous_port", current, "listen_port", port)

	// The server holds the port from the SetListenPort sent at login, so
	// without this it keeps handing peers the dead one until the next
	// reconnect. A failure here is not fatal: the write path already tears
	// down the connection it failed on, and the reconnect re-announces the
	// current port from c.listenPort.
	if err := sendToServer(c, &server.SetListenPort{Port: port, ObfuscatedPort: 0}); err != nil {
		c.logger.Warn("announcing the new listen port to the server failed; it will be sent on the next reconnect",
			"err", err, "listen_port", port)
	}
}

// rebindPeerListener binds a second peer listener on port and makes it the
// current one, leaving the old listener's already-accepted sockets alone: an
// accepted connection is independent of the listener it came from, so
// in-flight transfers survive. The new listener starts accepting before the
// old one closes, so the swap has an overlap rather than a gap.
//
// On any failure the client is left exactly as it was, still bound to the old
// port.
func (c *Client) rebindPeerListener(ctx context.Context, port int) error {
	host, _, err := net.SplitHostPort(c.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("split listen addr %s: %w", c.cfg.ListenAddr, err)
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for peer connections on %s: %w", addr, err)
	}

	// Started before the install so no inbound connection can land on an
	// unattended listener, and outside lnMu so this never takes lifeMu while
	// holding lnMu (see the lnMu field comment).
	if !c.startTracked(func() { c.acceptPeers(ctx, ln) }) {
		_ = ln.Close()
		return fmt.Errorf("lifecycle stopping")
	}
	old, ok := c.setPeerListener(ln)
	if !ok {
		_ = ln.Close() // teardown won the race; its accept loop exits on ErrClosed
		return fmt.Errorf("lifecycle stopping")
	}
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// fetchGluetunPort queries the gluetun control server configured via
// c.cfg.GluetunControlURL for the currently forwarded port. It performs a
// single request bounded by c.cfg.gluetunTimeout - retry and backoff are
// retryStartup's job (which also warn-logs every failed attempt), and
// trySetup logs the fetched port on success, so this function does neither
// on its own.
func (c *Client) fetchGluetunPort(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.gluetunTimeout)
	defer cancel()

	url := strings.TrimSuffix(c.cfg.GluetunControlURL, "/") + "/v1/portforward"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build gluetun request: %w", err)
	}
	if c.cfg.GluetunAPIKey != "" {
		req.Header.Set("X-API-Key", c.cfg.GluetunAPIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch gluetun forwarded port: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return 0, fmt.Errorf("gluetun control server rejected the request (HTTP %d): check soulseek.gluetun.api_key", resp.StatusCode)
		}
		return 0, fmt.Errorf("gluetun control server returned HTTP %d", resp.StatusCode)
	}

	var body struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode gluetun forwarded port response: %w", err)
	}

	if body.Port == 0 {
		return 0, fmt.Errorf("gluetun reports forwarded port 0: VPN port forwarding not yet established")
	}
	if body.Port < 0 || body.Port > 65535 {
		return 0, fmt.Errorf("gluetun reports out-of-range forwarded port %d", body.Port)
	}

	return body.Port, nil
}
