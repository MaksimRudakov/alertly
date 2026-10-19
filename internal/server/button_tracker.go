package server

import (
	"container/list"
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/sink"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

// buttonKey identifies a message whose buttons are under management, in any
// messenger (Telegram chat_id/message_id, Slack channel/ts).
type buttonKey struct {
	Sink string
	Chat string
	ID   string
}

func keyOf(ref sink.MessageRef) buttonKey {
	return buttonKey{Sink: ref.Sink, Chat: ref.Chat, ID: ref.ID}
}

func telegramRef(chatID, messageID int64) sink.MessageRef {
	return sink.MessageRef{Sink: sink.Telegram, Chat: strconv.FormatInt(chatID, 10), ID: strconv.FormatInt(messageID, 10)}
}

// ButtonEntry is what the tracker knows about one message with buttons.
type ButtonEntry struct {
	Ref     sink.MessageRef
	Cluster string
	// Value is the alert fingerprint (silence tracker) or the silence ID
	// (undo tracker).
	Value string
	// Part is the message as sent, for messengers that resend the whole body
	// to change its buttons (Slack). Empty for Telegram.
	Part      sink.Part
	ExpiresAt time.Time
}

// ButtonTracker tracks alert messages that carry buttons and expires them
// after its TTL. Entries are additionally bounded by maxEntries with FIFO
// eviction (all entries share one TTL, so insertion order is expiry order);
// an evicted message behaves exactly like a restart case — the button stays
// on screen but a click is rejected and the buttons removed (strict policy).
// State is in-memory; after a restart old buttons stay visible but are
// rejected because the tracker does not know them.
type ButtonTracker struct {
	mu      sync.Mutex
	entries map[buttonKey]*list.Element
	order   *list.List // front = oldest inserted = first to expire
	ttl     time.Duration
	// maxEntries bounds the tracker; <= 0 means unbounded.
	maxEntries int
	now        func() time.Time
}

func NewButtonTracker(ttl time.Duration, maxEntries int) *ButtonTracker {
	return &ButtonTracker{
		entries:    make(map[buttonKey]*list.Element),
		order:      list.New(),
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
	}
}

// Register records a Telegram message of the default cluster.
func (t *ButtonTracker) Register(chatID, messageID int64, fingerprint string) {
	t.RegisterFor(chatID, messageID, config.DefaultCluster, fingerprint)
}

// RegisterFor records a Telegram message of a given cluster.
func (t *ButtonTracker) RegisterFor(chatID, messageID int64, cluster, fingerprint string) {
	if messageID == 0 {
		return
	}
	t.RegisterMessage(telegramRef(chatID, messageID), cluster, fingerprint, sink.Part{})
}

// RegisterRef records a delivered message (ButtonRegistrar).
func (t *ButtonTracker) RegisterRef(cluster string, ref sink.MessageRef, fingerprint string, part sink.Part) {
	t.RegisterMessage(ref, cluster, fingerprint, part)
}

// RegisterMessage records a message whose buttons should live for TTL. When
// the tracker is full the oldest entry is evicted (its buttons stay on
// screen; the sweeper never sees it, and a late click gets the strict expired
// path).
func (t *ButtonTracker) RegisterMessage(ref sink.MessageRef, cluster, value string, part sink.Part) {
	if t == nil || ref.ID == "" || ref.ID == "0" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := keyOf(ref)
	if el, ok := t.entries[key]; ok {
		entry := el.Value.(*ButtonEntry)
		entry.Cluster = cluster
		entry.Value = value
		entry.Part = part
		entry.ExpiresAt = t.now().Add(t.ttl)
		t.order.MoveToBack(el)
		return
	}
	t.entries[key] = t.order.PushBack(&ButtonEntry{
		Ref:       ref,
		Cluster:   cluster,
		Value:     value,
		Part:      part,
		ExpiresAt: t.now().Add(t.ttl),
	})
	if t.maxEntries > 0 {
		for t.order.Len() > t.maxEntries {
			oldest := t.order.Front()
			t.order.Remove(oldest)
			delete(t.entries, keyOf(oldest.Value.(*ButtonEntry).Ref))
		}
	}
}

// Valid reports whether a Telegram message is still within its window.
func (t *ButtonTracker) Valid(chatID, messageID int64) bool {
	_, ok := t.Entry(telegramRef(chatID, messageID))
	return ok
}

// Lookup returns the value recorded for a live Telegram message.
func (t *ButtonTracker) Lookup(chatID, messageID int64) (fingerprint string, ok bool) {
	e, ok := t.Entry(telegramRef(chatID, messageID))
	return e.Value, ok
}

// LookupEntry is Lookup plus the cluster the message belongs to.
func (t *ButtonTracker) LookupEntry(chatID, messageID int64) (cluster, fingerprint string, ok bool) {
	e, ok := t.Entry(telegramRef(chatID, messageID))
	return e.Cluster, e.Value, ok
}

// Entry returns the entry of a message still within its window. ok is false
// when the tracker is nil, the message is unknown (restart or eviction) or
// its window elapsed.
func (t *ButtonTracker) Entry(ref sink.MessageRef) (ButtonEntry, bool) {
	if t == nil {
		return ButtonEntry{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.entries[keyOf(ref)]
	if !ok {
		return ButtonEntry{}, false
	}
	e := el.Value.(*ButtonEntry)
	if !t.now().Before(e.ExpiresAt) {
		return ButtonEntry{}, false
	}
	return *e, true
}

// Consume removes a Telegram message entry.
func (t *ButtonTracker) Consume(chatID, messageID int64) {
	t.ConsumeRef(telegramRef(chatID, messageID))
}

// ConsumeRef removes an entry (typically after a successful action, so the
// sweeper does not re-edit an already-updated message). Safe on missing
// entries.
func (t *ButtonTracker) ConsumeRef(ref sink.MessageRef) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := keyOf(ref)
	if el, ok := t.entries[key]; ok {
		t.order.Remove(el)
		delete(t.entries, key)
	}
}

// ExpiredEntry is returned from Sweep so callers can remove the buttons.
// ChatID/MessageID are set for Telegram messages.
type ExpiredEntry struct {
	ChatID    int64
	MessageID int64
	Ref       sink.MessageRef
	Part      sink.Part
}

// Sweep pops all entries whose TTL has elapsed and returns them. Entries
// expire in insertion order, so the walk stops at the first live entry.
func (t *ButtonTracker) Sweep() []ExpiredEntry {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var expired []ExpiredEntry
	for {
		front := t.order.Front()
		if front == nil {
			break
		}
		e := front.Value.(*ButtonEntry)
		if now.Before(e.ExpiresAt) {
			break
		}
		x := ExpiredEntry{Ref: e.Ref, Part: e.Part}
		if e.Ref.Sink == sink.Telegram {
			x.ChatID, _ = strconv.ParseInt(e.Ref.Chat, 10, 64)
			x.MessageID, _ = strconv.ParseInt(e.Ref.ID, 10, 64)
		}
		expired = append(expired, x)
		t.order.Remove(front)
		delete(t.entries, keyOf(e.Ref))
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

// ButtonSweeper runs Sweep on a ticker and removes the buttons of expired
// messages: through the message's sink when Sinks has it, else (Telegram
// only) through Telegram directly.
type ButtonSweeper struct {
	Tracker  *ButtonTracker
	Telegram telegram.Client
	Sinks    map[string]sink.Sink
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
		var err error
		if sk, ok := s.Sinks[e.Ref.Sink]; ok {
			err = sk.SetActions(ectx, e.Ref, e.Part, nil)
		} else if e.Ref.Sink == sink.Telegram && s.Telegram != nil {
			err = s.Telegram.EditMessageReplyMarkup(ectx, e.ChatID, e.MessageID, nil)
		}
		if err != nil {
			// Already-edited / message-not-found → log and move on; the entry
			// is already dropped, so no retry loop develops.
			s.Logger.Warn("sweeper: remove buttons failed",
				"sink", e.Ref.Sink, "chat", e.Ref.Chat, "message", e.Ref.ID, "err", err)
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
	}
	s.Logger.Debug("button sweeper pass", "expired", len(expired))
}
