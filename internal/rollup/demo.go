package rollup

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DeleteDemoAggregates deletes the time series and the breakdowns of the
// links of a demo workspace, for the seed that is about to delete the
// workspace. The aggregates of a workspace that is not a demo are left as
// they are.
func DeleteDemoAggregates(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	for _, table := range []string{"link_hourly", "link_daily_dimension"} {
		_, err := tx.Exec(ctx,
			`DELETE FROM `+table+` WHERE workspace_id = $1::uuid AND (SELECT demo FROM workspaces WHERE id = $1::uuid)`, workspaceID)
		if err != nil {
			return fmt.Errorf("delete the rows of a demo workspace in %s: %w", table, err)
		}
	}
	return nil
}
