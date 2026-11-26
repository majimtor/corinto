package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/majimtor/corinto/internal/postgres"
)

// ErrNotDemo means that an event of a workspace that is not a demo was
// about to be set aside by hand.
var ErrNotDemo = errors.New("only an event of a demo workspace is set aside by hand")

// DeleteDemoEvents deletes what the inbox holds of a demo workspace: its
// events, its dead letters and its counts, for the seed that is about to
// delete the workspace. What belongs to a workspace that is not a demo is
// left as it is, and so are the counts that belong to no workspace.
func DeleteDemoEvents(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	for _, table := range []string{"event_inbox", "event_dead_letters", "pipeline_counters"} {
		_, err := tx.Exec(ctx,
			`DELETE FROM `+table+` WHERE workspace_id = $1::uuid AND (SELECT demo FROM workspaces WHERE id = $1::uuid)`, workspaceID)
		if err != nil {
			return fmt.Errorf("delete the rows of a demo workspace in %s: %w", table, err)
		}
	}
	return nil
}

// SetAsideDemoEvent writes an event of a demo workspace straight to the
// dead letters, in the transaction of the caller, and returns its
// identifier. The response is counted as served, the event is accepted and
// then fails every one of its attempts at once, with the refusal the
// caller gives, each through the statement that does it for any other
// event: the reconciliation of its day adds up, and no consumer ever sees
// it pending.
//
// Nothing in the schema refuses an event that a consumer writes, so an
// event of the seed cannot get there by failing. This is how the seed
// shows one, and nothing else calls it.
func SetAsideDemoEvent(ctx context.Context, tx pgx.Tx, event Event, refusal postgres.Refusal) (string, error) {
	var demo bool
	err := tx.QueryRow(ctx, `SELECT demo FROM workspaces WHERE id = $1::uuid`, event.WorkspaceID).Scan(&demo)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !demo) {
		return "", ErrNotDemo
	}
	if err != nil {
		return "", fmt.Errorf("read the workspace of an event: %w", err)
	}

	// As a buffer takes an event: to the second, with an identifier of
	// that second, and the response counted on its day.
	event.OccurredAt = event.OccurredAt.Truncate(time.Second)
	event.ID = newID(event.OccurredAt)
	day := time.Unix(event.OccurredAt.Unix()/secondsInADay*secondsInADay, 0).UTC()
	if _, err := tx.Exec(ctx, addCounts,
		[]time.Time{day}, []string{event.WorkspaceID}, []string{string(counterServed)}, []int64{1}); err != nil {
		return "", fmt.Errorf("count the response of an event that is set aside: %w", err)
	}
	if err := (&Store{db: tx}).insert(ctx, []Event{event}); err != nil {
		return "", fmt.Errorf("accept an event that is set aside: %w", err)
	}
	for range MaxAttempts {
		if _, err := Fail(ctx, tx, event, refusal); err != nil {
			return "", err
		}
	}
	var there bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_dead_letters WHERE id = $1::uuid)`, event.ID).Scan(&there); err != nil {
		return "", fmt.Errorf("read the event that was set aside: %w", err)
	}
	if !there {
		return "", errors.New("an event that failed all its attempts is not among the dead letters")
	}
	return event.ID, nil
}

// DemoEventsPending returns how many events of a workspace wait to be
// processed. The seed asks, to know when its history is counted: a
// consumer of another process may be at it too.
func (s *Store) DemoEventsPending(ctx context.Context, workspaceID string) (int64, error) {
	var pending int64
	err := s.db.QueryRow(ctx,
		`SELECT count(*) FROM event_inbox WHERE workspace_id = $1::uuid AND processed_at IS NULL`, workspaceID).Scan(&pending)
	if err != nil {
		return 0, fmt.Errorf("count the pending events of a workspace: %w", err)
	}
	return pending, nil
}
