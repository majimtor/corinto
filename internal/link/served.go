package link

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Served is what the redirect needs of a link, and nothing else of it:
// where it leads, whether it redirects, and what an access event names.
type Served struct {
	ID          string
	WorkspaceID string
	Identifier  string
	State       State
	// Resolved is what the redirect answers with. A deleted link has none.
	Resolved  string
	ExpiresAt *time.Time
	// Version tells an older state of the link from a newer one.
	Version int64
}

// Served returns every link of the redirect domain that is not deleted,
// and the time of the database at which they were read.
func (s *Store) Served(ctx context.Context) ([]Served, time.Time, error) {
	return s.served(ctx, `state <> 'deleted'`)
}

// ServedSince returns the links of the redirect domain that changed at a
// moment or after it, deleted ones included, and the time of the database
// at which they were read. A change carries the time at which its
// transaction began, not the one at which it was committed: whoever follows
// the changes asks again for a stretch it has already read.
func (s *Store) ServedSince(ctx context.Context, since time.Time) ([]Served, time.Time, error) {
	return s.served(ctx, `updated_at >= $2`, since)
}

func (s *Store) served(ctx context.Context, condition string, arguments ...any) ([]Served, time.Time, error) {
	// One snapshot for the time and for the rows. The time is the one at
	// which the transaction began, which is not later than the snapshot:
	// whoever asks again from it misses nothing the snapshot did not see.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("begin the reading of the served links: %w", err)
	}
	// Nothing was written: there is nothing to commit.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&at); err != nil {
		return nil, time.Time{}, fmt.Errorf("read the time of the database: %w", err)
	}
	rows, err := tx.Query(ctx,
		`SELECT id, workspace_id, identifier, state, coalesce(resolved_destination, ''), expires_at, version
		   FROM links WHERE domain = $1 AND `+condition,
		append([]any{s.domain}, arguments...)...)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read the served links: %w", err)
	}
	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Served, error) {
		var (
			served        Served
			id, workspace pgtype.UUID
		)
		err := row.Scan(&id, &workspace, &served.Identifier, &served.State, &served.Resolved, &served.ExpiresAt, &served.Version)
		served.ID, served.WorkspaceID = id.String(), workspace.String()
		return served, err
	})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read the served links: %w", err)
	}
	return links, at, nil
}
