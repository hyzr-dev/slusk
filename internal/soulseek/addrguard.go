package soulseek

import (
	"fmt"
	"net"
)

// blockedAddrError distinguishes a server-supplied address that is present
// but policy-forbidden to dial directly (loopback/link-local/private) from
// one that has no reachable target at all (nil/unspecified IP or
// non-positive port). Callers with an indirect NAT-traversal fallback (see
// dialPeer) treat a blockedAddrError like a failed direct dial and continue:
// the indirect path never dials the address itself, since the peer connects
// back to us instead.
type blockedAddrError struct{ err error }

func (e *blockedAddrError) Error() string { return e.err.Error() }
func (e *blockedAddrError) Unwrap() error { return e.err }

// validatePeerDialAddr rejects server-supplied peer addresses that must never
// be dialed (threat T12): unspecified/zero, loopback, and link-local targets
// unconditionally; RFC 1918 / ULA private ranges unless allowPrivate is set
// (soulseek.allow_private_peer_addresses), since legitimate LAN peers live
// there. Returns nil when the address may be dialed. A nil/unspecified IP or
// non-positive port yields a plain error (no reachable address at all); a
// present but forbidden address yields a *blockedAddrError so callers can
// tell the two cases apart (see dialPeer). This is the pure, unconditional
// guard - its own tests (addrguard_test.go) rely on loopback being blocked
// no matter what, so callers wanting the test-suite loopback carve-out must
// go through (*Client).validateDialAddr instead of calling this directly.
func validatePeerDialAddr(ip net.IP, port int, allowPrivate bool) error {
	if ip == nil || ip.IsUnspecified() || port <= 0 {
		return fmt.Errorf("no reachable address (ip=%v port=%d)", ip, port)
	}
	if ip.IsLoopback() {
		return &blockedAddrError{fmt.Errorf("refusing to dial loopback address %s", ip)}
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return &blockedAddrError{fmt.Errorf("refusing to dial link-local address %s", ip)}
	}
	if !allowPrivate && ip.IsPrivate() {
		return &blockedAddrError{fmt.Errorf("refusing to dial private address %s (set soulseek.allow_private_peer_addresses to allow LAN peers)", ip)}
	}
	return nil
}

// validateDialAddr is what every real dial site in this package (dialPeer,
// handleConnectToPeer, runCandidate) calls instead of validatePeerDialAddr
// directly. It applies the same T12 guard, with one carve-out: when
// c.cfg.allowLoopbackPeerDial is set, a loopback target is treated as
// allowed. That field is only ever set by this package's own test helpers
// (see startConnectedClient in connectpeer_test.go), which necessarily run
// their fake peers on 127.0.0.1; production configuration never sets it, so
// this carve-out has zero effect outside the test suite and does not weaken
// the real protection.
func (c *Client) validateDialAddr(ip net.IP, port int) error {
	if c.cfg.allowLoopbackPeerDial && ip != nil && ip.IsLoopback() && port > 0 {
		return nil
	}
	return validatePeerDialAddr(ip, port, c.cfg.AllowPrivatePeerAddresses)
}
