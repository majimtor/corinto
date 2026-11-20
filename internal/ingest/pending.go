package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/majimtor/corinto/internal/postgres"
)

// MaxAttempts is how many times an event is tried before it is set aside.
// docs/delivery-semantics.md states it, and the waits.
const MaxAttempts = 5

// retryWaits is how long an event waits after each failed attempt but the
// last, which has no next one.
var retryWaits = [MaxAttempts - 1]time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}

// ErrNoDeadLetter is the error of sending back to the inbox an event that
// is not among the dead letters.
var ErrNoDeadLetter = errors.New("there is no such dead-lettered event")

// due is the condition of an event that can be tried now: it is pending,
// and it does not wait for its next attempt.
const due = `processed_at IS NULL AND (next_attempt_at IS NULL OR next_attempt_at <= now())`

// Take locks pending events for a transaction and returns them, the oldest
// first and at most a number of them. An event that another transaction
// holds is skipped, not waited for: several consumers take different
// events. So is one that waits for its next attempt. The events stay
// pending until the transaction marks them and commits; one that ends
// otherwise leaves them for the next consumer
// (docs/delivery-semantics.md, statement 4).
func Take(ctx context.Context, tx pgx.Tx, limit int) ([]Event, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text, link_id::text, workspace_id::text, occurred_at, outcome, method, coalesce(referrer_host, ''),
		        device, browser, class, rule, rules_version
		   FROM event_inbox
		  WHERE `+due+`
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

// TakeOne locks one event for a transaction, if it can be tried now and
// nobody else holds it, and reports whether it did.
func TakeOne(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var taken bool
	err := tx.QueryRow(ctx, `SELECT true FROM event_inbox WHERE id = $1::uuid AND `+due+` FOR UPDATE SKIP LOCKED`, id).Scan(&taken)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("take a pending event: %w", err)
	}
	return taken, nil
}

// Fail records that an attempt to process an event failed because the
// database refused it. Before the last attempt the event waits, longer
// each time; after the last it leaves the inbox for the dead letters, with
// the refusal as the reason, and is counted as dead-lettered. It reports
// whether the event was dead-lettered. An event that is no longer pending
// is left as it is.
//
// The caller holds the event in the transaction, from before the attempt:
// two consumers never record the same failure.
func Fail(ctx context.Context, tx pgx.Tx, event Event, refusal postgres.Refusal) (bool, error) {
	var attempts int
	err := tx.QueryRow(ctx, `SELECT attempts FROM event_inbox WHERE id = $1::uuid AND processed_at IS NULL FOR UPDATE`, event.ID).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the attempts of an event: %w", err)
	}
	if attempts+1 < MaxAttempts {
		// By the clock of the database, which is the one that says when
		// the event is due.
		_, err := tx.Exec(ctx,
			`UPDATE event_inbox SET attempts = attempts + 1, next_attempt_at = now() + make_interval(secs => $2) WHERE id = $1::uuid`,
			event.ID, retryWaits[attempts].Seconds())
		if err != nil {
			return false, fmt.Errorf("record a failed attempt: %w", err)
		}
		return false, nil
	}

	moved, err := tx.Exec(ctx,
		`WITH gone AS (DELETE FROM event_inbox WHERE id = $1::uuid RETURNING *)
		 INSERT INTO event_dead_letters (id, link_id, workspace_id, occurred_at, outcome, method, referrer_host,
		                                 device, browser, class, rule, rules_version, accepted_at,
		                                 reason_sqlstate, reason_constraint)
		 SELECT id, link_id, workspace_id, occurred_at, outcome, method, referrer_host,
		        device, browser, class, rule, rules_version, accepted_at, $2, nullif($3, '')
		   FROM gone`,
		event.ID, refusal.Code, refusal.Constraint)
	if err != nil {
		return false, fmt.Errorf("move an event to the dead letters: %w", err)
	}
	if moved.RowsAffected() != 1 {
		return false, fmt.Errorf("move an event to the dead letters: %d rows moved", moved.RowsAffected())
	}
	day := time.Unix(event.OccurredAt.Unix()/secondsInADay*secondsInADay, 0).UTC()
	if _, err := tx.Exec(ctx, addCounts,
		[]time.Time{day}, []string{event.WorkspaceID}, []string{string(counterDeadLettered)}, []int64{1}); err != nil {
		return false, fmt.Errorf("count a dead-lettered event: %w", err)
	}
	return true, nil
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

// oldestPending is the acceptance of the oldest event that still waits: the
// first entry of the index of the events that wait.
const oldestPending = `SELECT accepted_at FROM event_inbox WHERE processed_at IS NULL ORDER BY accepted_at, id LIMIT 1`

// ProcessedThrough returns the moment up to which every accepted event has
// been processed or dead-lettered: the acceptance of the oldest event that
// still waits, or the moment of the question when none does
// (docs/delivery-semantics.md, statement 7). Both are by the clock of the
// database, which is the one that wrote the acceptance.
//
// Whoever answers with figures asks this first and reads the figures
// after: what they hold then is at least what this says.
func (s *Store) ProcessedThrough(ctx context.Context) (time.Time, error) {
	var through time.Time
	err := s.db.QueryRow(ctx, `SELECT coalesce((`+oldestPending+`), now())`).Scan(&through)
	if err != nil {
		return time.Time{}, fmt.Errorf("read how far the events are processed: %w", err)
	}
	return through.UTC(), nil
}
