package server

import (
	"sync"
	"sync/atomic"
	"time"
)

const readyzFailureWindow = 10

type ReadinessTracker interface {
	MarkReady()
	MarkUnready(reason string)
	RecordSendSuccess()
	RecordSendFailure(serverError bool)
	IsReady() (bool, string)
	LastCheck() time.Time
}

type readiness struct {
	mu              sync.Mutex
	ready           bool
	reason          string
	consecFails     int
	lastCheck       atomic.Pointer[time.Time]
}

func NewReadiness() ReadinessTracker {
	r := &readiness{reason: "startup: telegram getMe pending"}
	t := time.Time{}
	r.lastCheck.Store(&t)
	return r
}

func (r *readiness) MarkReady() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = true
	r.reason = ""
	r.consecFails = 0
	now := time.Now()
	r.lastCheck.Store(&now)
}

func (r *readiness) MarkUnready(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = false
	r.reason = reason
	now := time.Now()
	r.lastCheck.Store(&now)
}

func (r *readiness) RecordSendSuccess() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consecFails = 0
	if !r.ready {
		r.ready = true
		r.reason = ""
	}
}

func (r *readiness) RecordSendFailure(serverError bool) {
	if !serverError {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consecFails++
	if r.consecFails >= readyzFailureWindow {
		r.ready = false
		r.reason = "telegram api: too many consecutive 5xx errors"
	}
}

func (r *readiness) IsReady() (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready, r.reason
}

func (r *readiness) LastCheck() time.Time {
	if t := r.lastCheck.Load(); t != nil {
		return *t
	}
	return time.Time{}
}
