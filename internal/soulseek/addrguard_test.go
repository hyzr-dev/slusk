package soulseek

import (
	"net"
	"testing"
)

func TestValidatePeerDialAddr(t *testing.T) {
	tests := []struct {
		name         string
		ip           net.IP
		port         int
		allowPrivate bool
		wantErr      bool
	}{
		// No reachable address.
		{name: "nil ip", ip: nil, port: 1234, wantErr: true},
		{name: "0.0.0.0", ip: net.ParseIP("0.0.0.0"), port: 1234, wantErr: true},
		{name: "unspecified ipv6 ::", ip: net.ParseIP("::"), port: 1234, wantErr: true},
		{name: "port zero", ip: net.ParseIP("8.8.8.8"), port: 0, wantErr: true},
		{name: "port negative", ip: net.ParseIP("8.8.8.8"), port: -1, wantErr: true},

		// Loopback: always blocked, even with allowPrivate.
		{name: "loopback 127.0.0.1", ip: net.ParseIP("127.0.0.1"), port: 1234, wantErr: true},
		{name: "loopback 127.8.9.1", ip: net.ParseIP("127.8.9.1"), port: 1234, wantErr: true},
		{name: "loopback ::1", ip: net.ParseIP("::1"), port: 1234, wantErr: true},
		{name: "loopback ipv4-mapped ::ffff:127.0.0.1", ip: net.ParseIP("::ffff:127.0.0.1"), port: 1234, wantErr: true},
		{name: "loopback with allowPrivate", ip: net.ParseIP("127.0.0.1"), port: 1234, allowPrivate: true, wantErr: true},

		// Link-local: always blocked, even with allowPrivate.
		{name: "link-local unicast 169.254.10.10", ip: net.ParseIP("169.254.10.10"), port: 1234, wantErr: true},
		{name: "link-local unicast fe80::1", ip: net.ParseIP("fe80::1"), port: 1234, wantErr: true},
		{name: "link-local multicast ff02::1", ip: net.ParseIP("ff02::1"), port: 1234, wantErr: true},
		{name: "link-local with allowPrivate", ip: net.ParseIP("169.254.10.10"), port: 1234, allowPrivate: true, wantErr: true},

		// Public addresses: always allowed, regardless of allowPrivate.
		{name: "public 8.8.8.8", ip: net.ParseIP("8.8.8.8"), port: 1234, wantErr: false},
		{name: "public 8.8.8.8 with allowPrivate", ip: net.ParseIP("8.8.8.8"), port: 1234, allowPrivate: true, wantErr: false},
		{name: "public 2001:4860::8888", ip: net.ParseIP("2001:4860::8888"), port: 1234, wantErr: false},
		{name: "boundary public 172.32.0.1", ip: net.ParseIP("172.32.0.1"), port: 1234, wantErr: false},
		{name: "boundary public 9.255.255.255", ip: net.ParseIP("9.255.255.255"), port: 1234, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePeerDialAddr(tt.ip, tt.port, tt.allowPrivate)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatePeerDialAddr(%v, %d, %v) = %v, wantErr %v", tt.ip, tt.port, tt.allowPrivate, err, tt.wantErr)
			}
		})
	}
}

func TestValidatePeerDialAddrPrivateRanges(t *testing.T) {
	private := []struct {
		name string
		ip   net.IP
	}{
		{"rfc1918 10.1.2.3", net.ParseIP("10.1.2.3")},
		{"rfc1918 172.16.0.1", net.ParseIP("172.16.0.1")},
		{"rfc1918 172.31.255.255", net.ParseIP("172.31.255.255")},
		{"rfc1918 192.168.1.1", net.ParseIP("192.168.1.1")},
		{"ula fd00::1", net.ParseIP("fd00::1")},
		{"ipv4-mapped private ::ffff:192.168.1.1", net.ParseIP("::ffff:192.168.1.1")},
	}

	for _, p := range private {
		t.Run(p.name+"/blocked by default", func(t *testing.T) {
			if err := validatePeerDialAddr(p.ip, 1234, false); err == nil {
				t.Fatalf("expected private address %s to be blocked when allowPrivate=false", p.ip)
			}
		})
		t.Run(p.name+"/allowed when configured", func(t *testing.T) {
			if err := validatePeerDialAddr(p.ip, 1234, true); err != nil {
				t.Fatalf("expected private address %s to be allowed when allowPrivate=true, got %v", p.ip, err)
			}
		})
	}
}
