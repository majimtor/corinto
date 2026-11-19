package ingest

import (
	"context"
	"fmt"
	"time"
)

// DeleteProcessed deletes events of the inbox that were processed and were
// accepted before a moment, the oldest first and a number of them at the
// most, and returns how many they were. An event that still waits is never
// deleted, however old: it is something to look at, not something to
// forget.
func (s *Store) DeleteProcessed(ctx context.Context, before time.Time, limit int) (int, error) {
	deleted, err := s.db.Exec(ctx,
		`DELETE FROM event_inbox
		  WHERE id IN (SELECT id FROM event_inbox
		                WHERE processed_at IS NOT NULL AND accepted_at < $1
		                ORDER BY accepted_at
		                LIMIT $2)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("delete processed events: %w", err)
	}
	return int(deleted.RowsAffected()), nil
}

// DeleteDeadLetters deletes dead-lettered events that were set aside
// before a moment, a number of them at the most, and returns how many they
// were. They stay counted as dead-lettered: the count is not of this
// table.
func (s *Store) DeleteDeadLetters(ctx context.Context, before time.Time, limit int) (int, error) {
	deleted, err := s.db.Exec(ctx,
		`DELETE FROM event_dead_letters
		  WHERE id IN (SELECT id FROM event_dead_letters WHERE dead_lettered_at < $1 LIMIT $2)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("delete dead-lettered events: %w", err)
	}
	return int(deleted.RowsAffected()), nil
}

// DeleteCounters deletes the counters of the days before a day, given as
// its midnight in UTC, and returns how many rows they were.
func (s *Store) DeleteCounters(ctx context.Context, before time.Time) (int, error) {
	deleted, err := s.db.Exec(ctx, `DELETE FROM pipeline_counters WHERE day < $1::date`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete counters: %w", err)
	}
	return int(deleted.RowsAffected()), nil
}
