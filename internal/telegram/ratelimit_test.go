package telegram

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPerChatLimit(t *testing.T) {
	l := NewLimiter(100, 1)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := l.Wait(ctx, 42); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 1800*time.Millisecond {
		t.Errorf("expected ≥1.8s for 3 calls at 1 rps, got %v", elapsed)
	}
}

func TestChatsAreIsolated(t *testing.T) {
	l := NewLimiter(100, 1)
	ctx := context.Background()

	start := time.Now()
	var wg sync.WaitGroup
	for chat := int64(1); chat <= 5; chat++ {
		wg.Add(1)
		go func(c int64) {
			defer wg.Done()
			_, _ = l.Wait(ctx, c)
		}(chat)
	}
	wg.Wait()
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Errorf("isolated chats should be fast; got %v", elapsed)
	}
}

func TestGlobalCap(t *testing.T) {
	l := NewLimiter(2, 100)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := l.Wait(ctx, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 1500*time.Millisecond {
		t.Errorf("expected ≥1.5s for 5 calls at 2 rps global, got %v", elapsed)
	}
}

func TestContextCancel(t *testing.T) {
	l := NewLimiter(0.1, 0.1)
	_, _ = l.Wait(context.Background(), 1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Wait(ctx, 1); err == nil {
		t.Error("expected ctx error")
	}
}

func TestRaceSafety(t *testing.T) {
	l := NewLimiter(1000, 100)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(c int64) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_, _ = l.Wait(ctx, c%10)
			}
		}(int64(i))
	}
	wg.Wait()
}
