package link

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Block blocks the link that a short identifier names, whatever its
// workspace, and returns it. It is the operator's: what a report of abuse
// carries is the short link, not the identifier of a row. A link that is
// already blocked is returned as it is.
func (s *Store) Block(ctx context.Context, identifier string) (Link, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Link{}, fmt.Errorf("begin the block of a link: %w", err)
	}
	// After a commit this does nothing; before one it undoes everything.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	current, err := scan(tx.QueryRow(ctx,
		`SELECT `+columns+` FROM links
		  WHERE domain = $1 AND identifier = $2 AND state <> 'deleted' FOR UPDATE`, s.domain, identifier))
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("read the link: %w", err)
	}
	// Nothing changes, so the version stays and the replica of the redirect
	// has nothing to apply.
	if current.State == StateBlocked {
		return current, nil
	}

	blocked, err := scan(tx.QueryRow(ctx,
		`UPDATE links SET state = 'blocked', version = version + 1, updated_at = now()
		  WHERE id = $1 RETURNING `+columns, current.ID))
	if err != nil {
		return Link{}, fmt.Errorf("block the link: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Link{}, fmt.Errorf("commit the block of a link: %w", err)
	}
	return blocked, nil
}

// BlockWorkspace blocks a workspace and every link it has, in the
// transaction of the caller, and returns the name of the workspace and how
// many links it blocked. The block is written in each link, with a new
// version, so that the replica of the redirect learns of it the way it
// learns of any change and has one source to follow. Whoever calls it ends
// the keys of the workspace in the same transaction. A workspace that is
// already blocked is not an error.
func BlockWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) (string, int, error) {
	id, ok := parseID(workspaceID)
	if !ok {
		return "", 0, ErrWorkspaceNotFound
	}
	// The update locks the row, so a creation in the workspace either
	// commits before it, and its link is blocked below, or waits for it and
	// is refused.
	var name string
	err := tx.QueryRow(ctx,
		`UPDATE workspaces
		    SET state = 'blocked', updated_at = CASE WHEN state = 'blocked' THEN updated_at ELSE now() END
		  WHERE id = $1 RETURNING name`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrWorkspaceNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("block the workspace: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE links SET state = 'blocked', version = version + 1, updated_at = now()
		  WHERE workspace_id = $1 AND state NOT IN ('deleted', 'blocked')`, id)
	if err != nil {
		return "", 0, fmt.Errorf("block the links of the workspace: %w", err)
	}
	return name, int(tag.RowsAffected()), nil
}
