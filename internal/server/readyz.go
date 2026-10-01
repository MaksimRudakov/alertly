package server

import (
	"sort"
	"strings"
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
	// Touch refreshes LastCheck without changing readiness — called by the
	// health loop on every probe so "last check" reflects probing, not the
	// last readiness transition.
	Touch()
}

type readiness struct {
	mu          sync.Mutex
	ready       bool
	reason      string
	consecFails int
	lastCheck   atomic.Pointer[time.Time]
}

func NewReadiness() ReadinessTracker {
	return NewReadinessWithReason("startup: telegram getMe pending")
}

// NewReadinessWithReason starts unready with the given startup reason (one
// tracker per sink, each naming its own first probe).
func NewReadinessWithReason(reason string) ReadinessTracker {
	r := &readiness{reason: reason}
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
		r.reason = "too many consecutive server errors from the messenger API"
	}
}

func (r *readiness) IsReady() (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready, r.reason
}

func (r *readiness) Touch() {
	now := time.Now()
	r.lastCheck.Store(&now)
}

func (r *readiness) LastCheck() time.Time {
	if t := r.lastCheck.Load(); t != nil {
		return *t
	}
	return time.Time{}
}

// SinkReadiness aggregates the per-sink trackers behind /readyz. The pod is
// ready while ANY sink is ready: one messenger's outage must not pull the pod
// out of the Service and take delivery to the others down with it.
type SinkReadiness struct {
	names    []string
	trackers map[string]ReadinessTracker
}

// SinkState is one sink's readiness as reported by /readyz.
type SinkState struct {
	Name      string
	Ready     bool
	Reason    string
	LastCheck time.Time
}

func NewSinkReadiness(trackers map[string]ReadinessTracker) *SinkReadiness {
	names := make([]string, 0, len(trackers))
	for n := range trackers {
		names = append(names, n)
	}
	sort.Strings(names)
	return &SinkReadiness{names: names, trackers: trackers}
}

// Get returns the tracker of one sink (nil if the sink is not enabled).
func (s *SinkReadiness) Get(name string) ReadinessTracker {
	if s == nil {
		return nil
	}
	return s.trackers[name]
}

func (s *SinkReadiness) States() []SinkState {
	out := make([]SinkState, 0, len(s.names))
	for _, n := range s.names {
		ready, reason := s.trackers[n].IsReady()
		out = append(out, SinkState{Name: n, Ready: ready, Reason: reason, LastCheck: s.trackers[n].LastCheck()})
	}
	return out
}

// IsReady reports ready if any sink is ready; otherwise the reasons of all.
func (s *SinkReadiness) IsReady() (bool, string) {
	var reasons []string
	for _, st := range s.States() {
		if st.Ready {
			return true, ""
		}
		reasons = append(reasons, st.Name+": "+st.Reason)
	}
	return false, strings.Join(reasons, "; ")
}
