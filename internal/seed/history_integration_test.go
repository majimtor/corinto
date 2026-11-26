//go:build integration

package seed

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/rollup"
)

// counted reads what the aggregates hold of the demo workspaces: the
// events of each link, by its alias, the day, the class and the outcome.
func (s *stage) counted(t *testing.T) map[[4]string]int64 {
	t.Helper()
	rows, err := s.pool.Query(t.Context(),
		`SELECT l.identifier, (h.hour AT TIME ZONE 'UTC')::date::text, h.class, h.outcome, sum(h.events)::bigint
		   FROM link_hourly AS h JOIN links AS l ON l.id = h.link_id JOIN workspaces AS w ON w.id = l.workspace_id
		  WHERE w.demo
		  GROUP BY 1, 2, 3, 4`)
	if err != nil {
		t.Fatal(err)
	}
	figures := make(map[[4]string]int64)
	var (
		key    [4]string
		events int64
	)
	if _, err := pgx.ForEachRow(rows, []any{&key[0], &key[1], &key[2], &key[3], &events}, func() error {
		figures[key] = events
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return figures
}

// made returns what the history of a day holds, counted as counted counts
// it, and how many of its events are estimated human clicks.
func made(t *testing.T, day time.Time, days int) (figures map[[4]string]int64, perWorkspace []int, clicks int64) {
	t.Helper()
	classifier, err := classify.New()
	if err != nil {
		t.Fatal(err)
	}
	figures = make(map[[4]string]int64)
	for _, events := range history(classifier, day, days, sownFields()) {
		perWorkspace = append(perWorkspace, len(events))
		for _, event := range events {
			figures[[4]string{event.LinkID, event.OccurredAt.Format(time.DateOnly), string(event.Classification.Class), string(event.Outcome)}]++
			if event.Classification.Class == classify.ClassPresumedHuman && event.Outcome == ingest.OutcomeRedirected {
				clicks++
			}
		}
	}
	return figures, perWorkspace, clicks
}

// The synthetic history goes through the same classifier and the same
// consumer as real traffic (PLAN.md section 8), and the seed returns when
// it is counted. A consumer of another process may be at it meanwhile, as
// the worker of a system that is running: here one takes a hundred of the
// events and holds them, and the seed waits for it.
func TestTheSeedLeavesItsHistoryCountedAndTheReconciliationAddsUp(t *testing.T) {
	s := newStage(t)
	const held = 100
	pending := func() int64 {
		t.Helper()
		return s.number(t, `SELECT count(*) FROM event_inbox WHERE processed_at IS NULL`)
	}
	// The other consumer: once the history is being delivered it takes
	// its batch, and holds it until it is told to let go. It lets go
	// without having counted it, as a consumer that was stopped.
	holding, letGo, let := make(chan error, 1), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(letGo) })
	// However the test ends, the batch is let go: a database that is still
	// held cannot be closed.
	defer func() {
		release()
		<-let
	}()
	go func() {
		defer close(let)
		for delivered := 0; delivered < 10*held; {
			if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM event_inbox`).Scan(&delivered); err != nil {
				holding <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		tx, err := s.pool.Begin(t.Context())
		if err != nil {
			holding <- err
			return
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		taken, err := ingest.Take(t.Context(), tx, held)
		if err == nil && len(taken) != held {
			err = errors.New("the other consumer did not take its batch")
		}
		holding <- err
		<-letGo
	}()

	// The day the test runs on: the reconciliation shows the last days by
	// the clock of the database.
	now := time.Now()
	day := now.UTC().Truncate(24 * time.Hour)
	var (
		seeded   []Workspace
		seedErr  error
		returned = make(chan struct{})
	)
	go func() {
		defer close(returned)
		seeded, seedErr = Run(t.Context(), s.pool, s.links, s.keys, quiet, now, Options{})
	}()
	if err := <-holding; err != nil {
		t.Fatal(err)
	}
	// The seed counts all the rest, and then has nothing to take.
	for pending() != held {
		select {
		case <-returned:
			t.Fatalf("the seed returned with %d events not counted, and the error %v", pending(), seedErr)
		case <-time.After(20 * time.Millisecond):
		}
	}
	select {
	case <-returned:
		t.Fatalf("the seed returned while another consumer held %d of its events, with the error %v", held, seedErr)
	case <-time.After(5 * countPollEvery):
	}
	release()
	<-let
	<-returned
	if seedErr != nil {
		t.Fatal(seedErr)
	}
	want, perWorkspace, clicks := made(t, day, historyDays)

	// What the seed says it wrote: the history, and one event more for the
	// first workspace, which is its dead letter.
	if seeded[0].Events != perWorkspace[0]+1 || seeded[1].Events != perWorkspace[1] {
		t.Errorf("the seed says it wrote %d and %d events, want %d and %d", seeded[0].Events, seeded[1].Events, perWorkspace[0]+1, perWorkspace[1])
	}
	// Nothing waits, and the aggregates hold the history, event by event.
	if waiting := pending(); waiting != 0 {
		t.Fatalf("when the seed returned %d events still waited to be counted", waiting)
	}
	if got := s.counted(t); !maps.Equal(got, want) {
		t.Errorf("the aggregates hold %d figures of link, day, class and outcome, and the history %d; they are not the same", len(got), len(want))
	}
	for i, workspace := range seeded {
		if rows := s.number(t, `SELECT count(*) FROM event_inbox WHERE workspace_id = $1::uuid`, workspace.ID); rows != int64(perWorkspace[i]) {
			t.Errorf("the inbox has %d events of %s, want %d", rows, workspace.Name, perWorkspace[i])
		}
	}
	// The breakdowns count the estimated human clicks, each under one
	// value of each dimension.
	for _, dimension := range []string{"referrer_host", "device", "browser"} {
		if got := s.number(t, `SELECT coalesce(sum(events), 0) FROM link_daily_dimension WHERE dimension = $1`, dimension); got != clicks {
			t.Errorf("the breakdown by %s adds up to %d clicks, want %d", dimension, got, clicks)
		}
	}
	if versions := s.number(t, `SELECT count(*) FROM rule_versions WHERE first_hour >= $1 AND last_hour < $2`, day.AddDate(0, 0, -historyDays), day); versions != 1 {
		t.Errorf("%d versions of the rules classified events within the days of the history, want one", versions)
	}

	// One dead letter (PLAN.md section 8), which the seed set aside itself:
	// a click on the first link of the first workspace, with its reason.
	if rows := s.number(t,
		`SELECT count(*) FROM event_dead_letters AS d JOIN links AS l ON l.id = d.link_id
		  WHERE d.workspace_id = $1::uuid AND l.identifier = $2 AND d.occurred_at = $3 AND d.class = 'presumed_human'
		    AND d.reason_sqlstate = '23514' AND d.reason_constraint = 'seed_example'`,
		seeded[0].ID, demos[0].links[0].alias, day.AddDate(0, 0, -deadLetterDaysAgo).Add(12*time.Hour)); rows != 1 {
		t.Errorf("%d dead letters are the one of the seed, want it", rows)
	}
	if rows := s.number(t, `SELECT count(*) FROM event_dead_letters`); rows != 1 {
		t.Errorf("there are %d dead letters, want the one of the seed", rows)
	}

	// The reconciliation of every day adds up, with nothing lost and
	// nothing pending, and is not a row of zeros.
	inbox := ingest.NewStore(s.pool, quiet)
	for i, workspace := range seeded {
		days, err := inbox.Reconciliation(t.Context(), workspace.ID, ingest.MaxDays)
		if err != nil {
			t.Fatal(err)
		}
		var served, dead int64
		for _, terms := range days {
			if terms.Served != terms.Accepted || terms.Lost != 0 || terms.Pending != 0 || terms.Accepted != terms.Processed+terms.DeadLettered {
				t.Errorf("the reconciliation of %s on %s does not add up: %+v", workspace.Name, terms.Day.Format(time.DateOnly), terms)
			}
			if terms.Day.Equal(day) != (terms.Served == 0) {
				t.Errorf("%s was served %d responses on %s; want some on every day but the day of the seed", workspace.Name, terms.Served, terms.Day.Format(time.DateOnly))
			}
			served += terms.Served
			dead += terms.DeadLettered
		}
		if wantDead := int64(1 - i); dead != wantDead || served == 0 {
			t.Errorf("the reconciliation of %s shows %d dead letters and %d responses served, want %d and some", workspace.Name, dead, served, wantDead)
		}
	}

	// What a page of the panel reads for a link, over its last thirty
	// days: the figures of the history, and for the link that has expired
	// the requests it no longer redirects.
	tomorrow := day.Add(24 * time.Hour)
	period, err := rollup.NewPeriod(tomorrow.AddDate(0, 0, -30), tomorrow, rollup.ByDay)
	if err != nil {
		t.Fatal(err)
	}
	reader := rollup.NewReader(s.pool)
	for _, alias := range []string{"spring-reading", "winter-clearance", "ride-map"} {
		var expected rollup.Figures
		for key, events := range want {
			if occurred, _ := time.Parse(time.DateOnly, key[1]); key[0] != alias || occurred.Before(period.From()) {
				continue
			}
			switch {
			case key[3] == string(ingest.OutcomeInactive):
				expected.InactiveRequests += events
			case key[2] == string(classify.ClassPresumedHuman):
				expected.EstimatedHumanClicks += events
				expected.Requests += events
			default:
				expected.Requests += events
			}
		}
		var id string
		if err := s.pool.QueryRow(t.Context(),
			`SELECT l.id::text FROM links AS l JOIN workspaces AS w ON w.id = l.workspace_id WHERE w.demo AND l.identifier = $1`, alias).Scan(&id); err != nil {
			t.Fatal(err)
		}
		got, err := reader.Summary(t.Context(), id, period)
		if err != nil {
			t.Fatal(err)
		}
		if got != expected || got.Requests == 0 {
			t.Errorf("the last thirty days of %s are %+v, want %+v", alias, got, expected)
		}
		if alias == "winter-clearance" && got.InactiveRequests == 0 {
			t.Error("the link that has expired has no request that it did not redirect")
		}
	}
}

// Two seeds on one day leave the same figures, and the second leaves
// nothing of the first: its history goes with its workspaces. So does the
// history of a seed that was stopped halfway. The two that finish write
// twenty days and not ninety: it is the same code, and a fifth of the wait.
func TestASecondSeedLeavesTheSameFiguresAndNothingOfTheFirst(t *testing.T) {
	const days = 20
	s := newStage(t)
	// A seed that is stopped while it writes its history, which takes
	// seconds: it fails, and leaves what it had written.
	short, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	if seeded, err := Run(short, s.pool, s.links, s.keys, quiet, seedDay, Options{}); err == nil || seeded != nil {
		t.Fatalf("a seed that was stopped halfway = %v, %v; want an error", seeded, err)
	}
	if left := s.number(t, `SELECT count(*) FROM workspaces WHERE demo`); left != 2 {
		t.Fatalf("the seed that was stopped left %d demo workspaces, want the two it had created", left)
	}

	rows := func() [5]int64 {
		t.Helper()
		var counts [5]int64
		for i, table := range []string{"event_inbox", "event_dead_letters", "pipeline_counters", "link_hourly", "link_daily_dimension"} {
			counts[i] = s.number(t, `SELECT count(*) FROM `+table)
		}
		return counts
	}
	first, err := run(t.Context(), s.pool, s.links, s.keys, quiet, seedDay, false, days)
	if err != nil {
		t.Fatal(err)
	}
	figures, held := s.counted(t), rows()
	if want, _, _ := made(t, seedDay.UTC().Truncate(24*time.Hour), days); !maps.Equal(figures, want) {
		t.Fatalf("after a seed that was stopped and one that was not, the aggregates hold %d figures and the history %d; they are not the same", len(figures), len(want))
	}

	second, err := run(t.Context(), s.pool, s.links, s.keys, quiet, seedDay.Add(3*time.Hour), false, days)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].ID == first[0].ID || second[0].Events != first[0].Events || second[1].Events != first[1].Events {
		t.Errorf("the second seed left %+v after %+v; want new workspaces with as many events", second, first)
	}
	if again := s.counted(t); !maps.Equal(again, figures) {
		t.Error("a second seed on the same day left other figures")
	}
	// As many rows in every table as after the first: none of it stayed.
	if again := rows(); again != held {
		t.Errorf("after the second seed the tables hold %v rows, and after the first %v", again, held)
	}
	for _, old := range first {
		for _, table := range []string{"event_inbox", "event_dead_letters", "pipeline_counters", "link_hourly", "link_daily_dimension"} {
			if left := s.number(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1::uuid`, old.ID); left != 0 {
				t.Errorf("%s still holds %d rows of the first %s", table, left, old.Name)
			}
		}
	}
}

// A history of a few days is the last of them and no more: what the other
// tests of the seed leave out, and what a test that needs some traffic can
// ask for.
func TestAHistoryOfAFewDaysIsTheLastOfThem(t *testing.T) {
	s := newStage(t)
	seeded, err := run(t.Context(), s.pool, s.links, s.keys, quiet, seedDay, false, 5)
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay.UTC().Truncate(24 * time.Hour)
	want, perWorkspace, _ := made(t, day, 5)
	if got := s.counted(t); !maps.Equal(got, want) || seeded[0].Events != perWorkspace[0]+1 || seeded[1].Events != perWorkspace[1] {
		t.Errorf("a history of five days left %d figures and the seed says it wrote %d and %d events; want %d figures, %d and %d events",
			len(got), seeded[0].Events, seeded[1].Events, len(want), perWorkspace[0]+1, perWorkspace[1])
	}
	if outside := s.number(t, `SELECT count(*) FROM event_inbox WHERE occurred_at < $1 OR occurred_at >= $2`, day.AddDate(0, 0, -5), day); outside != 0 {
		t.Errorf("%d events are outside the five days before the seed", outside)
	}
	// And with no day, the seed is its workspaces and nothing else.
	without := s.run(t, seedDay)
	if events := s.number(t, `SELECT (SELECT count(*) FROM event_inbox) + (SELECT count(*) FROM event_dead_letters) + (SELECT count(*) FROM link_hourly)`); events != 0 || without[0].Events != 0 {
		t.Errorf("a seed without a history left %d events, dead letters and figures", events)
	}
}

// What a run can be asked for, through the function the command calls:
// to do nothing when the demo workspaces are there, and to write no
// history, which is what the load run asks.
func TestARunDoesAsItIsAsked(t *testing.T) {
	s := newStage(t)
	traces := func() int64 {
		t.Helper()
		return s.number(t, `SELECT (SELECT count(*) FROM event_inbox) + (SELECT count(*) FROM event_dead_letters) +
		                           (SELECT count(*) FROM link_hourly) + (SELECT count(*) FROM pipeline_counters)`)
	}
	first, err := Run(t.Context(), s.pool, s.links, s.keys, quiet, seedDay, Options{OnlyIfMissing: true, WithoutHistory: true})
	if err != nil || len(first) != 2 || first[0].Events != 0 || first[1].Events != 0 || traces() != 0 {
		t.Fatalf("on an empty database, a run without a history = %+v, %v, with %d events, dead letters, figures and counts; want the two workspaces and none", first, err, traces())
	}
	if again, err := Run(t.Context(), s.pool, s.links, s.keys, quiet, seedDay, Options{OnlyIfMissing: true}); err != nil || again != nil || traces() != 0 {
		t.Errorf("with the demo workspaces there, a run only if they are missing = %+v, %v; want nothing done, and no history written", again, err)
	}
	replaced, err := Run(t.Context(), s.pool, s.links, s.keys, quiet, seedDay, Options{WithoutHistory: true})
	if err != nil || len(replaced) != 2 || replaced[0].ID == first[0].ID || traces() != 0 {
		t.Errorf("a run without a history = %+v, %v; want new workspaces and no history", replaced, err)
	}
}
