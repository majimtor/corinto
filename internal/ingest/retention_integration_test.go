//go:build integration

package ingest

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/majimtor/corinto/internal/postgres"
)

// aged accepts events and makes them as old as a test says: accepted so
// long ago, and processed a second later when they are.
func (s *stage) aged(t *testing.T, ago time.Duration, processed bool, n int) []Event {
	t.Helper()
	events := s.events(noon, n)
	if settled, err := s.store.Accept(t.Context(), events); err != nil || settled != n {
		t.Fatalf("Accept settled %d events with the error %v", settled, err)
	}
	for i, event := range events {
		// Each a millisecond before the one before: the last one written
		// is the oldest, so that the order of their age is not the one
		// they are stored in.
		_, err := s.pool.Exec(t.Context(),
			`UPDATE event_inbox
			    SET accepted_at = $2::timestamptz - make_interval(secs => $3) - make_interval(secs => $4::float8 / 1000),
			        processed_at = CASE WHEN $5 THEN $2::timestamptz - make_interval(secs => $3) + interval '1 second' END
			  WHERE id = $1::uuid`, event.ID, s.clock(t), ago.Seconds(), i, processed)
		if err != nil {
			t.Fatal(err)
		}
	}
	return events
}

// clock reads the clock of the database once per test: what is deleted is
// decided against it.
func (s *stage) clock(t *testing.T) time.Time {
	t.Helper()
	if s.now.IsZero() {
		if err := s.pool.QueryRow(t.Context(), `SELECT now()`).Scan(&s.now); err != nil {
			t.Fatal(err)
		}
	}
	return s.now
}

// here returns which of some events are still in the inbox, by their
// place among them.
func (s *stage) here(t *testing.T, table string, events []Event) []int {
	t.Helper()
	var places []int
	for i, event := range events {
		if s.number(t, `SELECT count(*) FROM `+table+` WHERE id = $1::uuid`, event.ID) == 1 {
			places = append(places, i)
		}
	}
	return places
}

// A processed event is deleted once it was accepted before the moment the
// caller gives, the oldest first and no more than the caller says. An
// event that still waits is never deleted, however old.
func TestProcessedEventsAreDeletedByTheirAgeAndPendingOnesNever(t *testing.T) {
	s := newStage(t)
	const month = 30 * 24 * time.Hour
	old := s.aged(t, month+time.Hour, true, 7)
	recent := s.aged(t, month-time.Hour, true, 2)
	waiting := s.aged(t, 2*month, false, 2)
	// One that waits for its next attempt, and is as old.
	if _, err := s.pool.Exec(t.Context(), `UPDATE event_inbox SET attempts = 3, next_attempt_at = now() WHERE id = $1::uuid`, waiting[1].ID); err != nil {
		t.Fatal(err)
	}
	before := s.clock(t).Add(-month)

	// In batches of three: the three oldest, which are the last three,
	// the next three, and the one that is left.
	for round, want := range []struct {
		deleted int
		left    []int
	}{{3, []int{0, 1, 2, 3}}, {3, []int{0}}, {1, nil}, {0, nil}} {
		deleted, err := s.store.DeleteProcessed(t.Context(), before, 3)
		if err != nil || deleted != want.deleted {
			t.Fatalf("round %d: DeleteProcessed deleted %d events with the error %v, want %d", round+1, deleted, err, want.deleted)
		}
		if left := s.here(t, "event_inbox", old); !slices.Equal(left, want.left) {
			t.Fatalf("round %d: of the old events, %v are left; want %v", round+1, left, want.left)
		}
	}
	if left := s.here(t, "event_inbox", recent); len(left) != 2 {
		t.Errorf("of the two events accepted an hour after the moment, %v are left", left)
	}
	if left := s.here(t, "event_inbox", waiting); len(left) != 2 {
		t.Errorf("of the two events that wait, %v are left: one that waits is never deleted", left)
	}
	// Accepted exactly at the moment is not before it.
	edge := s.aged(t, month, true, 1)
	if deleted, err := s.store.DeleteProcessed(t.Context(), before, 3); err != nil || deleted != 0 || len(s.here(t, "event_inbox", edge)) != 1 {
		t.Errorf("an event accepted at the moment itself was deleted (%d, %v)", deleted, err)
	}
	// What was counted of the events stays counted.
	if accepted := s.counter(t, "2026-11-19", s.acme, "accepted"); accepted != 12 {
		t.Errorf("%d events are counted as accepted, want the 12 that were", accepted)
	}
}

// The events that were processed are found through an index of their own,
// in the order in which they are deleted: a day of them is a million rows.
func TestProcessedEventsAreFoundThroughTheirIndex(t *testing.T) {
	s := newStage(t)
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	// An empty table is read whole, or through a bitmap and sorted, at no
	// cost. Without those ways out, the plan says whether the index can
	// give the events in their order.
	if _, err := tx.Exec(t.Context(), `SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off`); err != nil {
		t.Fatal(err)
	}
	var plan string
	err = tx.QueryRow(t.Context(),
		`EXPLAIN (FORMAT JSON) SELECT id FROM event_inbox
		  WHERE processed_at IS NOT NULL AND accepted_at < now() ORDER BY accepted_at LIMIT 5000`).Scan(&plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, `"Index Name": "event_inbox_processed_idx"`) || strings.Contains(plan, `"Node Type": "Sort"`) {
		t.Errorf("processed events are not read in their order through event_inbox_processed_idx: %s", plan)
	}
	var definition string
	if err := tx.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes WHERE indexname = 'event_inbox_processed_idx'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(definition, "WHERE (processed_at IS NOT NULL)") {
		t.Errorf("the index of the processed events is %q", definition)
	}
}

// A dead-lettered event is deleted once it was set aside before the
// moment the caller gives, and stays counted.
func TestDeadLettersAreDeletedByTheMomentTheyWereSetAside(t *testing.T) {
	s := newStage(t)
	const month = 30 * 24 * time.Hour
	events := s.aged(t, 3*month, false, 5)
	failing, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = failing.Rollback(context.WithoutCancel(t.Context())) }()
	for _, event := range events {
		for range MaxAttempts {
			if _, err := Fail(t.Context(), failing, event, postgres.Refusal{Code: "23514"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := failing.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Accepted three months ago, all of them. What counts is when each
	// was set aside: three before the moment, one at it and one after.
	for i, ago := range []time.Duration{month + 2*time.Hour, month + time.Hour, month + time.Minute, month, month - time.Hour} {
		if _, err := s.pool.Exec(t.Context(),
			`UPDATE event_dead_letters SET dead_lettered_at = $2::timestamptz - make_interval(secs => $3) WHERE id = $1::uuid`,
			events[i].ID, s.clock(t), ago.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	before := s.clock(t).Add(-month)
	for round, want := range []int{2, 1, 0} {
		if deleted, err := s.store.DeleteDeadLetters(t.Context(), before, 2); err != nil || deleted != want {
			t.Fatalf("round %d: DeleteDeadLetters deleted %d events with the error %v, want %d", round+1, deleted, err, want)
		}
	}
	if left := s.here(t, "event_dead_letters", events); !slices.Equal(left, []int{3, 4}) {
		t.Errorf("of the dead letters, %v are left; want the one set aside at the moment and the one after it", left)
	}
	if counted := s.counter(t, "2026-11-19", s.acme, counterDeadLettered); counted != 5 {
		t.Errorf("%d events are counted as dead-lettered, want the 5 that were", counted)
	}
}

// The counters of the days before a day are deleted, whatever they count
// and whoever they are of, and the ones of that day are not.
func TestCountersAreDeletedByTheirDay(t *testing.T) {
	// The day is the one of UTC wherever the process runs: these tests run
	// eight hours behind it.

	s := newStage(t)
	day := func(date string) time.Time {
		t.Helper()
		parsed, err := time.Parse(time.DateOnly, date)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	var counts []Count
	for _, date := range []string{"2026-11-17", "2026-11-18", "2026-11-19", "2026-11-20"} {
		counts = append(counts,
			Count{Day: day(date), WorkspaceID: s.acme, Name: counterServed, Value: 4},
			Count{Day: day(date), WorkspaceID: s.globex, Name: counterProcessed, Value: 3},
			Count{Day: day(date), Name: CounterNotFound, Value: 2})
	}
	if settled, err := s.store.Count(t.Context(), counts); err != nil || settled != len(counts) {
		t.Fatalf("Count settled %d counts with the error %v", settled, err)
	}

	// The midnight in UTC, given in another zone, as a clock would.
	before := day("2026-11-19").In(time.Local)
	if deleted, err := s.store.DeleteCounters(t.Context(), before); err != nil || deleted != 6 {
		t.Fatalf("DeleteCounters deleted %d rows with the error %v, want the 6 of the two days before", deleted, err)
	}
	rows, err := s.pool.Query(t.Context(), `SELECT DISTINCT day::text FROM pipeline_counters ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(left, []string{"2026-11-19", "2026-11-20"}) || s.number(t, `SELECT count(*) FROM pipeline_counters`) != 6 {
		t.Errorf("the counters left are of the days %v", left)
	}
	if again, err := s.store.DeleteCounters(t.Context(), before); err != nil || again != 0 {
		t.Errorf("a second time DeleteCounters deleted %d rows (%v)", again, err)
	}
}
