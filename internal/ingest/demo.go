package ingest

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

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
