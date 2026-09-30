package slack

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter enforces a global and a per-channel request rate. Slack allows
// about one message per second per channel with short bursts.
type Limiter struct {
	global     *rate.Limiter
	mu         sync.Mutex
	perChannel map[string]*rate.Limiter
	chanRate   rate.Limit
	chanBurst  int
}

func NewLimiter(globalPerSec, perChannelPerSec float64) *Limiter {
	if globalPerSec <= 0 {
		globalPerSec = 20
	}
	if perChannelPerSec <= 0 {
		perChannelPerSec = 1
	}
	return &Limiter{
		global:     rate.NewLimiter(rate.Limit(globalPerSec), max(1, int(globalPerSec))),
		perChannel: map[string]*rate.Limiter{},
		chanRate:   rate.Limit(perChannelPerSec),
		chanBurst:  max(1, int(perChannelPerSec)),
	}
}

func (l *Limiter) Wait(ctx context.Context, channel string) (time.Duration, error) {
	start := time.Now()
	if err := l.global.Wait(ctx); err != nil {
		return time.Since(start), err
	}
	l.mu.Lock()
	lim, ok := l.perChannel[channel]
	if !ok {
		lim = rate.NewLimiter(l.chanRate, l.chanBurst)
		l.perChannel[channel] = lim
	}
	l.mu.Unlock()
	err := lim.Wait(ctx)
	return time.Since(start), err
}
