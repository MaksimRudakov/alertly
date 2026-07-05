package alertmanager

import (
	"container/list"
	"sync"
	"time"
)

// LabelCache stores a bounded, time-limited mapping fingerprint -> labels.
// It is populated when alertly forwards a notification to Telegram and is
// consulted by the callback handler as a fallback when Alertmanager does not
// return the alert (e.g., already resolved/expired). Eviction is FIFO by
// insertion order once MaxEntries is reached; removal and eviction are O(1)
// via a linked list of insertion order.
type LabelCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	order      *list.List // front = oldest inserted
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
}

type cacheEntry struct {
	fingerprint string
	labels      map[string]string
	expiresAt   time.Time
}

func NewLabelCache(ttl time.Duration, maxEntries int) *LabelCache {
	return &LabelCache{
		entries:    make(map[string]*list.Element),
		order:      list.New(),
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

	if el, exists := c.entries[fingerprint]; exists {
		entry := el.Value.(*cacheEntry)
		entry.labels = copied
		entry.expiresAt = c.now().Add(c.ttl)
		return
	}
	el := c.order.PushBack(&cacheEntry{
		fingerprint: fingerprint,
		labels:      copied,
		expiresAt:   c.now().Add(c.ttl),
	})
	c.entries[fingerprint] = el
	for c.order.Len() > c.maxEntries {
		oldest := c.order.Front()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).fingerprint)
	}
}

func (c *LabelCache) Get(fingerprint string) (map[string]string, bool) {
	if c == nil || fingerprint == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.entries[fingerprint]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*cacheEntry)
	if c.now().After(entry.expiresAt) {
		c.order.Remove(el)
		delete(c.entries, fingerprint)
		return nil, false
	}
	out := make(map[string]string, len(entry.labels))
	for k, v := range entry.labels {
		out[k] = v
	}
	return out, true
}

// Len returns the current number of cached fingerprints (including entries
// whose TTL has elapsed but which have not been touched since).
func (c *LabelCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
