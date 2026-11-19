package api

import (
	"strconv"
	"testing"
	"time"
)

func TestALimiterGivesTheWholeAllowanceAndThenOneEventAtATime(t *testing.T) {
	moved := &clock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	limited := newLimiter(10, moved.read)

	for i := range 10 {
		if wait := limited.take("a"); wait != 0 {
			t.Fatalf("event %d has to wait %s, want none", i+1, wait)
		}
	}
	if wait := limited.take("a"); wait != 6*time.Second {
		t.Errorf("event 11 has to wait %s, want 6s", wait)
	}
	if wait := limited.wait("a"); wait != 6*time.Second {
		t.Errorf("asking again says %s, want 6s: a refusal takes nothing", wait)
	}
	if wait := limited.take("b"); wait != 0 {
		t.Errorf("another key has to wait %s, want none", wait)
	}

	moved.advance(2 * time.Second)
	if wait := limited.wait("a"); wait != 4*time.Second {
		t.Errorf("two seconds later the wait is %s, want 4s", wait)
	}
	moved.advance(4 * time.Second)
	if wait := limited.wait("a"); wait != 0 {
		t.Errorf("six seconds later the wait is %s, want none", wait)
	}
	if wait := limited.wait("a"); wait != 0 {
		t.Errorf("asking took an event: the wait is now %s", wait)
	}
	if first, second := limited.take("a"), limited.take("a"); first != 0 || second != 6*time.Second {
		t.Errorf("one event was earned back, and the two taken waited %s and %s", first, second)
	}

	// After a whole period without events the allowance is whole again,
	// and no more than whole.
	moved.advance(10 * time.Minute)
	for i := range 10 {
		if wait := limited.take("a"); wait != 0 {
			t.Fatalf("after ten minutes, event %d has to wait %s", i+1, wait)
		}
	}
	if wait := limited.take("a"); wait == 0 {
		t.Error("an idle key was given more than its allowance")
	}
}

// A key is a client address, or stands for one: it is forgotten as soon as
// remembering it says nothing, and never later than a period after its
// last event.
func TestALimiterForgetsAKeyWithinAPeriodOfItsLastEvent(t *testing.T) {
	moved := &clock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	limited := newLimiter(10, moved.read)
	for range 10 {
		limited.take("busy")
	}
	limited.take("light")

	remembered := func() int {
		limited.mu.Lock()
		defer limited.mu.Unlock()
		return len(limited.restored)
	}
	limited.forget(moved.read())
	if remembered() != 2 {
		t.Fatalf("%d keys remembered right after their events, want 2", remembered())
	}
	// One event is earned back in six seconds.
	moved.advance(5 * time.Second)
	limited.forget(moved.read())
	if remembered() != 1 {
		t.Errorf("%d keys remembered after five seconds, want only the busy one", remembered())
	}
	// The whole allowance, in a minute: by the sweep before it, the key is
	// gone.
	moved.advance(54 * time.Second)
	limited.forget(moved.read())
	if remembered() != 0 {
		t.Errorf("%d keys remembered 59 seconds after the last event, want none", remembered())
	}
	// Forgetting gave nothing away: what was owed was at most one sweep.
	if wait := limited.take("busy"); wait != 0 {
		t.Errorf("a forgotten key has to wait %s", wait)
	}
}

// Past the bound a new key is not limited, and the ones already remembered
// still are: the memory of the process comes first.
func TestALimiterRemembersABoundedNumberOfKeys(t *testing.T) {
	moved := &clock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	limited := newLimiter(1, moved.read)
	for i := range maxLimited {
		limited.take(strconv.Itoa(i))
	}
	for range 3 {
		if wait := limited.take("one too many"); wait != 0 {
			t.Fatalf("a key past the bound has to wait %s", wait)
		}
	}
	if len(limited.restored) != maxLimited {
		t.Errorf("%d keys remembered, want %d", len(limited.restored), maxLimited)
	}
	if wait := limited.take("0"); wait == 0 {
		t.Error("a key that is remembered is no longer limited")
	}
}
