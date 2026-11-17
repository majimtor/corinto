//go:build integration

package link_test

import (
	"crypto/rand"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/link"
)

// stored is what the block of a link changes in its row.
type stored struct {
	State   link.State
	Version int64
}

// states returns the state and the version of every link of a workspace,
// by the identifier of its row.
func states(t *testing.T, pool *pgxpool.Pool, workspaceID string) map[string]stored {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT id, state, version FROM links WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := make(map[string]stored)
	for rows.Next() {
		var (
			id  string
			row stored
		)
		if err := rows.Scan(&id, &row.State, &row.Version); err != nil {
			t.Fatal(err)
		}
		found[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

// create stores a link that must be stored.
func create(t *testing.T, store *link.Store, workspaceID string, draft link.Draft) link.Link {
	t.Helper()
	if draft.Destination == "" {
		draft.Destination = "https://example.com/"
	}
	created, err := store.Create(t.Context(), workspaceID, draft)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// blockWorkspace blocks a workspace in a transaction of its own and returns
// how many links were blocked.
func blockWorkspace(t *testing.T, pool *pgxpool.Pool, workspaceID string) int {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	_, links, err := link.BlockWorkspace(t.Context(), tx, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return links
}

func TestBlockEndsTheLinkThatAShortIdentifierNames(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	elsewhere := link.NewStore(pool, "other.example.dev", rand.Reader)
	owner := workspace(t, pool, ", may_choose_aliases", ", true")

	// The link of the other domain is the older one: a block that forgot
	// the domain would find it first.
	sameAlias := create(t, elsewhere, owner, link.Draft{Alias: "spring-sale"})
	reported := create(t, store, owner, link.Draft{Alias: "spring-sale"})
	bystander := create(t, store, owner, link.Draft{})

	blocked, err := store.Block(t.Context(), "spring-sale")
	if err != nil {
		t.Fatal(err)
	}
	if blocked.ID != reported.ID || blocked.State != link.StateBlocked || blocked.Version != reported.Version+1 ||
		blocked.UpdatedAt.Before(reported.UpdatedAt) || blocked.Destination != reported.Destination {
		t.Errorf("the link blocked is %+v", blocked)
	}
	after := states(t, pool, owner)
	if after[sameAlias.ID].State != link.StateActive || after[sameAlias.ID].Version != 1 {
		t.Error("the link with the same identifier on another domain was touched")
	}
	if after[bystander.ID].State != link.StateActive || after[bystander.ID].Version != 1 {
		t.Error("another link of the workspace was touched")
	}

	// Blocking it again is not a change: the replica has nothing to apply.
	again, err := store.Block(t.Context(), "spring-sale")
	if err != nil || again.Version != blocked.Version || !again.UpdatedAt.Equal(blocked.UpdatedAt) {
		t.Errorf("blocking again: version %d and %v, want version %d and no error", again.Version, err, blocked.Version)
	}
}

func TestBlockReachesADisabledLinkAndNoDeletedOne(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")

	disabled := create(t, store, owner, link.Draft{})
	if _, err := store.Disable(t.Context(), owner, disabled.ID); err != nil {
		t.Fatal(err)
	}
	if blocked, err := store.Block(t.Context(), disabled.Identifier); err != nil || blocked.State != link.StateBlocked {
		t.Errorf("a disabled link: state %q and %v, want it blocked", blocked.State, err)
	}

	deleted := create(t, store, owner, link.Draft{})
	if err := store.Delete(t.Context(), owner, deleted.ID); err != nil {
		t.Fatal(err)
	}
	for name, identifier := range map[string]string{
		"a deleted link":                  deleted.Identifier,
		"an identifier nobody has":        "zzzzzzzz",
		"the identifier of a row":         disabled.ID,
		"an identifier in the wrong case": "SPRING-SALE",
		"nothing":                         "",
	} {
		t.Run(name, func(t *testing.T) {
			if blocked, err := store.Block(t.Context(), identifier); !errors.Is(err, link.ErrNotFound) || blocked.ID != "" {
				t.Errorf("err = %v with the link %q, want %v and none", err, blocked.ID, link.ErrNotFound)
			}
		})
	}
	if after := states(t, pool, owner); after[deleted.ID].State != link.StateDeleted {
		t.Errorf("the deleted link is now %q", after[deleted.ID].State)
	}
}

func TestBlockWorkspaceBlocksItsLinksAndNothingElse(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner, stranger := workspace(t, pool, "", ""), workspace(t, pool, "", "")

	active := create(t, store, owner, link.Draft{})
	disabled := create(t, store, owner, link.Draft{})
	if _, err := store.Disable(t.Context(), owner, disabled.ID); err != nil {
		t.Fatal(err)
	}
	alreadyBlocked := create(t, store, owner, link.Draft{})
	if _, err := store.Block(t.Context(), alreadyBlocked.Identifier); err != nil {
		t.Fatal(err)
	}
	deleted := create(t, store, owner, link.Draft{})
	if err := store.Delete(t.Context(), owner, deleted.ID); err != nil {
		t.Fatal(err)
	}
	foreign := create(t, store, stranger, link.Draft{})
	before := states(t, pool, owner)

	// A block that is not committed blocks nothing.
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := link.BlockWorkspace(t.Context(), tx, owner); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"}); err != nil {
		t.Fatalf("after a block that was undone, creating gives %v", err)
	}
	extra := 1

	if links := blockWorkspace(t, pool, owner); links != 2+extra {
		t.Errorf("%d links blocked, want %d: the active ones and the disabled one", links, 2+extra)
	}
	after := states(t, pool, owner)
	for name, id := range map[string]string{"the active link": active.ID, "the disabled link": disabled.ID} {
		if after[id].State != link.StateBlocked || after[id].Version != before[id].Version+1 {
			t.Errorf("%s is %q at version %d, want it blocked at version %d",
				name, after[id].State, after[id].Version, before[id].Version+1)
		}
	}
	if after[alreadyBlocked.ID] != before[alreadyBlocked.ID] {
		t.Errorf("the link that was already blocked changed: %+v", after[alreadyBlocked.ID])
	}
	if after[deleted.ID] != before[deleted.ID] {
		t.Errorf("the deleted link changed: %+v", after[deleted.ID])
	}
	if others := states(t, pool, stranger); others[foreign.ID].State != link.StateActive || others[foreign.ID].Version != 1 {
		t.Errorf("the link of another workspace is %+v", others[foreign.ID])
	}

	if _, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"}); !errors.Is(err, link.ErrWorkspaceBlocked) {
		t.Errorf("creating in the blocked workspace: err = %v, want %v", err, link.ErrWorkspaceBlocked)
	}
	if _, err := store.Create(t.Context(), stranger, link.Draft{Destination: "https://example.com/"}); err != nil {
		t.Errorf("creating in another workspace: err = %v, want none", err)
	}

	// Blocking it again changes nothing.
	if links := blockWorkspace(t, pool, owner); links != 0 {
		t.Errorf("blocking again blocked %d links, want none", links)
	}
	for id, row := range states(t, pool, owner) {
		if row != after[id] {
			t.Errorf("blocking again changed the link %s: %+v", id, row)
		}
	}
}

func TestBlockWorkspaceNamesTheWorkspaceOrRefuses(t *testing.T) {
	pool := database(t)
	owner := workspace(t, pool, "", "")

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	for name, id := range map[string]string{
		"a workspace that does not exist":   "00000000-0000-7000-8000-000000000000",
		"something that is not a workspace": "acme",
	} {
		if _, _, err := link.BlockWorkspace(t.Context(), tx, id); !errors.Is(err, link.ErrWorkspaceNotFound) {
			t.Errorf("%s: err = %v, want %v", name, err, link.ErrWorkspaceNotFound)
		}
	}
	name, links, err := link.BlockWorkspace(t.Context(), tx, owner)
	if err != nil || name != "Acme" || links != 0 {
		t.Errorf("BlockWorkspace = %q, %d, %v; want the name of the workspace and no link", name, links, err)
	}
}

// A creation either commits before the block, and its link is blocked with
// the rest, or waits for it and is refused. No link is left that redirects
// in a blocked workspace.
func TestNoLinkOutlivesTheBlockOfItsWorkspace(t *testing.T) {
	pool := database(t)
	store := link.NewStore(pool, redirectDomain, rand.Reader)
	owner := workspace(t, pool, "", "")

	var wait sync.WaitGroup
	for range 20 {
		wait.Go(func() {
			_, err := store.Create(t.Context(), owner, link.Draft{Destination: "https://example.com/"})
			if err != nil && !errors.Is(err, link.ErrWorkspaceBlocked) {
				t.Errorf("a creation failed with %v", err)
			}
		})
	}
	wait.Go(func() { blockWorkspace(t, pool, owner) })
	wait.Wait()

	for id, row := range states(t, pool, owner) {
		if row.State != link.StateBlocked {
			t.Errorf("the link %s is %q in a blocked workspace", id, row.State)
		}
	}
}
