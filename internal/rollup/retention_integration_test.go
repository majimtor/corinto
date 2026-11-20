//go:build integration

package rollup

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/postgres"
)

// traced tells a test every statement a pool sends.
type traced struct {
	mu sync.Mutex
	on func(sql string)
}

func (tr *traced) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.on != nil {
		tr.on(data.SQL)
	}
	return ctx
}

func (*traced) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// poolWith returns another pool to the database of the stage, changed as a
// test says.
func (s *stage) poolWith(t *testing.T, change func(*pgxpool.Config)) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	change(config)
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// retention returns the retention job of a process whose database works
// nine hours ahead of UTC, and its log: the days it reasons with are the
// ones of UTC all the same.
func (s *stage) retention(t *testing.T) (*Retention, *lockedLog) {
	t.Helper()
	ahead := s.poolWith(t, func(config *pgxpool.Config) { config.ConnConfig.RuntimeParams["timezone"] = "Asia/Tokyo" })
	log := &lockedLog{}
	logger := slog.New(slog.NewJSONHandler(log, nil))
	return NewRetention(ahead, ingest.NewStore(ahead, logger), logger), log
}

// The moments a run compares with, by the clock and the calendar of the
// database: the one before which raw events are due, and the midnight in
// UTC before which aggregates are.
const (
	rawDue = `now() - interval '720 hours'`
	dayDue = `((date_trunc('day', now() AT TIME ZONE 'UTC') - interval '13 months') AT TIME ZONE 'UTC')`
)

// exec runs a statement that a test needs to prepare what it tests.
func (s *stage) exec(t *testing.T, sql string, arguments ...any) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), sql, arguments...); err != nil {
		t.Fatal(err)
	}
}

// processedAt makes an event of the inbox one that was accepted at a
// moment, given as SQL, and processed a second later.
func (s *stage) processedAt(t *testing.T, event ingest.Event, moment string) {
	t.Helper()
	s.exec(t, `UPDATE event_inbox SET accepted_at = `+moment+`, processed_at = `+moment+` + interval '1 second' WHERE id = $1::uuid`, event.ID)
}

// sweep does a run that must not fail, and returns whether there was one.
func sweepOnce(t *testing.T, retention *Retention) bool {
	t.Helper()
	ran, err := retention.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep failed: %v", err)
	}
	return ran
}

// PLAN.md section 5: raw events are kept 30 days and aggregates 13 months,
// and a daily job deletes what is due and shows when it last ran.
func TestTheRetentionRunDeletesWhatIsDueAndNothingElse(t *testing.T) {
	s := newStage(t)
	retention, log := s.retention(t)

	// The inbox: a processed event a minute past its time, one a minute
	// short of it, and one that still waits and is twice as old.
	// They occurred today, whatever day the test runs on: accepting them
	// counts them on that day, and the count is not due.
	today := time.Now().UTC().Truncate(time.Second)
	due, kept, waiting := click(s.spring, s.acme, today), click(s.spring, s.acme, today), click(s.spring, s.acme, today)
	s.accept(t, due, kept, waiting)
	s.processedAt(t, due, rawDue+` - interval '1 minute'`)
	s.processedAt(t, kept, rawDue+` + interval '1 minute'`)
	s.exec(t, `UPDATE event_inbox SET accepted_at = now() - interval '60 days' WHERE id = $1::uuid`, waiting.ID)
	// The dead letters: accepted long ago, both; set aside a minute before
	// the moment, and a minute after.
	for _, moment := range []string{rawDue + ` - interval '1 minute'`, rawDue + ` + interval '1 minute'`} {
		s.exec(t, `INSERT INTO event_dead_letters (id, link_id, workspace_id, occurred_at, outcome, method, device, browser, class, rule,
		                                         rules_version, accepted_at, reason_sqlstate, dead_lettered_at)
		           VALUES (uuidv7(), $1::uuid, $2::uuid, now() - interval '90 days', 'redirected', 'GET', 'desktop', 'chrome',
		                   'presumed_human', 'presumed_human', 1, now() - interval '90 days', '23514', `+moment+`)`, s.spring, s.acme)
	}
	// The aggregates: the last hour and the day before the day that is
	// due, that day itself, and now.
	for _, hour := range []string{dayDue + ` - interval '1 hour'`, dayDue, `date_trunc('hour', now())`} {
		s.exec(t, `INSERT INTO link_hourly (link_id, workspace_id, hour, outcome, class, events)
		           VALUES ($1::uuid, $2::uuid, `+hour+`, 'redirected', 'presumed_human', 1)`, s.spring, s.acme)
	}
	for _, day := range []string{dayDue + ` - interval '1 day'`, dayDue, `now()`} {
		s.exec(t, `INSERT INTO link_daily_dimension (link_id, workspace_id, day, dimension, value, events)
		           VALUES ($1::uuid, $2::uuid, ((`+day+`) AT TIME ZONE 'UTC')::date, 'device', 'mobile', 1)`, s.spring, s.acme)
		s.exec(t, `INSERT INTO pipeline_counters (day, workspace_id, name, value)
		           VALUES (((`+day+`) AT TIME ZONE 'UTC')::date, $1::uuid, 'served', 5), (((`+day+`) AT TIME ZONE 'UTC')::date, NULL, 'not_found', 2)`, s.acme)
	}
	// The runs: one of long ago, and the one of yesterday.
	s.exec(t, `INSERT INTO retention_runs (started_at, finished_at) VALUES
	           (`+dayDue+` - interval '1 second', `+dayDue+`), (now() - interval '1 day', now() - interval '1 day')`)

	if !sweepOnce(t, retention) {
		t.Fatal("the run of today was not done")
	}
	for what, want := range map[string]struct {
		query string
		left  int64
	}{
		"the processed event past its time":             {`SELECT count(*) FROM event_inbox WHERE id = '` + due.ID + `'`, 0},
		"the processed event short of its time":         {`SELECT count(*) FROM event_inbox WHERE id = '` + kept.ID + `'`, 1},
		"the event that still waits":                    {`SELECT count(*) FROM event_inbox WHERE id = '` + waiting.ID + `' AND processed_at IS NULL`, 1},
		"the dead letter set aside before the moment":   {`SELECT count(*) FROM event_dead_letters WHERE dead_lettered_at < ` + rawDue, 0},
		"the dead letter set aside after the moment":    {`SELECT count(*) FROM event_dead_letters`, 1},
		"the hour before the day that is due":           {`SELECT count(*) FROM link_hourly WHERE hour < ` + dayDue, 0},
		"the hours from the day that is due on":         {`SELECT count(*) FROM link_hourly`, 2},
		"the breakdown of the day before":               {`SELECT count(*) FROM link_daily_dimension WHERE day < (` + dayDue + ` AT TIME ZONE 'UTC')::date`, 0},
		"the breakdowns from the day that is due on":    {`SELECT count(*) FROM link_daily_dimension`, 2},
		"the counters of the day before":                {`SELECT count(*) FROM pipeline_counters WHERE day < (` + dayDue + ` AT TIME ZONE 'UTC')::date`, 0},
		"the counters from the day that is due on":      {`SELECT count(*) FROM pipeline_counters WHERE name <> 'accepted'`, 4},
		"the count of the events accepted today":        {`SELECT count(*) FROM pipeline_counters WHERE name = 'accepted' AND value = 3`, 1},
		"the run of long ago":                           {`SELECT count(*) FROM retention_runs WHERE started_at < ` + dayDue, 0},
		"the run of yesterday and the one of today":     {`SELECT count(*) FROM retention_runs`, 2},
		"the run of today, with its end and its counts": {`SELECT count(*) FROM retention_runs WHERE started_at > now() - interval '1 minute' AND finished_at BETWEEN started_at AND now() AND (inbox_events, dead_letters, hourly_rows, daily_rows, counters) = (1, 1, 1, 1, 2)`, 1},
	} {
		if left := s.number(t, want.query); left != want.left {
			t.Errorf("%s: %d rows, want %d", what, left, want.left)
		}
	}
	finished := log.lines(t, "the retention run finished")
	if len(finished) != 1 || finished[0]["inbox_events"] != float64(1) || finished[0]["dead_letters"] != float64(1) ||
		finished[0]["hourly_rows"] != float64(1) || finished[0]["daily_rows"] != float64(1) || finished[0]["counters"] != float64(2) {
		t.Errorf("the log says %v, want one run with what it deleted", finished)
	}
}

// One run a day, by the day of UTC: the first process that asks does it,
// and a run that never finished is done again.
func TestTheRetentionRunIsDoneOnceADay(t *testing.T) {
	s := newStage(t)
	retention, _ := s.retention(t)
	runs := func() int64 { return s.number(t, `SELECT count(*) FROM retention_runs`) }
	const midnight = `(date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')`

	if !sweepOnce(t, retention) || runs() != 1 {
		t.Fatalf("the first time: %d runs", runs())
	}
	if sweepOnce(t, retention) || runs() != 1 {
		t.Fatalf("the run of today was done a second time: %d runs", runs())
	}
	// A run that began at the midnight of today is of today.
	s.exec(t, `UPDATE retention_runs SET started_at = `+midnight+`, finished_at = `+midnight)
	if sweepOnce(t, retention) || runs() != 1 {
		t.Fatalf("a run that began at midnight was not taken as the one of today: %d runs", runs())
	}
	// One that began a second earlier is of yesterday.
	s.exec(t, `UPDATE retention_runs SET started_at = `+midnight+` - interval '1 second'`)
	if !sweepOnce(t, retention) || runs() != 2 {
		t.Fatalf("after a run of yesterday, the one of today was not done: %d runs", runs())
	}
	// A run of today whose process stopped before the end does not count.
	s.exec(t, `UPDATE retention_runs SET finished_at = NULL WHERE started_at >= `+midnight)
	if !sweepOnce(t, retention) || runs() != 3 {
		t.Fatalf("after a run that never finished, the one of today was not done: %d runs", runs())
	}
	if unfinished := s.number(t, `SELECT count(*) FROM retention_runs WHERE finished_at IS NULL`); unfinished != 1 {
		t.Errorf("%d runs have no end, want the one that never finished, as it was", unfinished)
	}
	if sweepOnce(t, retention) || runs() != 3 {
		t.Errorf("the run of today was done again: %d runs", runs())
	}
}

// One process runs at a time: while another has the turn, a process does
// nothing and does not wait, and the turn is given back after a run.
func TestOneProcessDoesTheRetentionRunAtATime(t *testing.T) {
	s := newStage(t)
	retention, _ := s.retention(t)
	due := click(s.spring, s.acme, at(10, 0, 0))
	s.accept(t, due)
	s.processedAt(t, due, rawDue+` - interval '1 minute'`)

	// Another process is in the middle of a run.
	other, err := s.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	turn := func(function string) bool {
		t.Helper()
		var answer bool
		if err := other.QueryRow(t.Context(), `SELECT `+function+`($1, $2)`, turnOfTheJobs, turnOfTheRetention).Scan(&answer); err != nil {
			t.Fatal(err)
		}
		return answer
	}
	if !turn("pg_try_advisory_lock") {
		t.Fatal("the turn could not be taken")
	}
	quick, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if ran, err := retention.Sweep(quick); err != nil || ran {
		t.Fatalf("with the turn taken, Sweep ran = %t with the error %v; want nothing done and no wait", ran, err)
	}
	if runs, left := s.number(t, `SELECT count(*) FROM retention_runs`), s.number(t, `SELECT count(*) FROM event_inbox`); runs != 0 || left != 1 {
		t.Fatalf("with the turn taken there are %d runs and %d events, want none and the one that is due", runs, left)
	}

	// The other process ends, and this one does the run.
	if !turn("pg_advisory_unlock") {
		t.Fatal("the turn was not held")
	}
	if !sweepOnce(t, retention) || s.number(t, `SELECT count(*) FROM event_inbox`) != 0 {
		t.Fatal("with the turn free the run was not done")
	}
	// And gives the turn back: another process can take it.
	if !turn("pg_try_advisory_lock") {
		t.Error("the turn was not given back after the run")
	}
	turn("pg_advisory_unlock")
	// Also when nothing was to be done.
	if sweepOnce(t, retention) || !turn("pg_try_advisory_lock") {
		t.Error("the turn was not given back by a process that found the run done")
	}
	turn("pg_advisory_unlock")
}

// A run deletes in batches, and writes how far it got after each: one
// that stops halfway says what it deleted, gives the turn back, and the
// next run deletes the rest.
func TestARunDeletesInBatchesAndSaysHowFarItGot(t *testing.T) {
	s := newStage(t)
	events := make([]ingest.Event, 10)
	for i := range events {
		events[i] = click(s.spring, s.acme, at(10, i, 0))
	}
	s.accept(t, events...)
	for _, event := range events {
		s.processedAt(t, event, rawDue+` - interval '1 minute'`)
	}

	// The process is stopped when it sends its third batch of events.
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	statements := &traced{}
	batches := 0
	statements.on = func(sql string) {
		if strings.Contains(sql, "DELETE FROM event_inbox") {
			if batches++; batches == 3 {
				stop()
			}
		}
	}
	watched := s.poolWith(t, func(config *pgxpool.Config) { config.ConnConfig.Tracer = statements })
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	retention := NewRetention(watched, ingest.NewStore(watched, logger), logger)
	retention.batch = 3

	if ran, err := retention.Sweep(ctx); err == nil || !ran {
		t.Fatalf("a run that was stopped ended with ran = %t and the error %v", ran, err)
	}
	if left, said := s.number(t, `SELECT count(*) FROM event_inbox`), s.number(t, `SELECT count(*) FROM retention_runs WHERE finished_at IS NULL AND inbox_events = 6`); left != 4 || said != 1 {
		t.Fatalf("%d events are left and %d runs without an end say they deleted 6; want 4 and 1", left, said)
	}

	// The next one, with nobody stopping it: four batches of events, of
	// three, three, three and one, would be the ten; four are left, in two.
	statements.mu.Lock()
	batches = 0
	statements.on = func(sql string) {
		if strings.Contains(sql, "DELETE FROM event_inbox") {
			batches++
		}
	}
	statements.mu.Unlock()
	if !sweepOnce(t, retention) {
		t.Fatal("after a run that stopped halfway, the run of today was not done")
	}
	if left, said := s.number(t, `SELECT count(*) FROM event_inbox`), s.number(t, `SELECT count(*) FROM retention_runs WHERE finished_at IS NOT NULL AND inbox_events = 4`); left != 0 || said != 1 || batches != 2 {
		t.Errorf("%d events are left, %d finished runs say they deleted 4, in %d batches; want 0, 1 and 2", left, said, batches)
	}
	if runs := s.number(t, `SELECT count(*) FROM retention_runs`); runs != 2 {
		t.Errorf("%d runs, want the one that stopped and the one that finished", runs)
	}
}

// The table of the runs refuses what the code would not write.
func TestARunCannotEndBeforeItBeganNorDeleteLessThanNothing(t *testing.T) {
	s := newStage(t)
	for name, refused := range map[string]struct{ statement, constraint string }{
		"an end before the beginning": {`INSERT INTO retention_runs (finished_at) VALUES (now() - interval '1 second')`, "retention_runs_check"},
		"events deleted below zero":   {`INSERT INTO retention_runs (inbox_events) VALUES (-1)`, "retention_runs_inbox_events_check"},
		"counters deleted below zero": {`INSERT INTO retention_runs (counters) VALUES (-1)`, "retention_runs_counters_check"},
	} {
		_, err := s.pool.Exec(t.Context(), refused.statement)
		if refusal, ok := postgres.Refused(err); !ok || refusal.Code != "23514" || refusal.Constraint != refused.constraint {
			t.Errorf("%s: the statement ended with %v, want it refused by %s", name, err, refused.constraint)
		}
	}
}
