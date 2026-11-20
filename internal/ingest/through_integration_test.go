//go:build integration

package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/postgres"
)

// accepted returns the moment the inbox says it accepted an event at.
func (s *stage) accepted(t *testing.T, event Event) time.Time {
	t.Helper()
	var at time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT accepted_at FROM event_inbox WHERE id = $1::uuid`, event.ID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// through asks how far the events are processed.
func (s *stage) through(t *testing.T) time.Time {
	t.Helper()
	through, err := s.store.ProcessedThrough(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if through.Location() != time.UTC {
		t.Errorf("the moment is in %s, want it in UTC", through.Location())
	}
	return through
}

// Statement 7, where it is read: the acceptance of the oldest event that
// still waits, or the moment of the question when none does.
func TestProcessedThroughIsTheAcceptanceOfTheOldestEventThatWaits(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC-8", -8*60*60)
	t.Cleanup(func() { time.Local = local })
	s := newStage(t)
	now := func() time.Time {
		t.Helper()
		var at time.Time
		if err := s.pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	current := func(what string) {
		t.Helper()
		before := now()
		if through, after := s.through(t), now(); through.Before(before) || through.After(after) {
			t.Errorf("%s: the events are processed through %s, want the moment of the question, between %s and %s", what, through, before, after)
		}
	}

	current("with an empty inbox")
	// What was processed holds nothing back, however old it is.
	s.aged(t, 3*time.Hour, true, 2)
	current("with only processed events")

	// Of the events that wait, the one accepted first, which is not the
	// one written first.
	s.aged(t, 10*time.Minute, false, 1)
	oldest := s.aged(t, 30*time.Minute, false, 1)[0]
	next := s.aged(t, 20*time.Minute, false, 1)[0]
	if through, want := s.through(t), s.accepted(t, oldest); !through.Equal(want) {
		t.Fatalf("the events are processed through %s, want the acceptance of the oldest that waits, %s", through, want)
	}

	// An event that failed and waits for its next attempt still waits.
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE event_inbox SET attempts = 4, next_attempt_at = now() + interval '2 minutes' WHERE id = $1::uuid`, oldest.ID); err != nil {
		t.Fatal(err)
	}
	if through, want := s.through(t), s.accepted(t, oldest); !through.Equal(want) {
		t.Errorf("with the oldest event waiting for an attempt, the events are processed through %s, want %s", through, want)
	}

	// Set aside after its last attempt, it holds nothing back any longer:
	// the moment is the acceptance of the next one.
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if dead, err := Fail(t.Context(), tx, oldest, postgres.Refusal{Code: "23514", Constraint: "a_test"}); err != nil || !dead {
		t.Fatalf("Fail = %v, %v; want the event among the dead letters", dead, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if through, want := s.through(t), s.accepted(t, next); !through.Equal(want) {
		t.Errorf("with the oldest event dead-lettered, the events are processed through %s, want the acceptance of the next, %s", through, want)
	}

	// With every event processed, the moment of the question again.
	if _, err := s.pool.Exec(t.Context(), `UPDATE event_inbox SET processed_at = now() WHERE processed_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	current("with every event processed")
}

// The oldest event that waits is the first entry of the index of the
// events that wait: the question costs the same however long the inbox is.
func TestTheOldestEventThatWaitsIsFoundThroughTheIndex(t *testing.T) {
	s := newStage(t)
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := tx.Exec(t.Context(), `SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off`); err != nil {
		t.Fatal(err)
	}
	var plan string
	err = tx.QueryRow(t.Context(), `EXPLAIN (FORMAT JSON) `+oldestPending).Scan(&plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, `"Index Name": "event_inbox_pending_idx"`) || strings.Contains(plan, `"Node Type": "Sort"`) {
		t.Errorf("the oldest pending event is not read through event_inbox_pending_idx: %s", plan)
	}
}
