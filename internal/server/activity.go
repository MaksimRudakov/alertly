package server

import (
	"sync"
	"time"
)

// ActivityTracker records the last successfully parsed webhook and the last
// successful Telegram delivery. /status uses it to distinguish "genuinely
// quiet" from "Alertmanager stopped calling us": a healthy AM holding firing
// alerts combined with an old last-webhook timestamp points at broken routing.
type ActivityTracker struct {
	mu            sync.Mutex
	lastWebhook   time.Time
	webhookSource string
	lastDelivery  time.Time
}

func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{}
}

func (a *ActivityTracker) RecordWebhook(source string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.lastWebhook = time.Now()
	a.webhookSource = source
	a.mu.Unlock()
}

func (a *ActivityTracker) RecordDelivery() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.lastDelivery = time.Now()
	a.mu.Unlock()
}

// LastWebhook returns the time and source of the last parsed webhook; a zero
// time means none since process start.
func (a *ActivityTracker) LastWebhook() (time.Time, string) {
	if a == nil {
		return time.Time{}, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastWebhook, a.webhookSource
}

// LastDelivery returns the time of the last successful Telegram send; a zero
// time means none since process start.
func (a *ActivityTracker) LastDelivery() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastDelivery
}
