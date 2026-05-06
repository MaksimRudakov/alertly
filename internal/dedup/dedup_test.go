package dedup

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestKey(t *testing.T) {
	thread := 42
	tests := []struct {
		name string
		fp   string
		chat int64
		thr  *int
		st   string
		want string
	}{
		{"basic", "fp1", -1001, nil, "firing", "fp1|-1001||firing"},
		{"with_thread", "fp1", -1001, &thread, "firing", "fp1|-1001|42|firing"},
		{"resolved", "fp1", -1001, nil, "resolved", "fp1|-1001||resolved"},
		{"empty_fingerprint", "", -1001, nil, "firing", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Key(tt.fp, tt.chat, tt.thr, tt.st); got != tt.want {
				t.Errorf("Key=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestReserveTwiceWithinTTL(t *testing.T) {
	c := New(time.Hour)
	if c.Reserve("k1") {
		t.Fatal("first Reserve must return false (not seen)")
	}
	if !c.Reserve("k1") {
		t.Fatal("second Reserve must return true (seen)")
	}
	if c.Reserve("k2") {
		t.Fatal("different key must return false")
	}
}

func TestReserveAfterTTLExpiry(t *testing.T) {
	c := New(50 * time.Millisecond)
	now := time.Now()
	c.now = func() time.Time { return now }

	if c.Reserve("k1") {
		t.Fatal("first Reserve must return false")
	}
	c.now = func() time.Time { return now.Add(100 * time.Millisecond) }
	if c.Reserve("k1") {
		t.Fatal("after TTL, Reserve must treat key as fresh and return false")
	}
}

func TestForgetReleasesKey(t *testing.T) {
	c := New(time.Hour)
	if c.Reserve("k1") {
		t.Fatal("first Reserve must return false")
	}
	c.Forget("k1")
	if c.Reserve("k1") {
		t.Fatal("after Forget, key must be reclaimable")
	}
}

func TestSweepRemovesExpired(t *testing.T) {
	c := New(50 * time.Millisecond)
	now := time.Now()
	c.now = func() time.Time { return now }

	c.Reserve("a")
	c.Reserve("b")
	if c.Len() != 2 {
		t.Fatalf("len before sweep: %d", c.Len())
	}

	c.now = func() time.Time { return now.Add(200 * time.Millisecond) }
	c.Sweep()
	if c.Len() != 0 {
		t.Fatalf("len after sweep: %d", c.Len())
	}
}

func TestNilCacheIsNoop(t *testing.T) {
	var c *Cache
	if c.Reserve("k") {
		t.Error("nil Reserve must return false")
	}
	c.Forget("k")
	c.Sweep()
	if c.Len() != 0 {
		t.Error("nil Len must be 0")
	}
	if c.TTL() != 0 {
		t.Error("nil TTL must be 0")
	}
}

func TestEmptyKeyIsNotCached(t *testing.T) {
	c := New(time.Hour)
	if c.Reserve("") {
		t.Error("empty key must always return false")
	}
	if c.Reserve("") {
		t.Error("empty key must remain non-cached")
	}
	if c.Len() != 0 {
		t.Error("empty key must not be stored")
	}
}

func TestReserveConcurrent(t *testing.T) {
	c := New(time.Hour)
	const n = 100
	var (
		wg      sync.WaitGroup
		seenCnt int
		mu      sync.Mutex
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.Reserve("same") {
				mu.Lock()
				seenCnt++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if seenCnt != n-1 {
		t.Errorf("expected exactly 1 winner, got seen=%d (winners=%d)", seenCnt, n-seenCnt)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	c := New(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx, 5*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after ctx cancel")
	}
}
