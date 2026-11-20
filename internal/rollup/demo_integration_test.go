//go:build integration

package rollup

import (
	"context"
	"testing"

	"github.com/majimtor/corinto/internal/ingest"
)

// The aggregates of a demo workspace are deleted for the seed that creates
// the workspace again. The ones of any other are not.
func TestOnlyTheAggregatesOfADemoWorkspaceAreDeleted(t *testing.T) {
	s := newStage(t)
	if _, err := s.pool.Exec(t.Context(), `UPDATE workspaces SET demo = true WHERE id = $1::uuid`, s.acme); err != nil {
		t.Fatal(err)
	}
	referred := func(e *ingest.Event) { e.ReferrerHost = "news.example.com" }
	s.accept(t,
		with(click(s.spring, s.acme, at(10, 0, 0)), referred), click(s.summer, s.acme, at(11, 0, 0)),
		with(click(s.autumn, s.globex, at(10, 0, 0)), referred),
	)
	s.drain(t, 1)
	held := func(workspace string) [2]int64 {
		t.Helper()
		return [2]int64{
			s.number(t, `SELECT count(*) FROM link_hourly WHERE workspace_id = $1::uuid`, workspace),
			s.number(t, `SELECT count(*) FROM link_daily_dimension WHERE workspace_id = $1::uuid`, workspace),
		}
	}
	// An hour of each link, and a device, a browser and a referrer of each.
	if before, theirs := held(s.acme), held(s.globex); before != [2]int64{2, 6} || theirs != [2]int64{1, 3} {
		t.Fatalf("before any deletion the workspaces hold %v and %v", before, theirs)
	}

	for _, workspace := range []string{s.globex, s.acme} {
		tx, err := s.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		if err := DeleteDemoAggregates(t.Context(), tx, workspace); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if after := held(s.acme); after != [2]int64{} {
		t.Errorf("the demo workspace still holds %v in the time series and the breakdowns", after)
	}
	if theirs := held(s.globex); theirs != [2]int64{1, 3} {
		t.Errorf("the workspace that is not a demo holds %v, want an hour and three values", theirs)
	}
}
