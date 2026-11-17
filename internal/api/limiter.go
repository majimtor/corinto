package api

import (
	"context"
	"sync"
	"time"
)

const (
	// sweepEvery is how often a limiter forgets the keys that have their
	// whole allowance again.
	sweepEvery = time.Second
	// maxLimited bounds the keys a limiter remembers at once, so that a
	// flood of them cannot take the memory of the process. Past it, new
	// keys are not limited until others are forgotten: the bound protects
	// the process before it protects the limit.
	maxLimited = 100_000
)

// limiter allows each key a number of events in a minute, as a token
// bucket: a key that was idle has the whole allowance at once, and earns it
// back one event at a time. Against a fixed window it has no instant at
// which the count starts again, and it says exactly how long to wait.
//
// For each key it keeps one instant, the one at which the key will have its
// whole allowance again, and nothing else. A key that is not remembered has
// it already.
type limiter struct {
	mu sync.Mutex
	// earn is how long it takes to earn one event back: the minute divided
	// by the allowance.
	earn time.Duration
	// restored holds, for each key, when its allowance is whole again.
	restored map[string]time.Time
	now      func() time.Time
}

// period is how long it takes to earn the whole allowance back. Every limit
// of the API is a number of events in a minute.
const period = time.Minute

// newLimiter returns a limiter that allows each key a number of events in a
// minute.
func newLimiter(perMinute int, now func() time.Time) *limiter {
	return &limiter{
		earn:     period / time.Duration(perMinute),
		restored: make(map[string]time.Time),
		now:      now,
	}
}

// wait returns how long a key has to wait for its next event, or zero when
// it can have one now. It takes nothing.
func (l *limiter) wait(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.owed(key, l.now())
}

// take takes one event for a key. It returns zero when there was one, and
// otherwise how long the key has to wait, without taking anything.
func (l *limiter) take(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if owed := l.owed(key, now); owed > 0 {
		return owed
	}
	restored, remembered := l.restored[key]
	if !remembered && len(l.restored) >= maxLimited {
		return 0
	}
	if restored.Before(now) {
		restored = now
	}
	l.restored[key] = restored.Add(l.earn)
	return 0
}

// owed is how long a key is from having one event. A key has one when what
// it still has to earn back leaves room for it within the period.
func (l *limiter) owed(key string, now time.Time) time.Duration {
	restored, remembered := l.restored[key]
	if !remembered {
		// Not the same as a zero instant, which is so far back that the
		// difference below would not fit in a duration.
		return 0
	}
	return max(0, restored.Sub(now)-(period-l.earn))
}

// forget drops the keys that have their whole allowance again, or will
// before the next sweep: remembering them says nothing, and a key is a
// client address or stands for one. Dropping a key up to one sweep early
// gives it back a fraction of one event.
func (l *limiter) forget(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, restored := range l.restored {
		if !restored.After(now.Add(sweepEvery)) {
			delete(l.restored, key)
		}
	}
}

// sweep forgets keys once a second until ctx is done.
func (l *limiter) sweep(ctx context.Context) {
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.forget(l.now())
		}
	}
}
