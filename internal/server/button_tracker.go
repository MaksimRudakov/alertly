package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/MaksimRudakov/alertly/internal/telegram"
)

// buttonKey identifies a message whose inline keyboard is under management.
type buttonKey struct {
	ChatID    int64
	MessageID int64
}

type buttonEntry struct {
	Fingerprint string
	ExpiresAt   time.Time
}

// ButtonTracker tracks alert messages that carry silence buttons and expires
// them after ButtonTTL. Tracker state is in-memory; an alertly restart causes
// orphaned buttons to remain on screen, but callbacks for them will be
// rejected because the tracker will not recognise them (strict policy).
type ButtonTracker struct {
	mu      sync.Mutex
	entries map[buttonKey]buttonEntry
	ttl     time.Duration
	now     func() time.Time
}

func NewButtonTracker(ttl time.Duration) *ButtonTracker {
	return &ButtonTracker{
		entries: make(map[buttonKey]buttonEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// Register records a sent message whose keyboard should live for TTL.
func (t *ButtonTracker) Register(chatID, messageID int64, fingerprint string) {
	if t == nil || messageID == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[buttonKey{ChatID: chatID, MessageID: messageID}] = buttonEntry{
		Fingerprint: fingerprint,
		ExpiresAt:   t.now().Add(t.ttl),
	}
}

// Valid reports whether a message is still within its silence window.
// Returns false when the tracker is nil, the entry is missing (restart case),
// or the entry has expired.
func (t *ButtonTracker) Valid(chatID, messageID int64) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[buttonKey{ChatID: chatID, MessageID: messageID}]
	if !ok {
		return false
	}
	return t.now().Before(entry.ExpiresAt)
}

// Consume removes an entry (typically called after a successful silence so the
// sweeper does not re-edit an already-stripped message). Safe to call on
// missing entries.
func (t *ButtonTracker) Consume(chatID, messageID int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, buttonKey{ChatID: chatID, MessageID: messageID})
}

// ExpiredEntry is returned from Sweep so callers can edit the corresponding
// messages.
type ExpiredEntry struct {
	ChatID    int64
	MessageID int64
}

// Sweep pops all entries whose TTL has elapsed and returns them. Callers
// typically then call EditMessageReplyMarkup(nil) for each.
func (t *ButtonTracker) Sweep() []ExpiredEntry {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var expired []ExpiredEntry
	for k, e := range t.entries {
		if now.Before(e.ExpiresAt) {
			continue
		}
		expired = append(expired, ExpiredEntry(k))
		delete(t.entries, k)
	}
	return expired
}

// Len returns the current number of tracked messages (for metrics/debug).
func (t *ButtonTracker) Len() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// ButtonSweeper runs Sweep on a ticker and calls EditMessageReplyMarkup(nil)
// for each expired message.
type ButtonSweeper struct {
	Tracker  *ButtonTracker
	Telegram telegram.Client
	Logger   *slog.Logger
	Interval time.Duration
}

func (s *ButtonSweeper) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	s.Logger.Info("button sweeper started", "interval", interval)
	defer s.Logger.Info("button sweeper stopped")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *ButtonSweeper) sweepOnce(ctx context.Context) {
	expired := s.Tracker.Sweep()
	if len(expired) == 0 {
		return
	}
	for _, e := range expired {
		// Per-edit timeout so a stuck call does not stall the whole sweep.
		ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := s.Telegram.EditMessageReplyMarkup(ectx, e.ChatID, e.MessageID, nil); err != nil {
			// Already-edited / message-not-found → log and move on; we already
			// dropped the entry from the tracker, so no retry loop develops.
			s.Logger.Warn("sweeper: edit reply markup failed",
				"chat_id", e.ChatID, "message_id", e.MessageID, "err", err)
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
	}
	s.Logger.Debug("button sweeper pass", "expired", len(expired))
}
