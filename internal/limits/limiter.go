package limits

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

// Limiter allows each key a number of events in a stretch of time, as a
// token bucket: a key that was idle has its whole allowance at once, and
// earns it back one event at a time. Against a fixed window it has no
// instant at which the count starts again, and it says exactly how long to
// wait.
//
// For each key it keeps one instant, the one at which the key will have its
// whole allowance again, and nothing else. A key that is not remembered has
// it already.
type Limiter struct {
	mu sync.Mutex
	// earn is how long it takes to earn one event back.
	earn time.Duration
	// whole is how long it takes to earn the whole allowance back.
	whole time.Duration
	// restored holds, for each key, when its allowance is whole again.
	restored map[string]time.Time
	now      func() time.Time
}

// NewLimiter returns a limiter that lets each key have events in every
// stretch of time, sustained, and allowance of them at once after being
// idle. now is the clock; everything but a test passes time.Now.
func NewLimiter(events int, every time.Duration, allowance int, now func() time.Time) *Limiter {
	earn := every / time.Duration(events)
	return &Limiter{
		earn:     earn,
		whole:    earn * time.Duration(allowance),
		restored: make(map[string]time.Time),
		now:      now,
	}
}

// Wait returns how long a key has to wait for its next event, or zero when
// it can have one now. It takes nothing.
func (l *Limiter) Wait(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.owed(key, l.now())
}

// Take takes one event for a key. It returns zero when there was one, and
// otherwise how long the key has to wait, without taking anything.
func (l *Limiter) Take(key string) time.Duration {
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
// it still has to earn back leaves room for it within its allowance.
func (l *Limiter) owed(key string, now time.Time) time.Duration {
	restored, remembered := l.restored[key]
	if !remembered {
		// Not the same as a zero instant, which is so far back that the
		// difference below would not fit in a duration.
		return 0
	}
	return max(0, restored.Sub(now)-(l.whole-l.earn))
}

// Forget drops the keys that have their whole allowance again, or will
// have it before the next sweep: remembering them says nothing, and a key
// can be a client address or stand for one. A key is dropped early by no
// more than the time it takes to earn one event, so forgetting gives back
// one event at the most.
func (l *Limiter) Forget(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	early := min(sweepEvery, l.earn)
	for key, restored := range l.restored {
		if !restored.After(now.Add(early)) {
			delete(l.restored, key)
		}
	}
}

// Sweep forgets keys once a second until ctx is done.
func (l *Limiter) Sweep(ctx context.Context) {
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.Forget(l.now())
		}
	}
}
