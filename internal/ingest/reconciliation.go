package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// DefaultDays is how many days a reconciliation covers when it is not
	// told how many.
	DefaultDays = 7
	// MaxDays is how many days a reconciliation covers at the most.
	MaxDays = 31
)

// ErrDays means that a reconciliation was asked for of no day, or of more
// days than it covers.
var ErrDays = errors.New("a reconciliation covers from 1 to 31 days")

// Day is what became of the events that the responses of one UTC day owed
// a workspace: the terms of the two equalities of
// docs/delivery-semantics.md, counted by the day of the access.
type Day struct {
	// Day is the day, at its midnight in UTC.
	Day time.Time
	// Served is the responses that owed an event, and Accepted the events
	// that reached the inbox. Lost is the difference, never stored and
	// never below zero.
	Served, Accepted, Lost int64
	// Processed, Pending and DeadLettered are what became of the accepted
	// events: in the figures, waiting, or set aside.
	Processed, Pending, DeadLettered int64
}

// Reconciliation returns the terms of a workspace for each of its last
// days, today by the clock of the database included, the oldest first. A
// day in which nothing happened is there, with zeros.
//
// The counts and the events that wait are read by one statement, and so at
// one moment: accepted is processed, pending and dead-lettered added up in
// every answer, also while events are being processed. The events that
// wait are counted, not worked out from the others: that is what makes the
// equality worth reading.
func (s *Store) Reconciliation(ctx context.Context, workspaceID string, days int) ([]Day, error) {
	if days < 1 || days > MaxDays {
		return nil, ErrDays
	}
	rows, err := s.db.Query(ctx,
		`WITH span AS (
		     SELECT (now() AT TIME ZONE 'UTC')::date - n AS day FROM generate_series(0, $2::int - 1) AS n
		 ), counted AS (
		     SELECT day, name, value FROM pipeline_counters
		      WHERE workspace_id = $1::uuid AND day > (now() AT TIME ZONE 'UTC')::date - $2::int
		 ), waiting AS (
		     SELECT (occurred_at AT TIME ZONE 'UTC')::date AS day, count(*) AS events
		       FROM event_inbox
		      WHERE workspace_id = $1::uuid AND processed_at IS NULL
		      GROUP BY 1
		 )
		 SELECT s.day,
		        coalesce(max(c.value) FILTER (WHERE c.name = $3), 0)::bigint,
		        coalesce(max(c.value) FILTER (WHERE c.name = 'accepted'), 0)::bigint,
		        coalesce(max(c.value) FILTER (WHERE c.name = $4), 0)::bigint,
		        coalesce(max(c.value) FILTER (WHERE c.name = $5), 0)::bigint,
		        coalesce((SELECT w.events FROM waiting AS w WHERE w.day = s.day), 0)::bigint
		   FROM span AS s LEFT JOIN counted AS c ON c.day = s.day
		  GROUP BY s.day
		  ORDER BY s.day`,
		workspaceID, days, string(counterServed), string(counterProcessed), string(counterDeadLettered))
	if err != nil {
		return nil, fmt.Errorf("read the reconciliation: %w", err)
	}
	terms := make([]Day, 0, days)
	var day Day
	_, err = pgx.ForEachRow(rows, []any{&day.Day, &day.Served, &day.Accepted, &day.Processed, &day.DeadLettered, &day.Pending}, func() error {
		day.Lost = max(day.Served-day.Accepted, 0)
		terms = append(terms, day)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the reconciliation: %w", err)
	}
	return terms, nil
}
