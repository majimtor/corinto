package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store writes events and counts to PostgreSQL. It is the Inbox of the
// processes.
type Store struct {
	db     executor
	logger *slog.Logger
}

// executor is what the store asks of a pool: every write is one statement.
type executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// NewStore returns the store of the inbox over a pool.
func NewStore(pool *pgxpool.Pool, logger *slog.Logger) *Store {
	return &Store{db: pool, logger: logger}
}

// Accept inserts events into the inbox. An event whose identifier is
// already there is left as it is, so a batch written twice leaves one row
// per event. The count of what was accepted is taken by the insert itself,
// in the same statement: both happen or neither does.
//
// A batch that the database refuses for what it holds is inserted event by
// event, and an event refused on its own is dropped. It returns how many
// events, from the first, are settled, and an error when the database did
// not answer for the rest.
func (s *Store) Accept(ctx context.Context, events []Event) (int, error) {
	err := s.insert(ctx, events)
	if err == nil {
		return len(events), nil
	}
	if !refused(err) {
		return 0, fmt.Errorf("insert a batch of events: %w", err)
	}
	for i := range events {
		err := s.insert(ctx, events[i:i+1])
		if err != nil && !refused(err) {
			return i, fmt.Errorf("insert an event: %w", err)
		}
		if err != nil {
			s.note(ctx, "the inbox refused an event, which is dropped", err)
		}
	}
	return len(events), nil
}

// Values travel as text and are converted by the database, so that one it
// cannot read is refused by it, with a code, like any other it refuses.
const insertEvents = `
WITH accepted AS (
    INSERT INTO event_inbox (id, link_id, workspace_id, occurred_at, outcome, method, referrer_host,
                             device, browser, class, rule, rules_version)
    SELECT e.id::uuid, e.link::uuid, e.workspace::uuid, e.occurred_at, e.outcome, e.method, nullif(e.referrer, ''),
           e.device, e.browser, e.class, e.rule, e.version::integer
      FROM unnest($1::text[], $2::text[], $3::text[], $4::timestamptz[], $5::text[], $6::text[], $7::text[],
                  $8::text[], $9::text[], $10::text[], $11::text[], $12::bigint[])
           AS e (id, link, workspace, occurred_at, outcome, method, referrer, device, browser, class, rule, version)
    ON CONFLICT (id) DO NOTHING
    RETURNING workspace_id, (occurred_at AT TIME ZONE 'UTC')::date AS day
)
INSERT INTO pipeline_counters (day, workspace_id, name, value)
SELECT day, workspace_id, 'accepted', count(*) FROM accepted GROUP BY day, workspace_id
 ORDER BY day, workspace_id
ON CONFLICT ON CONSTRAINT pipeline_counters_key DO UPDATE SET value = pipeline_counters.value + EXCLUDED.value`

func (s *Store) insert(ctx context.Context, events []Event) error {
	var (
		ids, links, workspaces       = make([]string, len(events)), make([]string, len(events)), make([]string, len(events))
		times                        = make([]time.Time, len(events))
		outcomes, methods, referrers = make([]string, len(events)), make([]string, len(events)), make([]string, len(events))
		devices, browsers, classes   = make([]string, len(events)), make([]string, len(events)), make([]string, len(events))
		rules                        = make([]string, len(events))
		versions                     = make([]int64, len(events))
	)
	for i, event := range events {
		ids[i], links[i], workspaces[i] = event.ID, event.LinkID, event.WorkspaceID
		times[i] = event.OccurredAt
		outcomes[i], methods[i], referrers[i] = string(event.Outcome), event.Method, event.ReferrerHost
		devices[i], browsers[i] = string(event.Classification.Device), string(event.Classification.Browser)
		classes[i], rules[i] = string(event.Classification.Class), event.Classification.Rule
		versions[i] = int64(event.Classification.Version)
	}
	_, err := s.db.Exec(ctx, insertEvents,
		ids, links, workspaces, times, outcomes, methods, referrers, devices, browsers, classes, rules, versions)
	return err
}

// Count adds amounts to the counters, each to the row of its day, its
// workspace and its name. Like Accept, it writes one by one what the
// database refuses together, drops what is refused on its own, and returns
// how many counts, from the first, are settled.
func (s *Store) Count(ctx context.Context, counts []Count) (int, error) {
	err := s.add(ctx, counts)
	if err == nil {
		return len(counts), nil
	}
	if !refused(err) {
		return 0, fmt.Errorf("add to the counters: %w", err)
	}
	for i := range counts {
		err := s.add(ctx, counts[i:i+1])
		if err != nil && !refused(err) {
			return i, fmt.Errorf("add to a counter: %w", err)
		}
		if err != nil {
			s.note(ctx, "the database refused a count, which is dropped", err)
		}
	}
	return len(counts), nil
}

// The rows are written in the order of their key: two processes that add
// to the same counters in different orders would wait for each other.
const addCounts = `
INSERT INTO pipeline_counters (day, workspace_id, name, value)
SELECT c.day, nullif(c.workspace, '')::uuid, c.name, c.value
  FROM unnest($1::date[], $2::text[], $3::text[], $4::bigint[]) AS c (day, workspace, name, value)
 ORDER BY c.day, c.workspace, c.name
ON CONFLICT ON CONSTRAINT pipeline_counters_key DO UPDATE SET value = pipeline_counters.value + EXCLUDED.value`

func (s *Store) add(ctx context.Context, counts []Count) error {
	var (
		days       = make([]time.Time, len(counts))
		workspaces = make([]string, len(counts))
		names      = make([]string, len(counts))
		values     = make([]int64, len(counts))
	)
	for i, count := range counts {
		days[i], workspaces[i], names[i], values[i] = count.Day, count.WorkspaceID, string(count.Name), count.Value
	}
	_, err := s.db.Exec(ctx, addCounts, days, workspaces, names, values)
	return err
}

// refused reports whether the database answered that it will not take what
// it was given: a value it cannot read, or one that breaks a constraint.
// Trying again would get the same answer.
func refused(err error) bool {
	var answer *pgconn.PgError
	if !errors.As(err, &answer) {
		return false
	}
	// The classes of SQLSTATE for a data exception and for an integrity
	// constraint violation.
	return strings.HasPrefix(answer.Code, "22") || strings.HasPrefix(answer.Code, "23")
}

// note logs a refusal by its code and the constraint it names, and never
// by its text or its detail, which quote the row.
func (s *Store) note(ctx context.Context, message string, err error) {
	var answer *pgconn.PgError
	if errors.As(err, &answer) {
		s.logger.ErrorContext(ctx, message, "sqlstate", answer.Code, "constraint", answer.ConstraintName)
	}
}
