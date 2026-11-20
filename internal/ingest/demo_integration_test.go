//go:build integration

package ingest

import (
	"context"
	"testing"

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
