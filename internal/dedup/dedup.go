// Package dedup provides an in-memory TTL cache of recently delivered
// notifications, keyed by (fingerprint, chat target, status). Purpose: when an
// upstream caller (Alertmanager) retries a webhook that alertly already
// delivered to Telegram, suppress the second send. Cache is per-process; a pod
// restart re-opens the dedup window — accepted trade-off.
package dedup

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Cache struct {
	ttl   time.Duration
	mu    sync.Mutex
	items map[string]time.Time
	now   func() time.Time
}

func New(ttl time.Duration) *Cache {
	return &Cache{
		ttl:   ttl,
		items: make(map[string]time.Time),
		now:   time.Now,
	}
}

// TTL returns the configured retention window.
func (c *Cache) TTL() time.Duration {
	if c == nil {
		return 0
	}
	return c.ttl
}

// Key builds a stable cache key out of immutable notification + target + status
// components. Returns "" if fingerprint is empty (caller should skip dedup).
func Key(fingerprint string, chatID int64, threadID *int, status string) string {
	if fingerprint == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(fingerprint) + len(status) + 32)
	b.WriteString(fingerprint)
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(chatID, 10))
	b.WriteByte('|')
	if threadID != nil {
		b.WriteString(strconv.Itoa(*threadID))
	}
	b.WriteByte('|')
	b.WriteString(status)
	return b.String()
}

// Reserve atomically checks the key and, if not present within the TTL window,
// records it and returns false (caller may proceed to send). If already
// present, returns true (caller should skip). Callers that fail to deliver
// after a successful Reserve must call Forget so retries are not suppressed.
func (c *Cache) Reserve(key string) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.items[key]; ok && c.now().Sub(t) <= c.ttl {
		return true
	}
	c.items[key] = c.now()
	return false
}

// Forget removes a key. Used to roll back a Reserve when the send failed.
func (c *Cache) Forget(key string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// Sweep removes expired entries. Safe to call concurrently with Reserve/Forget.
func (c *Cache) Sweep() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, t := range c.items {
		if now.Sub(t) > c.ttl {
			delete(c.items, k)
		}
	}
}

// Len reports the current number of entries (including possibly expired ones
// that have not yet been swept).
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Run periodically calls Sweep until ctx is canceled. Interval defaults to
// min(ttl/2, 5m) when <= 0.
func (c *Cache) Run(ctx context.Context, interval time.Duration) {
	if c == nil {
		return
	}
	if interval <= 0 {
		interval = c.ttl / 2
		if interval <= 0 {
			return
		}
		if interval > 5*time.Minute {
			interval = 5 * time.Minute
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Sweep()
		}
	}
}
