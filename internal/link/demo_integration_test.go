//go:build integration

package link_test

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/link"
)

// within runs something in a transaction and commits it.
func within(t *testing.T, pool *pgxpool.Pool, do func(tx pgx.Tx) error) error {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	if err := do(tx); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}

func rowsOf(t *testing.T, pool *pgxpool.Pool, table, workspaceID string) int {
	t.Helper()
	var rows int
	column := "workspace_id"
	if table == "workspaces" {
		column = "id"
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE `+column+` = $1::uuid`, workspaceID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// A demo workspace is deleted with its links, rows and all, for the seed
// that creates it again. No other workspace is.
func TestOnlyADemoWorkspaceIsDeletedWithItsLinks(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	demo := workspace(t, pool, ", demo, may_choose_aliases", ", true, true")
	other := workspace(t, pool, ", demo", ", true")
	ordinary := workspace(t, pool, ", may_choose_aliases", ", true")
	for _, owner := range []string{demo, other, ordinary} {
		for range 2 {
			if _, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	named, err := store.Create(t.Context(), demo, link.Draft{Alias: "spring-sale", Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	// Whatever state its links are in: one that its workspace deleted, and
	// keeps as a row without its content, and one the operator blocked.
	if err := store.Delete(t.Context(), demo, named.ID); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.Create(t.Context(), demo, link.Draft{Alias: "summer-sale", Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Block(t.Context(), blocked.Identifier); err != nil {
		t.Fatal(err)
	}

	// The demo workspaces are the two that are, in the order of their
	// identifiers, and while a transaction holds them nothing is written
	// under them.
	holder, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	found, err := link.DemoWorkspaces(t.Context(), holder)
	want := []string{demo, other}
	slices.Sort(want)
	if err != nil || !slices.Equal(found, want) {
		t.Fatalf("DemoWorkspaces = %v, %v; want %v", found, err, want)
	}
	err = within(t, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SET LOCAL lock_timeout = '300ms'`); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(),
			`INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination)
			 VALUES ($1::uuid, $2, 'late-link', 'https://example.com/', 'https://example.com/')`, demo, redirectDomain)
		return err
	})
	var waited *pgconn.PgError
	if !errors.As(err, &waited) || waited.Code != "55P03" {
		t.Errorf("a link written under a demo workspace that is held ended with %v, want it to wait", err)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	// What is not a demo, or is no workspace, is left as it is, also by a
	// caller that commits whatever it is told.
	for name, id := range map[string]string{
		"a workspace that is not a demo": ordinary, "a workspace nobody has": "00000000-0000-7000-8000-000000000000", "what is not an identifier": "acme",
	} {
		var refusal error
		if err := within(t, pool, func(tx pgx.Tx) error {
			refusal = link.DeleteDemoWorkspace(t.Context(), tx, id)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(refusal, link.ErrNotDemo) {
			t.Errorf("%s: DeleteDemoWorkspace = %v, want %v", name, refusal, link.ErrNotDemo)
		}
	}
	if links, there := rowsOf(t, pool, "links", ordinary), rowsOf(t, pool, "workspaces", ordinary); links != 2 || there != 1 {
		t.Fatalf("the workspace that is not a demo has %d links and %d rows, want 2 and itself", links, there)
	}

	if err := within(t, pool, func(tx pgx.Tx) error { return link.DeleteDemoWorkspace(t.Context(), tx, demo) }); err != nil {
		t.Fatal(err)
	}
	if links, there := rowsOf(t, pool, "links", demo), rowsOf(t, pool, "workspaces", demo); links != 0 || there != 0 {
		t.Errorf("the demo workspace that was deleted has %d links and %d rows", links, there)
	}
	// The other demo, and the workspace that is not one, are as they were.
	if links, theirs := rowsOf(t, pool, "links", other), rowsOf(t, pool, "links", ordinary); links != 2 || theirs != 2 {
		t.Errorf("the other workspaces have %d and %d links, want 2 each", links, theirs)
	}
	// Nothing of its links is kept: an identifier one of them had, even one
	// its workspace had deleted, can be issued again.
	for _, alias := range []string{"spring-sale", "summer-sale"} {
		if _, err := store.Create(t.Context(), ordinary, link.Draft{Alias: alias, Destination: "https://example.com/"}); err != nil {
			t.Errorf("the identifier %s of a deleted demo workspace cannot be issued again: %v", alias, err)
		}
	}
}

// A demo workspace that still holds something else is not deleted: the
// database refuses, and whoever deletes has to delete that first.
func TestADemoWorkspaceThatStillHoldsKeysIsNotDeleted(t *testing.T) {
	pool := database(t)
	demo := workspace(t, pool, ", demo", ", true")
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO api_keys (workspace_id, name, secret_hash, scopes) VALUES ($1::uuid, 'Seed key', decode(repeat('ab', 32), 'hex'), '{links:read}')`, demo); err != nil {
		t.Fatal(err)
	}
	err := within(t, pool, func(tx pgx.Tx) error { return link.DeleteDemoWorkspace(t.Context(), tx, demo) })
	var refusal *pgconn.PgError
	if !errors.As(err, &refusal) || refusal.Code != "23503" || rowsOf(t, pool, "workspaces", demo) != 1 {
		t.Errorf("DeleteDemoWorkspace of a workspace with a key = %v, want the database to refuse it and the workspace to stay", err)
	}
}
