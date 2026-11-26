//go:build integration

package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/postgres"
)

// What the inbox holds of a demo workspace is deleted for the seed that
// creates the workspace again: its events, the ones that wait and the ones
// that were processed, its dead letters and its counts. What is of another
// workspace, or of none, is not.
func TestOnlyWhatTheInboxHoldsOfADemoWorkspaceIsDeleted(t *testing.T) {
	s := newStage(t)
	if _, err := s.pool.Exec(t.Context(), `UPDATE workspaces SET demo = true WHERE id = $1::uuid`, s.acme); err != nil {
		t.Fatal(err)
	}
	// In each workspace: an event that waits, one that was processed and
	// one among the dead letters, with the counts they leave.
	for _, owner := range []struct{ workspace, link string }{{s.acme, s.acmeLink}, {s.globex, s.globexLink}} {
		events := s.events(noon, 3)
		for i := range events {
			events[i].LinkID, events[i].WorkspaceID = owner.link, owner.workspace
		}
		if settled, err := s.store.Accept(t.Context(), events); err != nil || settled != 3 {
			t.Fatalf("Accept settled %d events with the error %v", settled, err)
		}
		if settled, err := s.store.Count(t.Context(), []Count{{Day: noon, WorkspaceID: owner.workspace, Name: counterServed, Value: 3}}); err != nil || settled != 1 {
			t.Fatalf("Count settled %d counts with the error %v", settled, err)
		}
		if _, err := s.pool.Exec(t.Context(), `UPDATE event_inbox SET attempts = 4 WHERE id = $1::uuid`, events[2].ID); err != nil {
			t.Fatal(err)
		}
		tx, err := s.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := MarkProcessed(t.Context(), tx, events[:1]); err != nil {
			t.Fatal(err)
		}
		if dead, err := Fail(t.Context(), tx, events[2], postgres.Refusal{Code: "23514", Constraint: "a_test"}); err != nil || !dead {
			t.Fatalf("Fail = %v, %v; want the event among the dead letters", dead, err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	// A count that is of no workspace.
	if settled, err := s.store.Count(t.Context(), []Count{{Day: noon, Name: CounterNotFound, Value: 7}}); err != nil || settled != 1 {
		t.Fatalf("Count settled %d counts with the error %v", settled, err)
	}
	held := func(workspace string) [3]int64 {
		t.Helper()
		return [3]int64{
			s.number(t, `SELECT count(*) FROM event_inbox WHERE workspace_id = $1::uuid`, workspace),
			s.number(t, `SELECT count(*) FROM event_dead_letters WHERE workspace_id = $1::uuid`, workspace),
			s.number(t, `SELECT count(*) FROM pipeline_counters WHERE workspace_id = $1::uuid`, workspace),
		}
	}
	// Two events, a dead letter and four counts: served, accepted,
	// processed and dead-lettered.
	whole := [3]int64{2, 1, 4}
	if before, theirs := held(s.acme), held(s.globex); before != whole || theirs != whole {
		t.Fatalf("before any deletion the workspaces hold %v and %v, want %v each", before, theirs, whole)
	}

	for _, workspace := range []string{s.globex, s.acme} {
		tx, err := s.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		if err := DeleteDemoEvents(t.Context(), tx, workspace); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if after := held(s.acme); after != [3]int64{} {
		t.Errorf("the demo workspace still holds %v in the inbox, its dead letters and its counts", after)
	}
	if theirs := held(s.globex); theirs != whole {
		t.Errorf("the workspace that is not a demo holds %v, want %v", theirs, whole)
	}
	if nobody := s.number(t, `SELECT coalesce(sum(value), 0) FROM pipeline_counters WHERE workspace_id IS NULL`); nobody != 7 {
		t.Errorf("the counts of no workspace add up to %d, want 7", nobody)
	}
}

// The seed shows a dead letter that no failure made: nothing in the schema
// refuses an event. What it sets aside is counted as any event that failed
// all its attempts, so the reconciliation of its day adds up, and it can
// be sent back like any other.
func TestAnEventOfADemoWorkspaceIsSetAsideAndTheReconciliationAddsUp(t *testing.T) {
	s := newStage(t)
	if _, err := s.pool.Exec(t.Context(), `UPDATE workspaces SET demo = true WHERE id = $1::uuid`, s.acme); err != nil {
		t.Fatal(err)
	}
	// Two days ago, so that the reconciliation of the last days has it: a
	// second of that day, and an access a part of a second into it.
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	click := Event{
		LinkID: s.acmeLink, WorkspaceID: s.acme, OccurredAt: at.Add(987 * time.Millisecond),
		Outcome: OutcomeRedirected, Method: "GET", Classification: human,
	}
	reason := postgres.Refusal{Code: "23514", Constraint: "a_test"}
	setAside := func(event Event, commit bool) (string, error) {
		t.Helper()
		tx, err := s.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		id, err := SetAsideDemoEvent(t.Context(), tx, event, reason)
		if err == nil && commit {
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		return id, err
	}
	everything := func() [3]int64 {
		t.Helper()
		return [3]int64{
			s.rows(t), s.number(t, `SELECT count(*) FROM event_dead_letters`), s.number(t, `SELECT count(*) FROM pipeline_counters`),
		}
	}

	// It is the transaction of the caller: undone, it leaves nothing.
	if id, err := setAside(click, false); err != nil || id == "" {
		t.Fatalf("SetAsideDemoEvent = %q, %v", id, err)
	}
	if left := everything(); left != [3]int64{} {
		t.Fatalf("a transaction that was undone left %v events, dead letters and counts", left)
	}
	// Only a demo workspace has an event set aside by hand.
	for what, workspace := range map[string]string{
		"a workspace that is not a demo":  s.globex,
		"a workspace that does not exist": "00000000-0000-7000-8000-000000000000",
	} {
		other := click
		other.WorkspaceID = workspace
		if id, err := setAside(other, true); !errors.Is(err, ErrNotDemo) || id != "" {
			t.Errorf("%s: SetAsideDemoEvent = %q, %v; want %v", what, id, err, ErrNotDemo)
		}
	}
	if left := everything(); left != [3]int64{} {
		t.Fatalf("the refusals left %v events, dead letters and counts", left)
	}

	id, err := setAside(click, true)
	if err != nil {
		t.Fatal(err)
	}
	// Among the dead letters and not in the inbox, with its reason, as the
	// buffer would have handed it over: to the second, under an identifier
	// of that second.
	if rows := s.number(t,
		`SELECT count(*) FROM event_dead_letters
		  WHERE id = $1::uuid AND workspace_id = $2::uuid AND link_id = $3::uuid AND class = 'presumed_human'
		    AND reason_sqlstate = '23514' AND reason_constraint = 'a_test'
		    AND occurred_at = $4 AND uuid_extract_timestamp(id) = occurred_at`,
		id, s.acme, s.acmeLink, at); rows != 1 || s.rows(t) != 0 {
		t.Errorf("%d dead letters are the event that was set aside, and the inbox has %d events; want 1 and none", rows, s.rows(t))
	}
	day := at.Format(time.DateOnly)
	for name, want := range map[Counter]int64{counterServed: 1, "accepted": 1, counterDeadLettered: 1, counterProcessed: 0} {
		if got := s.counter(t, day, s.acme, name); got != want {
			t.Errorf("the counter %q of the day is at %d, want %d", name, got, want)
		}
	}
	days, err := s.store.Reconciliation(t.Context(), s.acme, 7)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, terms := range days {
		if terms.Served != terms.Accepted+terms.Lost || terms.Accepted != terms.Processed+terms.Pending+terms.DeadLettered {
			t.Errorf("the reconciliation of %s does not add up: %+v", terms.Day.Format(time.DateOnly), terms)
		}
		if terms.Day.Format(time.DateOnly) == day {
			found = terms.Served == 1 && terms.Accepted == 1 && terms.Lost == 0 && terms.DeadLettered == 1 && terms.Pending == 0
		}
	}
	if !found {
		t.Errorf("the reconciliation does not show the day of the event with one served, accepted and dead-lettered: %+v", days)
	}

	// Nothing of the workspace waits, until the operator sends the dead
	// letter back: then it does, and nothing of another workspace is
	// counted with it.
	if pending, err := s.store.DemoEventsPending(t.Context(), s.acme); err != nil || pending != 0 {
		t.Errorf("DemoEventsPending = %d, %v; want none", pending, err)
	}
	theirs := s.event(noon)
	theirs.LinkID, theirs.WorkspaceID = s.globexLink, s.globex
	if settled, err := s.store.Accept(t.Context(), []Event{theirs}); err != nil || settled != 1 {
		t.Fatalf("Accept settled %d events with the error %v", settled, err)
	}
	if sent, err := s.store.RetryDeadLetters(t.Context()); err != nil || sent != 1 {
		t.Fatalf("RetryDeadLetters = %d, %v; want the one event", sent, err)
	}
	if pending, err := s.store.DemoEventsPending(t.Context(), s.acme); err != nil || pending != 1 {
		t.Errorf("after the dead letter was sent back, DemoEventsPending = %d, %v; want it", pending, err)
	}
}
