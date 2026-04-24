package alertmanager

import (
	"testing"
	"time"
)

func TestLabelCache_PutGet(t *testing.T) {
	c := NewLabelCache(time.Hour, 10)
	c.Put("fp1", map[string]string{"a": "1"})
	labels, ok := c.Get("fp1")
	if !ok {
		t.Fatal("expected hit")
	}
	if labels["a"] != "1" {
		t.Errorf("labels: %+v", labels)
	}
}

func TestLabelCache_TTLExpires(t *testing.T) {
	c := NewLabelCache(time.Millisecond, 10)
	// Freeze clock so the test isn't flaky under CI load.
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	c.Put("fp", map[string]string{"a": "1"})
	now = now.Add(time.Second)
	if _, ok := c.Get("fp"); ok {
		t.Error("expected expiry")
	}
}

func TestLabelCache_EvictsOldest(t *testing.T) {
	c := NewLabelCache(time.Hour, 2)
	c.Put("fp1", map[string]string{"a": "1"})
	c.Put("fp2", map[string]string{"b": "2"})
	c.Put("fp3", map[string]string{"c": "3"})

	if _, ok := c.Get("fp1"); ok {
		t.Error("fp1 should have been evicted")
	}
	if _, ok := c.Get("fp2"); !ok {
		t.Error("fp2 should still be present")
	}
	if _, ok := c.Get("fp3"); !ok {
		t.Error("fp3 should be present")
	}
}

func TestLabelCache_UpdatePreservesOrder(t *testing.T) {
	c := NewLabelCache(time.Hour, 2)
	c.Put("fp1", map[string]string{"a": "1"})
	c.Put("fp2", map[string]string{"b": "2"})
	c.Put("fp1", map[string]string{"a": "new"}) // update, not insert
	c.Put("fp3", map[string]string{"c": "3"})

	// Order was fp1, fp2; update keeps fp1 in original slot → eviction removes fp1.
	if _, ok := c.Get("fp1"); ok {
		t.Error("fp1 should have been evicted (oldest by insertion, not by update)")
	}
	if _, ok := c.Get("fp2"); !ok {
		t.Error("fp2 should be present")
	}
	labels, ok := c.Get("fp3")
	if !ok || labels["c"] != "3" {
		t.Errorf("fp3: ok=%v labels=%+v", ok, labels)
	}
}

func TestLabelCache_NilSafe(t *testing.T) {
	var c *LabelCache
	c.Put("fp", map[string]string{"a": "1"})
	if _, ok := c.Get("fp"); ok {
		t.Error("nil cache should return false")
	}
}
