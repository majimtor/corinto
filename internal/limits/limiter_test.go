package limits_test

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/limits"
)

// clock is a time the test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
}

func TestALimiterGivesTheWholeAllowanceAndThenOneEventAtATime(t *testing.T) {
	moved := newClock()
	limited := limits.NewLimiter(10, time.Minute, 10, moved.read)

	for i := range 10 {
		if wait := limited.Take("a"); wait != 0 {
			t.Fatalf("event %d has to wait %s, want none", i+1, wait)
		}
	}
	if wait := limited.Take("a"); wait != 6*time.Second {
		t.Errorf("event 11 has to wait %s, want 6s", wait)
	}
	if wait := limited.Wait("a"); wait != 6*time.Second {
		t.Errorf("asking again says %s, want 6s: a refusal takes nothing", wait)
	}
	if wait := limited.Take("b"); wait != 0 {
		t.Errorf("another key has to wait %s, want none", wait)
	}

	moved.advance(2 * time.Second)
	if wait := limited.Wait("a"); wait != 4*time.Second {
		t.Errorf("two seconds later the wait is %s, want 4s", wait)
	}
	moved.advance(4 * time.Second)
	if wait := limited.Wait("a"); wait != 0 {
		t.Errorf("six seconds later the wait is %s, want none", wait)
	}
	if wait := limited.Wait("a"); wait != 0 {
		t.Errorf("asking took an event: the wait is now %s", wait)
	}
	if first, second := limited.Take("a"), limited.Take("a"); first != 0 || second != 6*time.Second {
		t.Errorf("one event was earned back, and the two taken waited %s and %s", first, second)
	}

	// After a long time without events the allowance is whole again, and no
	// more than whole.
	moved.advance(10 * time.Minute)
	for i := range 10 {
		if wait := limited.Take("a"); wait != 0 {
			t.Fatalf("after ten minutes, event %d has to wait %s", i+1, wait)
		}
	}
	if wait := limited.Take("a"); wait == 0 {
		t.Error("an idle key was given more than its allowance")
	}
}

// A sustained rate and a larger allowance at once are two numbers: twenty
// a second, and forty after being idle.
func TestALimiterHasARateAndAnAllowanceAtOnce(t *testing.T) {
	moved := newClock()
	limited := limits.NewLimiter(20, time.Second, 40, moved.read)

	for i := range 40 {
		if wait := limited.Take("a"); wait != 0 {
			t.Fatalf("event %d of the burst has to wait %s", i+1, wait)
		}
	}
	if wait := limited.Take("a"); wait != 50*time.Millisecond {
		t.Errorf("event 41 has to wait %s, want 50ms", wait)
	}
	// A second later twenty were earned back, not forty.
	moved.advance(time.Second)
	taken := 0
	for taken < 100 && limited.Take("a") == 0 {
		taken++
	}
	if taken != 20 {
		t.Errorf("a second later %d events fit, want 20", taken)
	}
	// Two seconds later the whole allowance is back, and no more.
	moved.advance(time.Minute)
	taken = 0
	for taken < 100 && limited.Take("a") == 0 {
		taken++
	}
	if taken != 40 {
		t.Errorf("after a minute %d events fit, want 40", taken)
	}
}

// A key can be a client address, or stand for one: it is forgotten as soon
// as remembering it says nothing, never later than its whole allowance
// takes to come back, and forgetting gives back one event at the most.
func TestALimiterForgetsAKeyWithoutGivingAnythingAway(t *testing.T) {
	t.Run("ten a minute", func(t *testing.T) {
		moved := newClock()
		limited := limits.NewLimiter(10, time.Minute, 10, moved.read)
		for range 10 {
			limited.Take("busy")
		}
		// 58 seconds later a sweep finds nine events earned back and the
		// tenth two seconds away: the key is still remembered.
		moved.advance(58 * time.Second)
		limited.Forget(moved.read())
		taken := 0
		for taken < 100 && limited.Take("busy") == 0 {
			taken++
		}
		if taken != 9 {
			t.Errorf("after a sweep at 58 seconds %d events fit, want the 9 that were earned", taken)
		}
		// One second before the allowance is whole the key is dropped, and
		// has everything again: a second of what takes six to earn.
		moved.advance(time.Minute - time.Second)
		limited.Forget(moved.read())
		taken = 0
		for taken < 100 && limited.Take("busy") == 0 {
			taken++
		}
		if taken != 10 {
			t.Errorf("after being forgotten %d events fit, want 10", taken)
		}
	})
	t.Run("twenty a second", func(t *testing.T) {
		moved := newClock()
		limited := limits.NewLimiter(20, time.Second, 40, moved.read)
		for range 40 {
			limited.Take("busy")
		}
		// 1.2 seconds later twenty-four were earned back, and the whole
		// allowance is less than a second away. A sweep that dropped every
		// key about to be whole before the next one would give forty.
		moved.advance(1200 * time.Millisecond)
		limited.Forget(moved.read())
		taken := 0
		for taken < 100 && limited.Take("busy") == 0 {
			taken++
		}
		if taken != 24 {
			t.Errorf("after a sweep at 1.2 seconds %d events fit, want the 24 that were earned", taken)
		}
	})
}

// Past the bound a new key is not limited, and the ones already remembered
// still are: the memory of the process comes first.
func TestALimiterRemembersABoundedNumberOfKeys(t *testing.T) {
	moved := newClock()
	limited := limits.NewLimiter(1, time.Minute, 1, moved.read)
	for i := range 100_000 {
		limited.Take(strconv.Itoa(i))
	}
	for range 3 {
		if wait := limited.Take("one too many"); wait != 0 {
			t.Fatalf("a key past the bound has to wait %s", wait)
		}
	}
	if wait := limited.Take("0"); wait == 0 {
		t.Error("a key that is remembered is no longer limited")
	}
	// Once the others are forgotten there is room, and the key is limited.
	moved.advance(time.Hour)
	limited.Forget(moved.read())
	if first, second := limited.Take("one too many"), limited.Take("one too many"); first != 0 || second == 0 {
		t.Errorf("with room again, the key waited %s and %s, want none and some", first, second)
	}
}
