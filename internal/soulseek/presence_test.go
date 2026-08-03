package soulseek

import (
	"fmt"
	"sync"
	"testing"

	"github.com/samuelenocsson/slusk/internal/soulseek/soul/server"
)

func TestPresenceStatusMapping(t *testing.T) {
	tests := []struct {
		name   string
		exists bool
		status server.UserStatus
		known  bool
		online bool
	}{
		{name: "offline", exists: true, status: server.StatusOffline, known: true, online: false},
		{name: "away", exists: true, status: server.StatusAway, known: true, online: true},
		{name: "online", exists: true, status: server.StatusOnline, known: true, online: true},
		{name: "unknown status", exists: true, status: server.UserStatus(99)},
		{name: "does not exist", exists: false, status: server.StatusOnline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newPresenceTracker()
			p.reconcileAndSnapshot([]string{"alice"})
			p.activate(1)
			p.updateWatch(1, server.WatchUser{Username: "alice", Exists: tt.exists, Status: tt.status})
			got, known := p.reconcileAndSnapshot([]string{"alice"})["alice"]
			if known != tt.known || got != tt.online {
				t.Fatalf("presence = (%v, %v), want (%v, %v)", got, known, tt.online, tt.known)
			}
		})
	}
}

func TestPresenceReconcileFiltersDeduplicatesAndCapsNewest(t *testing.T) {
	p := newPresenceTracker()
	usernames := []string{"", "alice", "alice"}
	for i := 0; i < maxPresenceUsers; i++ {
		usernames = append(usernames, fmt.Sprintf("user-%03d", i))
	}
	usernames = append(usernames, "over-cap")

	p.reconcileAndSnapshot(usernames)
	p.activate(4)
	actions := p.syncActions(4)
	if len(actions.watch) != maxPresenceUsers {
		t.Fatalf("watch count = %d, want %d", len(actions.watch), maxPresenceUsers)
	}
	if actions.watch[0] != "alice" || actions.watch[maxPresenceUsers-1] != "user-498" {
		t.Fatalf("watch bounds = %q..%q", actions.watch[0], actions.watch[maxPresenceUsers-1])
	}
	for _, username := range actions.watch {
		p.acknowledgeWatch(4, username)
		p.updateStatus(4, username, server.StatusOnline)
	}
	p.updateStatus(4, "over-cap", server.StatusOnline)

	got := p.reconcileAndSnapshot(usernames)
	if len(got) != maxPresenceUsers {
		t.Fatalf("snapshot count = %d, want %d", len(got), maxPresenceUsers)
	}
	if _, exists := got["over-cap"]; exists {
		t.Fatal("over-cap username appeared in snapshot")
	}
	if repeat := p.syncActions(4); len(repeat.watch) != 0 || len(repeat.unwatch) != 0 {
		t.Fatalf("identical reconciliation produced actions: %+v", repeat)
	}
}

func TestPresenceEvictionAndDisconnectInvalidation(t *testing.T) {
	p := newPresenceTracker()
	p.reconcileAndSnapshot([]string{"alice", "bob"})
	p.activate(8)
	for _, username := range p.syncActions(8).watch {
		p.acknowledgeWatch(8, username)
		p.updateStatus(8, username, server.StatusOnline)
	}

	got := p.reconcileAndSnapshot([]string{"bob", "carol"})
	if len(got) != 1 || !got["bob"] {
		t.Fatalf("snapshot after eviction = %v, want only bob online", got)
	}
	actions := p.syncActions(8)
	if len(actions.unwatch) != 1 || actions.unwatch[0] != "alice" || len(actions.watch) != 1 || actions.watch[0] != "carol" {
		t.Fatalf("eviction actions = %+v", actions)
	}

	p.invalidate(8)
	if got := p.reconcileAndSnapshot([]string{"bob", "carol"}); len(got) != 0 {
		t.Fatalf("snapshot after disconnect = %v, want unknown", got)
	}
	p.activate(9)
	if actions := p.syncActions(9); len(actions.watch) != 2 {
		t.Fatalf("reconnect watch actions = %+v, want retained desired set", actions)
	}
	p.invalidate(8)
	p.updateStatus(9, "bob", server.StatusOffline)
	if got, known := p.reconcileAndSnapshot([]string{"bob", "carol"})["bob"]; !known || got {
		t.Fatalf("stale cleanup affected generation 9: (%v, %v)", got, known)
	}
}

func TestPresenceIgnoresUnsolicitedAndFiltersSnapshotInput(t *testing.T) {
	p := newPresenceTracker()
	p.reconcileAndSnapshot([]string{"alice", "bob"})
	p.activate(1)
	p.updateStatus(1, "alice", server.StatusOnline)
	p.updateStatus(1, "mallory", server.StatusOnline)

	got := p.reconcileAndSnapshot([]string{"bob"})
	if len(got) != 0 {
		t.Fatalf("filtered snapshot = %v, want empty", got)
	}
	p.updateStatus(1, "alice", server.StatusOnline)
	if got := p.reconcileAndSnapshot([]string{"bob"}); len(got) != 0 {
		t.Fatalf("evicted user was accepted: %v", got)
	}
}

func TestPresenceConcurrentAccess(t *testing.T) {
	p := newPresenceTracker()
	p.activate(1)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				username := fmt.Sprintf("user-%d", (i+worker)%20)
				p.reconcileAndSnapshot([]string{username, "shared"})
				p.updateStatus(1, username, server.UserStatus(i%3))
				_ = p.syncActions(1)
			}
		}()
	}
	wg.Wait()
}
