package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Workspace is what is read from a workspace before something is created
// in it.
type Workspace struct {
	MayChooseAliases bool
	// Allowed is the destination policy: the entries a destination must
	// match. An empty list allows any destination.
	Allowed []AllowedPrefix
	// The quotas. Each bounds what the workspace holds of one kind, and the
	// package that owns the kind does the counting.
	MaxLinks int
	MaxKeys  int
}

// LockWorkspace reads an active workspace and locks its row until the
// transaction ends. Whatever is created under a quota goes through it:
// creations in one workspace wait for each other, so a count taken after
// this call cannot be passed by two of them at once, and the workspace
// cannot be blocked halfway. A workspace that does not exist and one that
// is blocked are errors.
func LockWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) (Workspace, error) {
	id, ok := parseID(workspaceID)
	if !ok {
		return Workspace{}, ErrWorkspaceNotFound
	}
	var (
		workspace Workspace
		state     string
		allowlist []byte
	)
	err := tx.QueryRow(ctx,
		`SELECT state, may_choose_aliases, allowed_destinations, max_links, max_keys
		   FROM workspaces WHERE id = $1 FOR UPDATE`, id,
	).Scan(&state, &workspace.MayChooseAliases, &allowlist, &workspace.MaxLinks, &workspace.MaxKeys)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, ErrWorkspaceNotFound
	}
	if err != nil {
		return Workspace{}, fmt.Errorf("read the workspace: %w", err)
	}
	if state != "active" {
		return Workspace{}, ErrWorkspaceBlocked
	}
	if err := json.Unmarshal(allowlist, &workspace.Allowed); err != nil {
		return Workspace{}, fmt.Errorf("read the allowlist of the workspace: %w", err)
	}
	return workspace, nil
}
