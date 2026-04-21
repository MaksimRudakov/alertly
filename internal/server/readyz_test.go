package server

import (
	"testing"
	"time"
)

func TestReadinessInitiallyUnready(t *testing.T) {
	r := NewReadiness()
	ready, reason := r.IsReady()
	if ready {
		t.Error("expected initial unready")
	}
	if reason != "startup: telegram getMe pending" {
		t.Errorf("unexpected startup reason: %q", reason)
	}
	if !r.LastCheck().IsZero() {
		t.Error("LastCheck should start zero")
	}
}

func TestReadinessMarkReadyClearsReason(t *testing.T) {
	r := NewReadiness()
	before := time.Now()
	r.MarkReady()
	ready, reason := r.IsReady()
	if !ready {
		t.Error("MarkReady did not flip ready")
	}
	if reason != "" {
		t.Errorf("reason not cleared: %q", reason)
	}
	if r.LastCheck().Before(before) {
		t.Errorf("LastCheck not updated by MarkReady")
	}
}

func TestReadinessMarkUnreadySetsReason(t *testing.T) {
	r := NewReadiness()
	r.MarkReady()
	r.MarkUnready("custom: something is wrong")
	ready, reason := r.IsReady()
	if ready {
		t.Error("MarkUnready did not flip unready")
	}
	if reason != "custom: something is wrong" {
		t.Errorf("reason: %q", reason)
	}
}

func TestReadinessRecordSendSuccessReArmsReady(t *testing.T) {
	r := NewReadiness()
	// Simulate a runtime failure-induced unready, then a success.
	for i := 0; i < readyzFailureWindow; i++ {
		r.RecordSendFailure(true)
	}
	if ready, _ := r.IsReady(); ready {
		t.Fatal("should be unready after failure window")
	}
	r.RecordSendSuccess()
	if ready, reason := r.IsReady(); !ready || reason != "" {
		t.Errorf("RecordSendSuccess did not re-arm: ready=%v reason=%q", ready, reason)
	}
}

func TestReadinessRecordSendFailureIgnoresClientErrors(t *testing.T) {
	r := NewReadiness()
	r.MarkReady()
	// 4xx send failures (serverError=false) must NOT degrade readiness.
	for i := 0; i < 100; i++ {
		r.RecordSendFailure(false)
	}
	if ready, _ := r.IsReady(); !ready {
		t.Error("4xx failures should not flip readiness")
	}
}

func TestReadinessRecordSendFailureTripsAfterWindow(t *testing.T) {
	r := NewReadiness()
	r.MarkReady()
	for i := 0; i < readyzFailureWindow-1; i++ {
		r.RecordSendFailure(true)
	}
	if ready, _ := r.IsReady(); !ready {
		t.Fatal("should still be ready just below threshold")
	}
	r.RecordSendFailure(true)
	ready, reason := r.IsReady()
	if ready {
		t.Error("should be unready after threshold")
	}
	if reason == "" {
		t.Error("reason should be set after threshold")
	}
}

func TestReadinessSendSuccessResetsCounter(t *testing.T) {
	r := NewReadiness()
	r.MarkReady()
	for i := 0; i < readyzFailureWindow-1; i++ {
		r.RecordSendFailure(true)
	}
	r.RecordSendSuccess()
	// After a success reset, another (window-1) failures should still leave us ready.
	for i := 0; i < readyzFailureWindow-1; i++ {
		r.RecordSendFailure(true)
	}
	if ready, _ := r.IsReady(); !ready {
		t.Error("counter was not reset by RecordSendSuccess")
	}
}
