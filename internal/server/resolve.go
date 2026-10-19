package server

import (
	"container/list"
	"strconv"
	"sync"
	"time"

	"github.com/MaksimRudakov/alertly/internal/sink"
)

// maxMessagesPerAlert caps how many firing messages of one incident are
// remembered: Alertmanager re-sends a long-firing alert every
// repeat_interval, and each repeat lands as a new message (dedup TTL is
// shorter). The resolve rewrites all of them and replies to the newest.
const maxMessagesPerAlert = 5

// FiringMessage is one delivered firing notification.
type FiringMessage struct {
	// First is the first part: the reply target, rewritten as resolved.
	First sink.MessageRef
	// Last is the part carrying the buttons; LastPart is its body as sent
	// (Slack needs it to drop the buttons). Equal to First for one-part
	// messages.
	Last     sink.MessageRef
	LastPart sink.Part
}

type messageLogEntry struct {
	key       string
	msgs      []FiringMessage
	expiresAt time.Time
}

// MessageLog links an alert incident in a chat to its firing messages so the
// resolve can answer or rewrite them. In memory, TTL + FIFO bounded: the
// order list is expiry order because every Add refreshes the TTL and moves
// the entry to the back. A miss (restart, eviction, TTL) makes the resolve a
// new message, exactly as without the log.
type MessageLog struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	order      *list.List
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
}

func NewMessageLog(ttl time.Duration, maxEntries int) *MessageLog {
	return &MessageLog{
		entries:    map[string]*list.Element{},
		order:      list.New(),
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
	}
}

// MessageLogKey identifies one incident (fingerprint + start time) of a
// cluster in one chat/thread.
func MessageLogKey(cluster, fingerprint string, startsAt time.Time, target sink.Target) string {
	return cluster + "|" + fingerprint + "|" + strconv.FormatInt(startsAt.Unix(), 10) + "|" + target.String()
}

func (l *MessageLog) Add(key string, m FiringMessage) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneLocked(now)
	if el, ok := l.entries[key]; ok {
		e := el.Value.(*messageLogEntry)
		e.msgs = append(e.msgs, m)
		if len(e.msgs) > maxMessagesPerAlert {
			e.msgs = e.msgs[len(e.msgs)-maxMessagesPerAlert:]
		}
		e.expiresAt = now.Add(l.ttl)
		l.order.MoveToBack(el)
		return
	}
	l.entries[key] = l.order.PushBack(&messageLogEntry{key: key, msgs: []FiringMessage{m}, expiresAt: now.Add(l.ttl)})
	for l.maxEntries > 0 && l.order.Len() > l.maxEntries {
		l.removeLocked(l.order.Front())
	}
}

// Get returns the firing messages of a live entry, oldest first.
func (l *MessageLog) Get(key string) ([]FiringMessage, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.entries[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*messageLogEntry)
	if !l.now().Before(e.expiresAt) {
		l.removeLocked(el)
		return nil, false
	}
	return append([]FiringMessage(nil), e.msgs...), true
}

func (l *MessageLog) Delete(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.entries[key]; ok {
		l.removeLocked(el)
	}
}

func (l *MessageLog) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *MessageLog) pruneLocked(now time.Time) {
	for front := l.order.Front(); front != nil; front = l.order.Front() {
		if now.Before(front.Value.(*messageLogEntry).expiresAt) {
			return
		}
		l.removeLocked(front)
	}
}

func (l *MessageLog) removeLocked(el *list.Element) {
	l.order.Remove(el)
	delete(l.entries, el.Value.(*messageLogEntry).key)
}
