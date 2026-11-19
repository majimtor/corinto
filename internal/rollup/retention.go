package rollup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/ingest"
)

// How long things are kept. PLAN.md section 5 states the two periods.
const (
	// RawEventsFor is how long a raw event is kept: an event of the inbox
	// from its acceptance, once it was processed, and a dead-lettered one
	// from the moment it was set aside.
	RawEventsFor = 30 * 24 * time.Hour
	// AggregatesForMonths is how many months the aggregates of a UTC day
	// are kept, and with them the counters of that day.
	AggregatesForMonths = 13

	// retentionCheckEvery is how often a process asks whether the run of
	// the day is still to be done.
	retentionCheckEvery = time.Hour
	// retentionBatch is how many rows one statement of a run deletes at
	// the most. A day of events is deleted in many short statements, none
	// of which holds the table for long.
	retentionBatch = 5000
	// retentionStatementTimeout bounds one statement of a run.
	retentionStatementTimeout = 30 * time.Second
)

// The turn of the retention run, as a lock of the session: one process
// runs at a time, and a process that dies gives the turn back with its
// connection. The two numbers are a key of their own kind, apart from the
// turns of the links, which are single numbers.
const (
	turnOfTheJobs      = 0x636f7269 // "cori"
	turnOfTheRetention = 1
)

// Retention deletes what has been kept for its time, once a day.
type Retention struct {
	pool   *pgxpool.Pool
	inbox  *ingest.Store
	logger *slog.Logger
	// batch is retentionBatch, but for a test with a handful of rows.
	batch int
}

// NewRetention returns the retention job over a pool. The inbox deletes
// what is of its tables.
func NewRetention(pool *pgxpool.Pool, inbox *ingest.Store, logger *slog.Logger) *Retention {
	return &Retention{pool: pool, inbox: inbox, logger: logger, batch: retentionBatch}
}

// Run does the run of each day until its context is done. It asks when it
// starts and every hour: the run of a day is done by the first process
// that asks after midnight in UTC, and a run that failed is tried again
// the next time.
func (r *Retention) Run(ctx context.Context) {
	sweep(ctx, r.Sweep, r.logger)
}

// sweep asks for runs. It is apart from Run so that a test can say what
// each one finds.
func sweep(ctx context.Context, once func(context.Context) (bool, error), logger *slog.Logger) {
	ticker := time.NewTicker(retentionCheckEvery)
	defer ticker.Stop()
	for {
		if _, err := once(ctx); err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "the retention run failed and will be tried again", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep does the run of the day, unless a run has already finished today,
// in UTC, or another process is in the middle of one. It reports whether
// it did.
func (r *Retention) Sweep(ctx context.Context) (bool, error) {
	turn, err := r.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("take a connection for the retention run: %w", err)
	}
	defer turn.Release()
	var mine bool
	if err := turn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, turnOfTheJobs, turnOfTheRetention).Scan(&mine); err != nil {
		return false, fmt.Errorf("ask for the turn of the retention run: %w", err)
	}
	if !mine {
		return false, nil
	}
	// Given back whatever happened to the run. A connection that cannot
	// say so is closed, which gives the turn back too.
	defer func() {
		back, cancel := context.WithTimeout(context.WithoutCancel(ctx), retentionStatementTimeout)
		defer cancel()
		if _, err := turn.Exec(back, `SELECT pg_advisory_unlock($1, $2)`, turnOfTheJobs, turnOfTheRetention); err != nil {
			_ = turn.Conn().Close(back)
		}
	}()

	var done bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM retention_runs
		                 WHERE finished_at IS NOT NULL
		                   AND started_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')`).Scan(&done); err != nil {
		return false, fmt.Errorf("ask whether the retention run of today is done: %w", err)
	}
	if done {
		return false, nil
	}
	return true, r.run(ctx)
}

// run is one run: a row that says it began, the deletions, and the end.
func (r *Retention) run(ctx context.Context) error {
	var (
		id       string
		raw, day time.Time
	)
	// The moment of the run is the one of the database, whose clock wrote
	// the moments the run compares it with, and so is its calendar: raw
	// is the moment before which raw events are due, and day the midnight
	// in UTC before which aggregates are. Aggregates are of UTC days, and
	// are deleted by whole days.
	err := r.pool.QueryRow(ctx,
		`INSERT INTO retention_runs DEFAULT VALUES
		 RETURNING id::text,
		           started_at - make_interval(secs => $1),
		           (date_trunc('day', started_at AT TIME ZONE 'UTC') - make_interval(months => $2)) AT TIME ZONE 'UTC'`,
		RawEventsFor.Seconds(), AggregatesForMonths).Scan(&id, &raw, &day)
	if err != nil {
		return fmt.Errorf("begin a retention run: %w", err)
	}
	// A day is sent as the one it is in UTC, wherever the process runs.
	day = day.UTC()

	deleted := make(map[string]int)
	for _, step := range []struct {
		column string
		remove func(ctx context.Context) (int, error)
	}{
		{"inbox_events", func(ctx context.Context) (int, error) { return r.inbox.DeleteProcessed(ctx, raw, r.batch) }},
		{"dead_letters", func(ctx context.Context) (int, error) { return r.inbox.DeleteDeadLetters(ctx, raw, r.batch) }},
		{"hourly_rows", func(ctx context.Context) (int, error) {
			return r.change(ctx, `DELETE FROM link_hourly WHERE ctid IN (SELECT ctid FROM link_hourly WHERE hour < $1 LIMIT $2)`, day, r.batch)
		}},
		{"daily_rows", func(ctx context.Context) (int, error) {
			return r.change(ctx, `DELETE FROM link_daily_dimension WHERE ctid IN (SELECT ctid FROM link_daily_dimension WHERE day < $1::date LIMIT $2)`, day, r.batch)
		}},
	} {
		// In batches, until one comes back short. What each deleted is
		// written at once: a run that stops halfway says how far it got.
		for {
			batch, err := r.within(ctx, step.remove)
			if err != nil {
				return err
			}
			deleted[step.column] += batch
			if _, err := r.change(ctx, `UPDATE retention_runs SET `+step.column+` = `+step.column+` + $2 WHERE id = $1::uuid`, id, batch); err != nil {
				return err
			}
			if batch < r.batch {
				break
			}
		}
	}
	// The counters of a day and the runs themselves are a few rows.
	counters, err := r.within(ctx, func(ctx context.Context) (int, error) { return r.inbox.DeleteCounters(ctx, day) })
	if err != nil {
		return err
	}
	if _, err := r.change(ctx, `DELETE FROM retention_runs WHERE started_at < $1`, day); err != nil {
		return err
	}
	if _, err := r.change(ctx, `UPDATE retention_runs SET counters = $2, finished_at = now() WHERE id = $1::uuid`, id, counters); err != nil {
		return err
	}
	r.logger.InfoContext(ctx, "the retention run finished",
		"inbox_events", deleted["inbox_events"], "dead_letters", deleted["dead_letters"],
		"hourly_rows", deleted["hourly_rows"], "daily_rows", deleted["daily_rows"], "counters", counters)
	return nil
}

// within gives one statement of a run its time.
func (r *Retention) within(ctx context.Context, statement func(context.Context) (int, error)) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, retentionStatementTimeout)
	defer cancel()
	return statement(ctx)
}

// change runs one statement of a run on the tables of this package, and
// returns how many rows it changed.
func (r *Retention) change(ctx context.Context, sql string, arguments ...any) (int, error) {
	return r.within(ctx, func(ctx context.Context) (int, error) {
		changed, err := r.pool.Exec(ctx, sql, arguments...)
		if err != nil {
			return 0, fmt.Errorf("retention run: %w", err)
		}
		return int(changed.RowsAffected()), nil
	})
}
