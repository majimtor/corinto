package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Take locks pending events for a transaction and returns them, the oldest
// first and at most a number of them. An event that another transaction
// holds is skipped, not waited for: several consumers take different
// events. The events stay pending until the transaction marks them and
// commits; one that ends otherwise leaves them for the next consumer
// (docs/delivery-semantics.md, statement 4).
func Take(ctx context.Context, tx pgx.Tx, limit int) ([]Event, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text, link_id::text, workspace_id::text, occurred_at, outcome, method, coalesce(referrer_host, ''),
		        device, browser, class, rule, rules_version
		   FROM event_inbox
		  WHERE processed_at IS NULL
		  ORDER BY accepted_at, id
		  LIMIT $1
		    FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("take pending events: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var event Event
		err := row.Scan(&event.ID, &event.LinkID, &event.WorkspaceID, &event.OccurredAt, &event.Outcome, &event.Method,
			&event.ReferrerHost, &event.Classification.Device, &event.Classification.Browser,
			&event.Classification.Class, &event.Classification.Rule, &event.Classification.Version)
		event.OccurredAt = event.OccurredAt.UTC()
		return event, err
	})
	if err != nil {
		return nil, fmt.Errorf("take pending events: %w", err)
	}
	return events, nil
}

// MarkProcessed marks events that a transaction took as processed, and
// counts them on the day of their access. Whoever added them to the
// aggregates calls it in the same transaction, so that both happen or
// neither does.
func MarkProcessed(ctx context.Context, tx pgx.Tx, events []Event) error {
	ids := make([]string, len(events))
	processed := make(map[tally]int64)
	for i, event := range events {
		ids[i] = event.ID
		processed[tally{event.OccurredAt.Unix() / secondsInADay, event.WorkspaceID, counterProcessed}]++
	}
	marked, err := tx.Exec(ctx, `UPDATE event_inbox SET processed_at = now() WHERE id = ANY($1::uuid[]) AND processed_at IS NULL`, ids)
	if err != nil {
		return fmt.Errorf("mark events as processed: %w", err)
	}
	if marked.RowsAffected() != int64(len(events)) {
		// The transaction holds these rows: nobody else can have marked
		// them, so what was handed over is not what was taken.
		return fmt.Errorf("mark events as processed: %d of %d were pending", marked.RowsAffected(), len(events))
	}

	var (
		days       = make([]time.Time, 0, len(processed))
		workspaces = make([]string, 0, len(processed))
		names      = make([]string, 0, len(processed))
		values     = make([]int64, 0, len(processed))
	)
	for key, value := range processed {
		days = append(days, time.Unix(key.day*secondsInADay, 0).UTC())
		workspaces, names, values = append(workspaces, key.workspace), append(names, string(key.name)), append(values, value)
	}
	if _, err := tx.Exec(ctx, addCounts, days, workspaces, names, values); err != nil {
		return fmt.Errorf("count the events processed: %w", err)
	}
	return nil
}
