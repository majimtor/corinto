// Package rollup adds the events of the inbox to the aggregates: the time
// series of each link and its breakdowns (PLAN.md section 5). From
// acceptance on, each event counts exactly once
// (docs/delivery-semantics.md, statements 4 and 6).
package rollup

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/postgres"
)

const (
	// BatchSize is how many pending events a consumer takes at once.
	// docs/delivery-semantics.md states it.
	BatchSize = 500
	// MaxReferrerHosts is how many referrer hosts a link keeps for a day.
	// The host comes from the requester, who can write as many as it
	// likes; past this number they are counted together.
	MaxReferrerHosts = 100
	// OtherReferrers is the value of the referrer hosts past the cap. No
	// host has a parenthesis, so it cannot be mistaken for one.
	OtherReferrers = "(other)"
	// NoReferrer is the value of the clicks that named no referrer.
	NoReferrer = "(none)"

	// PollEvery is how often a consumer asks for pending events when the
	// last time it found less than a batch.
	PollEvery = time.Second

	// batchTimeout bounds the transaction of a batch, so that a database
	// that hangs is a failed batch and not a consumer that stopped.
	batchTimeout = 10 * time.Second
	// idleTimeout is how long the database lets a transaction of a
	// consumer sit without a statement before it ends it. A consumer that
	// hangs between two statements holds the rows it wrote; this gives
	// them back to the others, and its events to the next attempt.
	idleTimeout = 30 * time.Second

	secondsInAnHour = 60 * 60
	secondsInADay   = 24 * secondsInAnHour
)

// Dimension is one of the breakdowns of a link.
type Dimension string

// The dimensions. A breakdown is of one of them, never of two crossed.
const (
	DimensionReferrerHost Dimension = "referrer_host"
	DimensionDevice       Dimension = "device"
	DimensionBrowser      Dimension = "browser"
)

// Consumer takes pending events from the inbox and adds them to the
// aggregates. Several can run at once, in one process or in many.
type Consumer struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	// idle is idleTimeout, but for a test that cannot wait that long.
	idle time.Duration
}

// NewConsumer returns a consumer over a pool.
func NewConsumer(pool *pgxpool.Pool, logger *slog.Logger) *Consumer {
	return &Consumer{pool: pool, logger: logger, idle: idleTimeout}
}

// Run processes the inbox until its context is done: a batch, another at
// once when that one was whole, and otherwise the next one PollEvery
// later. A batch that fails is pending and is tried again like any other.
func (c *Consumer) Run(ctx context.Context) {
	run(ctx, c.Batch, c.logger)
}

// run asks for batches. It is apart from Run so that a test can say what
// each batch finds.
func run(ctx context.Context, batch func(context.Context) (int, error), logger *slog.Logger) {
	ticker := time.NewTicker(PollEvery)
	defer ticker.Stop()
	failing := false
	for {
		processed, err := batch(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			// The process is stopping: the batch was cut off, not refused,
			// and its events are pending.
		case err != nil && !failing:
			logger.WarnContext(ctx, "a batch of the inbox could not be processed, and its events stay pending", "error", err.Error())
		case err == nil && failing:
			logger.InfoContext(ctx, "the inbox is being processed again")
		}
		failing = err != nil
		// A whole batch says that more may wait. One that failed
		// processed nothing, and waits for the interval like any other.
		if processed == BatchSize {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Batch takes up to BatchSize pending events, adds them to the aggregates
// and marks them as processed, in one transaction: both happen or neither
// does. It returns how many events it processed; none means that nothing
// was pending, or that other consumers hold what is.
//
// When the database refuses the batch for something it holds, the events
// are tried one by one, each in its own transaction, so that one bad event
// does not hold the others back. Only the failure of an event on its own
// counts as one of its attempts, and only a refusal is a failure: a
// database that is away, a deadlock or a batch that ran out of time leave
// the events as they were, to be tried again
// (docs/delivery-semantics.md, statement 5).
func (c *Consumer) Batch(ctx context.Context) (int, error) {
	events, err := c.together(ctx)
	if err == nil {
		return len(events), nil
	}
	if _, refused := postgres.Refused(err); !refused {
		return 0, err
	}
	processed := 0
	for _, event := range events {
		alone, err := c.alone(ctx, event)
		if err != nil {
			return processed, err
		}
		if alone {
			processed++
		}
	}
	return processed, nil
}

// begin begins a transaction of a consumer.
func (c *Consumer) begin(ctx context.Context) (pgx.Tx, error) {
	// Read committed: a consumer that waited for another to finish with a
	// link has to see what the other wrote.
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin a transaction: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('idle_in_transaction_session_timeout', $1, true)`,
		strconv.FormatInt(c.idle.Milliseconds(), 10)); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("bound the idle time of a transaction: %w", err)
	}
	return tx, nil
}

// together processes a batch in one transaction. It returns the events it
// took, also when it fails: they are the ones to try one by one.
func (c *Consumer) together(ctx context.Context) ([]ingest.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()
	tx, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	// After a commit this does nothing. Before it, the events go back to
	// pending for whoever comes next.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	events, err := ingest.Take(ctx, tx, BatchSize)
	if err != nil || len(events) == 0 {
		return nil, err
	}
	if err := add(ctx, tx, events); err != nil {
		return events, err
	}
	if err := ingest.MarkProcessed(ctx, tx, events); err != nil {
		return events, err
	}
	if err := tx.Commit(ctx); err != nil {
		return events, fmt.Errorf("commit a batch: %w", err)
	}
	return events, nil
}

// alone processes one event in a transaction of its own, and reports
// whether it was processed. An event that the database refuses is left
// with one attempt more, or among the dead letters after the last; one
// that somebody else took meanwhile is theirs.
func (c *Consumer) alone(ctx context.Context, event ingest.Event) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()
	tx, err := c.begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The event is held from here to the end, so that its failure is
	// recorded by whoever saw it and by nobody else.
	taken, err := ingest.TakeOne(ctx, tx, event.ID)
	if err != nil || !taken {
		return false, err
	}
	// The attempt is a part of the transaction that can be undone on its
	// own: what it wrote goes, and the event stays held.
	attempt, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin an attempt: %w", err)
	}
	err = add(ctx, attempt, []ingest.Event{event})
	if err == nil {
		err = ingest.MarkProcessed(ctx, attempt, []ingest.Event{event})
	}
	if err == nil {
		if err := attempt.Commit(ctx); err != nil {
			return false, fmt.Errorf("end an attempt: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit an event: %w", err)
		}
		return true, nil
	}

	refusal, refused := postgres.Refused(err)
	if !refused {
		return false, err
	}
	if err := attempt.Rollback(ctx); err != nil {
		return false, fmt.Errorf("undo an attempt: %w", err)
	}
	dead, err := ingest.Fail(ctx, tx, event, refusal)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit a failed attempt: %w", err)
	}
	// By its identifier, which is what the operator sends back to the
	// inbox, and by the answer of the database without its text.
	if dead {
		c.logger.ErrorContext(ctx, "an event failed on every attempt and is among the dead letters",
			"event", event.ID, "sqlstate", refusal.Code, "constraint", refusal.Constraint)
	} else {
		c.logger.WarnContext(ctx, "an event could not be processed and will be tried again",
			"event", event.ID, "sqlstate", refusal.Code, "constraint", refusal.Constraint)
	}
	return false, nil
}

// series is the key of a row of the time series.
type series struct {
	link, workspace string
	// hour is the number of the hour, counted from 1970.
	hour    int64
	outcome ingest.Outcome
	class   classify.Class
}

// slice is the key of a row of a breakdown.
type slice struct {
	linkDay
	dimension Dimension
	value     string
}

// linkDay is a link on a UTC day.
type linkDay struct {
	link, workspace string
	// day is the number of the day, counted from 1970.
	day int64
}

// add adds events to the aggregates. Aggregates are sums: the order of
// the events does not matter.
//
// Every statement writes its rows in the order of their key, and the
// statements follow each other in one order. Consumers that change the
// same rows then wait for each other in a line, and never in a circle.
func add(ctx context.Context, tx pgx.Tx, events []ingest.Event) error {
	hourly := make(map[series]int64)
	daily := make(map[slice]int64)
	// The clicks that named a referrer, by host, until it is known which
	// hosts the link keeps for the day.
	referred := make(map[linkDay]map[string]int64)
	for _, event := range events {
		seconds := event.OccurredAt.Unix()
		hourly[series{event.LinkID, event.WorkspaceID, seconds / secondsInAnHour, event.Outcome, event.Classification.Class}]++
		// The breakdowns are of the estimated human clicks, and of nothing
		// else.
		if event.Outcome != ingest.OutcomeRedirected || event.Classification.Class != classify.ClassPresumedHuman {
			continue
		}
		on := linkDay{event.LinkID, event.WorkspaceID, seconds / secondsInADay}
		daily[slice{on, DimensionDevice, string(event.Classification.Device)}]++
		daily[slice{on, DimensionBrowser, string(event.Classification.Browser)}]++
		if event.ReferrerHost == "" {
			daily[slice{on, DimensionReferrerHost, NoReferrer}]++
			continue
		}
		if referred[on] == nil {
			referred[on] = make(map[string]int64)
		}
		referred[on][event.ReferrerHost]++
	}

	if err := addHourly(ctx, tx, hourly); err != nil {
		return err
	}
	if len(referred) > 0 {
		kept, err := keptHosts(ctx, tx, slices.Collect(maps.Keys(referred)))
		if err != nil {
			return err
		}
		for on, clicks := range referred {
			if kept[on] == nil {
				kept[on] = make(map[string]bool)
			}
			// In the order of their names, so that which hosts are kept
			// does not depend on how the events came.
			for _, host := range slices.Sorted(maps.Keys(clicks)) {
				value := host
				switch {
				case kept[on][host]:
				case len(kept[on]) < MaxReferrerHosts:
					kept[on][host] = true
				default:
					value = OtherReferrers
				}
				daily[slice{on, DimensionReferrerHost, value}] += clicks[host]
			}
		}
	}
	return addDaily(ctx, tx, daily)
}

func addHourly(ctx context.Context, tx pgx.Tx, hourly map[series]int64) error {
	var (
		links, workspaces = make([]string, 0, len(hourly)), make([]string, 0, len(hourly))
		hours             = make([]time.Time, 0, len(hourly))
		outcomes, classes = make([]string, 0, len(hourly)), make([]string, 0, len(hourly))
		events            = make([]int64, 0, len(hourly))
	)
	for key, count := range hourly {
		links, workspaces = append(links, key.link), append(workspaces, key.workspace)
		hours = append(hours, time.Unix(key.hour*secondsInAnHour, 0).UTC())
		outcomes, classes = append(outcomes, string(key.outcome)), append(classes, string(key.class))
		events = append(events, count)
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO link_hourly (link_id, workspace_id, hour, outcome, class, events)
		 SELECT a.link::uuid, a.workspace::uuid, a.hour, a.outcome, a.class, a.events
		   FROM unnest($1::text[], $2::text[], $3::timestamptz[], $4::text[], $5::text[], $6::bigint[])
		        AS a (link, workspace, hour, outcome, class, events)
		  ORDER BY a.link::uuid, a.hour, a.outcome, a.class
		 ON CONFLICT (link_id, hour, outcome, class) DO UPDATE SET events = link_hourly.events + EXCLUDED.events`,
		links, workspaces, hours, outcomes, classes, events)
	if err != nil {
		return fmt.Errorf("add to the time series: %w", err)
	}
	return nil
}

func addDaily(ctx context.Context, tx pgx.Tx, daily map[slice]int64) error {
	if len(daily) == 0 {
		return nil
	}
	var (
		links, workspaces  = make([]string, 0, len(daily)), make([]string, 0, len(daily))
		days               = make([]time.Time, 0, len(daily))
		dimensions, values = make([]string, 0, len(daily)), make([]string, 0, len(daily))
		events             = make([]int64, 0, len(daily))
	)
	for key, count := range daily {
		links, workspaces = append(links, key.link), append(workspaces, key.workspace)
		days = append(days, time.Unix(key.day*secondsInADay, 0).UTC())
		dimensions, values = append(dimensions, string(key.dimension)), append(values, key.value)
		events = append(events, count)
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO link_daily_dimension (link_id, workspace_id, day, dimension, value, events)
		 SELECT s.link::uuid, s.workspace::uuid, s.day, s.dimension, s.value, s.events
		   FROM unnest($1::text[], $2::text[], $3::date[], $4::text[], $5::text[], $6::bigint[])
		        AS s (link, workspace, day, dimension, value, events)
		  ORDER BY s.link::uuid, s.day, s.dimension, s.value
		 ON CONFLICT (link_id, day, dimension, value) DO UPDATE SET events = link_daily_dimension.events + EXCLUDED.events`,
		links, workspaces, days, dimensions, values, events)
	if err != nil {
		return fmt.Errorf("add to the breakdowns: %w", err)
	}
	return nil
}

// keptHosts returns the referrer hosts that links already keep for a day.
// It first takes, for the transaction, the turn of each link and day:
// whoever adds a host to one has counted the ones it keeps, and nobody
// else adds one in between. The cap is exact, with one consumer or with
// many.
func keptHosts(ctx context.Context, tx pgx.Tx, pairs []linkDay) (map[linkDay]map[string]bool, error) {
	// The turns are taken in the order of their number, like every other
	// lock here.
	turns := make([]int64, 0, len(pairs))
	var (
		links      = make([]string, 0, len(pairs))
		days       = make([]time.Time, 0, len(pairs))
		workspaces = make(map[linkDay]string, len(pairs))
	)
	for _, pair := range pairs {
		hash := fnv.New64a()
		_, _ = fmt.Fprintf(hash, "%s/%d", pair.link, pair.day)
		turns = append(turns, int64(hash.Sum64())) //nolint:gosec // Only its bits matter: it names a lock.
		links, days = append(links, pair.link), append(days, time.Unix(pair.day*secondsInADay, 0).UTC())
		workspaces[linkDay{link: pair.link, day: pair.day}] = pair.workspace
	}
	slices.Sort(turns)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(turn) FROM unnest($1::bigint[]) AS turn`, slices.Compact(turns)); err != nil {
		return nil, fmt.Errorf("take the turn of a link and a day: %w", err)
	}

	rows, err := tx.Query(ctx,
		`SELECT d.link_id::text, d.day, d.value
		   FROM link_daily_dimension AS d
		   JOIN unnest($1::text[], $2::date[]) AS p (link, day) ON d.link_id = p.link::uuid AND d.day = p.day
		  WHERE d.dimension = $3 AND d.value <> $4`,
		links, days, string(DimensionReferrerHost), NoReferrer)
	if err != nil {
		return nil, fmt.Errorf("read the referrer hosts that are kept: %w", err)
	}
	// The row of the clicks without a referrer is not a host. The one of
	// the hosts past the cap is there only when the hundred are, and then
	// it changes nothing.
	kept := make(map[linkDay]map[string]bool)
	var (
		link, host string
		day        time.Time
	)
	_, err = pgx.ForEachRow(rows, []any{&link, &day, &host}, func() error {
		on := linkDay{link: link, day: day.Unix() / secondsInADay}
		on.workspace = workspaces[on]
		if kept[on] == nil {
			kept[on] = make(map[string]bool)
		}
		kept[on][host] = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the referrer hosts that are kept: %w", err)
	}
	return kept, nil
}
