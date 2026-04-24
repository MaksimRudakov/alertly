package alertmanager

import (
	"sync"
	"time"
)

// LabelCache stores a bounded, time-limited mapping fingerprint -> labels.
// It is populated when alertly forwards a notification to Telegram and is
// consulted by the callback handler as a fallback when Alertmanager does not
// return the alert (e.g., already resolved/expired). Eviction is FIFO by
// insertion order once MaxEntries is reached.
type LabelCache struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	order      []string
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
}

type cacheEntry struct {
	labels    map[string]string
	expiresAt time.Time
}

func NewLabelCache(ttl time.Duration, maxEntries int) *LabelCache {
	return &LabelCache{
		entries:    make(map[string]cacheEntry),
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
	}
}

func (c *LabelCache) Put(fingerprint string, labels map[string]string) {
	if c == nil || fingerprint == "" || len(labels) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	copied := make(map[string]string, len(labels))
	for k, v := range labels {
		copied[k] = v
	}

	if _, exists := c.entries[fingerprint]; !exists {
		c.order = append(c.order, fingerprint)
		c.evictLocked()
	}
	c.entries[fingerprint] = cacheEntry{
		labels:    copied,
		expiresAt: c.now().Add(c.ttl),
	}
}

func (c *LabelCache) Get(fingerprint string) (map[string]string, bool) {
	if c == nil || fingerprint == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[fingerprint]
	if !ok {
		return nil, false
	}
	if c.now().After(entry.expiresAt) {
		c.removeLocked(fingerprint)
		return nil, false
	}
	out := make(map[string]string, len(entry.labels))
	for k, v := range entry.labels {
		out[k] = v
	}
	return out, true
}

func (c *LabelCache) evictLocked() {
	for len(c.order) > c.maxEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

func (c *LabelCache) removeLocked(fingerprint string) {
	delete(c.entries, fingerprint)
	for i, fp := range c.order {
		if fp == fingerprint {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}
