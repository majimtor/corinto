package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DeleteDemoKeys deletes the keys of a demo workspace, rows and all, for
// the seed that is about to delete the workspace. The keys of a workspace
// that is not a demo are left as they are.
func DeleteDemoKeys(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	_, err := tx.Exec(ctx,
		`DELETE FROM api_keys WHERE workspace_id = $1::uuid AND (SELECT demo FROM workspaces WHERE id = $1::uuid)`, workspaceID)
	if err != nil {
		return fmt.Errorf("delete the keys of a demo workspace: %w", err)
	}
	return nil
}
