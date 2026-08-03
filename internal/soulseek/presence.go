package soulseek

import (
	"sort"
	"sync"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul/server"
)

const maxPresenceUsers = 500

type presenceTracker struct {
	mu sync.Mutex

	desiredOrder []string
	desired      map[string]struct{}
	remote       map[string]struct{}
	online       map[string]bool
	generation   uint64

	syncNeeded chan struct{}
}

type presenceSyncActions struct {
	watch   []string
	unwatch []string
}

func newPresenceTracker() *presenceTracker {
	return &presenceTracker{
		desired:    make(map[string]struct{}),
		remote:     make(map[string]struct{}),
		online:     make(map[string]bool),
		syncNeeded: make(chan struct{}, 1),
	}
}

// ConversationPresence reconciles the newest conversation counterparts that
// should be watched and returns only statuses explicitly learned in the
// current server connection. Missing map keys mean presence is unknown.
func (c *Client) ConversationPresence(usernames []string) map[string]bool {
	return c.presence.reconcileAndSnapshot(usernames)
}

func (p *presenceTracker) reconcileAndSnapshot(usernames []string) map[string]bool {
	order := make([]string, 0, min(len(usernames), maxPresenceUsers))
	desired := make(map[string]struct{}, cap(order))
	for _, username := range usernames {
		if username == "" {
			continue
		}
		if _, exists := desired[username]; exists {
			continue
		}
		desired[username] = struct{}{}
		order = append(order, username)
		if len(order) == maxPresenceUsers {
			break
		}
	}

	p.mu.Lock()
	changed := !sameUserOrder(p.desiredOrder, order)
	if changed {
		p.desiredOrder = order
		p.desired = desired
		for username := range p.online {
			if _, keep := desired[username]; !keep {
				delete(p.online, username)
			}
		}
	}
	result := make(map[string]bool, len(p.online))
	for _, username := range order {
		if online, known := p.online[username]; known {
			result[username] = online
		}
	}
	p.mu.Unlock()

	if changed {
		p.signalSync()
	}
	return result
}

func sameUserOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (p *presenceTracker) signalSync() {
	select {
	case p.syncNeeded <- struct{}{}:
	default:
	}
}

func (p *presenceTracker) activate(generation uint64) {
	p.mu.Lock()
	p.generation = generation
	clear(p.remote)
	clear(p.online)
	p.mu.Unlock()
	p.signalSync()
}

func (p *presenceTracker) invalidate(generation uint64) {
	p.mu.Lock()
	if p.generation == generation {
		p.generation = 0
		clear(p.remote)
		clear(p.online)
	}
	p.mu.Unlock()
}

func (p *presenceTracker) syncActions(generation uint64) presenceSyncActions {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.generation != generation {
		return presenceSyncActions{}
	}

	actions := presenceSyncActions{}
	for username := range p.remote {
		if _, keep := p.desired[username]; !keep {
			actions.unwatch = append(actions.unwatch, username)
		}
	}
	sort.Strings(actions.unwatch)
	for _, username := range p.desiredOrder {
		if _, watched := p.remote[username]; !watched {
			actions.watch = append(actions.watch, username)
		}
	}
	return actions
}

func (p *presenceTracker) acknowledgeWatch(generation uint64, username string) {
	p.mu.Lock()
	if p.generation != generation {
		p.mu.Unlock()
		return
	}
	p.remote[username] = struct{}{}
	_, stillDesired := p.desired[username]
	p.mu.Unlock()
	if !stillDesired {
		p.signalSync()
	}
}

func (p *presenceTracker) acknowledgeUnwatch(generation uint64, username string) {
	p.mu.Lock()
	if p.generation != generation {
		p.mu.Unlock()
		return
	}
	delete(p.remote, username)
	delete(p.online, username)
	_, desiredAgain := p.desired[username]
	p.mu.Unlock()
	if desiredAgain {
		p.signalSync()
	}
}

func (p *presenceTracker) updateWatch(generation uint64, msg server.WatchUser) {
	if !msg.Exists {
		p.update(generation, msg.Username, msg.Status, false)
		return
	}
	p.update(generation, msg.Username, msg.Status, true)
}

func (p *presenceTracker) updateStatus(generation uint64, username string, status server.UserStatus) {
	p.update(generation, username, status, true)
}

func (p *presenceTracker) update(generation uint64, username string, status server.UserStatus, exists bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.generation != generation {
		return
	}
	if _, wanted := p.desired[username]; !wanted {
		return
	}
	if !exists {
		delete(p.online, username)
		return
	}
	switch status {
	case server.StatusOffline:
		p.online[username] = false
	case server.StatusAway, server.StatusOnline:
		p.online[username] = true
	default:
		delete(p.online, username)
	}
}
