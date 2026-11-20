//go:build integration

package auth_test

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/majimtor/corinto/internal/auth"
)

// The keys of a demo workspace are deleted, rows and all, for the seed
// that creates the workspace again. The keys of any other are not.
func TestOnlyTheKeysOfADemoWorkspaceAreDeleted(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	demo, ordinary := workspace(t, pool, 10), workspace(t, pool, 10)
	if _, err := pool.Exec(t.Context(), `UPDATE workspaces SET demo = true WHERE id = $1::uuid`, demo); err != nil {
		t.Fatal(err)
	}
	_, shown := issue(t, keys, demo, auth.ScopeLinksRead)
	revoked, _ := issue(t, keys, demo, auth.ScopeLinksRead)
	if err := keys.Revoke(t.Context(), demo, revoked.ID); err != nil {
		t.Fatal(err)
	}
	_, theirs := issue(t, keys, ordinary, auth.ScopeLinksRead)

	for _, workspaceID := range []string{ordinary, demo} {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.DeleteDemoKeys(t.Context(), tx, workspaceID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	count := func(workspaceID string) int {
		t.Helper()
		var rows int
		if err := pool.QueryRow(context.WithoutCancel(t.Context()), `SELECT count(*) FROM api_keys WHERE workspace_id = $1::uuid`, workspaceID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	// The one that was revoked too: a key that was revoked keeps its row,
	// and a workspace with a row of a key cannot be deleted.
	if rows := count(demo); rows != 0 {
		t.Errorf("the demo workspace still has %d keys", rows)
	}
	refused(t, keys, shown)
	if rows := count(ordinary); rows != 1 {
		t.Errorf("the workspace that is not a demo has %d keys, want its one", rows)
	}
	if _, err := keys.Authenticate(t.Context(), theirs); err != nil {
		t.Errorf("the key of the workspace that is not a demo is refused: %v", err)
	}
}
