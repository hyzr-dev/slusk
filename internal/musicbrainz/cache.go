package musicbrainz

import (
	"sync"
	"time"
)

// ttlCache is a small in-memory, TTL-expiring cache keyed on the full request
// URL, safe for concurrent use. There is no eviction beyond lazy expiry on
// read: MusicBrainz identify traffic is a handful of explicit calls per user
// action (issue #321 replaced an autocomplete design specifically to keep
// this bounded), so an unbounded map never grows large enough to matter.
type ttlCache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	body    []byte
	expires time.Time
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{ttl: ttl, entries: make(map[string]cacheEntry)}
}

// get returns a cached body for key, false if absent or expired.
func (c *ttlCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.body, true
}

// set stores body for key with the cache's configured TTL.
func (c *ttlCache) set(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{body: body, expires: time.Now().Add(c.ttl)}
}
