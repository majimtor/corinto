// Package seed fills a database with the demo workspaces: made-up
// workspaces, links and keys with which whoever runs the system can try
// it (PLAN.md section 8).
package seed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/rollup"
)

const (
	// The turn of the seed: one run at a time, so that two of them do not
	// delete what the other is creating. The first number is the one this
	// project gives to the turns of its jobs.
	turnOfTheJobs = 0x636f7269
	turnOfTheSeed = 2

	// keyName is the name of the key each demo workspace gets.
	keyName = "Seed key"
)

// ErrBusy means that another seed is running.
var ErrBusy = errors.New("another seed is running")

// Workspace is a demo workspace as the seed left it.
type Workspace struct {
	ID   string
	Name string
	// KeyID names the key of the workspace, and Key is its credential:
	// what its holder sends. The credential is in this value and nowhere
	// else, and cannot be read again.
	KeyID string
	Key   string
	// Links are the short identifiers of its links.
	Links []string
}

// Run deletes the demo workspaces with everything they hold and creates
// the ones of the seed again, each with its links and a key that may do
// everything in it. No workspace that is not a demo is touched.
//
// The links are created through the store, by the rules of any other
// link. The one that has expired did so a number of days before the day
// of now, so that two runs on one day leave the same links.
//
// A redirect that is running holds its own copy of the links, and keeps
// the old ones until it reads the whole copy again, within five minutes,
// or is started again.
func Run(ctx context.Context, pool *pgxpool.Pool, links *link.Store, keys *auth.Keys, now time.Time) ([]Workspace, error) {
	turn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("take a connection for the seed: %w", err)
	}
	defer turn.Release()
	var mine bool
	if err := turn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, turnOfTheJobs, turnOfTheSeed).Scan(&mine); err != nil {
		return nil, fmt.Errorf("ask for the turn of the seed: %w", err)
	}
	if !mine {
		return nil, ErrBusy
	}
	// Given back whatever happened. A connection that cannot say so is
	// closed, which gives the turn back too.
	defer func() {
		back, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := turn.Exec(back, `SELECT pg_advisory_unlock($1, $2)`, turnOfTheJobs, turnOfTheSeed); err != nil {
			_ = turn.Conn().Close(back)
		}
	}()

	if err := forget(ctx, pool); err != nil {
		return nil, err
	}
	day := now.UTC().Truncate(24 * time.Hour)
	seeded := make([]Workspace, 0, len(demos))
	for _, demo := range demos {
		planted, err := plant(ctx, pool, links, keys, demo, day)
		if err != nil {
			return nil, fmt.Errorf("seed the workspace %s: %w", demo.name, err)
		}
		seeded = append(seeded, planted)
	}
	return seeded, nil
}

// forget deletes every demo workspace with what it holds, in one
// transaction: all of them are gone or none is. Each table is deleted from
// by the package that owns it, the ones that refer to others first.
func forget(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the deletion of the demo workspaces: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	ids, err := link.DemoWorkspaces(ctx, tx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ingest.DeleteDemoEvents(ctx, tx, id); err != nil {
			return err
		}
		if err := rollup.DeleteDemoAggregates(ctx, tx, id); err != nil {
			return err
		}
		if err := auth.DeleteDemoKeys(ctx, tx, id); err != nil {
			return err
		}
		if err := link.DeleteDemoWorkspace(ctx, tx, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the deletion of the demo workspaces: %w", err)
	}
	return nil
}

// plant creates one demo workspace with its links and its key.
func plant(ctx context.Context, pool *pgxpool.Pool, links *link.Store, keys *auth.Keys, demo demo, day time.Time) (Workspace, error) {
	workspace, err := link.CreateWorkspace(ctx, pool, link.WorkspaceDraft{
		Name: demo.name, Demo: true, MayChooseAliases: true,
		MaxLinks: limits.MaxLinksDemo, MaxKeys: limits.MaxKeys,
	})
	if err != nil {
		return Workspace{}, err
	}
	planted := Workspace{ID: workspace.ID, Name: workspace.Name, Links: make([]string, 0, len(demo.links))}
	for _, entry := range demo.links {
		draft := link.Draft{
			Alias: entry.alias, Destination: entry.destination, Campaign: entry.campaign,
			Title: entry.title, Tags: entry.tags,
		}
		if entry.expiredDaysAgo > 0 {
			expired := day.AddDate(0, 0, -entry.expiredDaysAgo)
			draft.ExpiresAt = &expired
		}
		created, err := links.Create(ctx, workspace.ID, draft)
		if err != nil {
			return Workspace{}, fmt.Errorf("create the link %s: %w", entry.alias, err)
		}
		planted.Links = append(planted.Links, created.Identifier)
	}
	key, credential, err := keys.Issue(ctx, workspace.ID, auth.Draft{Name: keyName, Scopes: auth.Scopes()})
	if err != nil {
		return Workspace{}, fmt.Errorf("issue its key: %w", err)
	}
	planted.KeyID, planted.Key = key.ID, credential
	return planted, nil
}
