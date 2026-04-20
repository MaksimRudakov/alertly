package telegram

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type Limiter struct {
	global       *rate.Limiter
	perChat      map[int64]*rate.Limiter
	perChatMu    sync.Mutex
	perChatRate  rate.Limit
	perChatBurst int
}

func NewLimiter(globalPerSec, perChatPerSec float64) *Limiter {
	if globalPerSec <= 0 {
		globalPerSec = 30
	}
	if perChatPerSec <= 0 {
		perChatPerSec = 1
	}
	return &Limiter{
		global:       rate.NewLimiter(rate.Limit(globalPerSec), max1(int(globalPerSec))),
		perChat:      make(map[int64]*rate.Limiter),
		perChatRate:  rate.Limit(perChatPerSec),
		perChatBurst: max1(int(perChatPerSec)),
	}
}

func (l *Limiter) Wait(ctx context.Context, chatID int64) (waited time.Duration, err error) {
	start := time.Now()
	if err := l.global.Wait(ctx); err != nil {
		return time.Since(start), err
	}
	if err := l.chatLimiter(chatID).Wait(ctx); err != nil {
		return time.Since(start), err
	}
	return time.Since(start), nil
}

func (l *Limiter) chatLimiter(chatID int64) *rate.Limiter {
	l.perChatMu.Lock()
	defer l.perChatMu.Unlock()
	lim, ok := l.perChat[chatID]
	if !ok {
		lim = rate.NewLimiter(l.perChatRate, l.perChatBurst)
		l.perChat[chatID] = lim
	}
	return lim
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}
