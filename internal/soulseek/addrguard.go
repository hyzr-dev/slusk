package soulseek

import (
	"fmt"
	"net"
	"testing"
)

// validatePeerDialAddr rejects server-supplied peer addresses that must never
// be dialed (threat T12): unspecified/zero, loopback, and link-local targets
// unconditionally; RFC 1918 / ULA private ranges unless allowPrivate is set
// (soulseek.allow_private_peer_addresses), since legitimate LAN peers live
// there. Returns nil when the address may be dialed. This is the pure,
// unconditional guard - its own tests (addrguard_test.go) rely on loopback
// being blocked no matter what, so callers wanting the test-suite loopback
// carve-out must go through (*Client).validateDialAddr instead of calling
// this directly.
func validatePeerDialAddr(ip net.IP, port int, allowPrivate bool) error {
	if ip == nil || ip.IsUnspecified() || port <= 0 {
		return fmt.Errorf("no reachable address (ip=%v port=%d)", ip, port)
	}
	if ip.IsLoopback() {
		return fmt.Errorf("refusing to dial loopback address %s", ip)
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("refusing to dial link-local address %s", ip)
	}
	if !allowPrivate && ip.IsPrivate() {
		return fmt.Errorf("refusing to dial private address %s (set soulseek.allow_private_peer_addresses to allow LAN peers)", ip)
	}
	return nil
}

// validateDialAddr is what every real dial site in this package (dialPeer,
// handleConnectToPeer, runCandidate) calls instead of validatePeerDialAddr
// directly. It applies the same T12 guard, with one carve-out: inside a `go
// test` binary, a loopback target is treated as allowed, because this
// package's test suite necessarily runs its fake peers on 127.0.0.1.
// testing.Testing() is only ever true when the calling binary was built by
// `go test`; a normal `go build` production binary never links the testing
// package, so this carve-out has zero effect outside the test suite and does
// not weaken the real protection.
func (c *Client) validateDialAddr(ip net.IP, port int) error {
	if testing.Testing() && ip != nil && ip.IsLoopback() && port > 0 {
		return nil
	}
	return validatePeerDialAddr(ip, port, c.cfg.AllowPrivatePeerAddresses)
}
