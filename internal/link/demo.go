package link

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNotDemo means that a workspace that is not a demo was about to be
// deleted.
var ErrNotDemo = errors.New("only a demo workspace is deleted with all it holds")

// DemoWorkspaces returns the demo workspaces and holds them for the
// transaction: until it ends, nothing new is written under them.
func DemoWorkspaces(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM workspaces WHERE demo ORDER BY id FOR UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("read the demo workspaces: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the demo workspaces: %w", err)
	}
	return ids, nil
}

// DeleteDemoWorkspace deletes a demo workspace and its links, rows and
// all. It is what the seed does before it creates them again, and it is
// not what a workspace does when it deletes a link: nothing is kept, and
// the identifiers of the links can be issued again. Whatever else belongs
// to the workspace has to be gone first.
//
// A workspace that is not a demo is left as it is, with ErrNotDemo.
func DeleteDemoWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	id, ok := parseID(workspaceID)
	if !ok {
		return ErrNotDemo
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM links WHERE workspace_id = $1 AND (SELECT demo FROM workspaces WHERE id = $1)`, id); err != nil {
		return fmt.Errorf("delete the links of a demo workspace: %w", err)
	}
	deleted, err := tx.Exec(ctx, `DELETE FROM workspaces WHERE id = $1 AND demo`, id)
	if err != nil {
		return fmt.Errorf("delete a demo workspace: %w", err)
	}
	if deleted.RowsAffected() != 1 {
		return ErrNotDemo
	}
	return nil
}
