package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/seed"
)

// The options of the seed: the one that makes it do nothing when the
// database already has a demo workspace, and the one that leaves the demo
// workspaces without a history.
const (
	seedIfMissing = "--if-missing"
	seedNoHistory = "--no-history"
)

// seedOptions reads the options of the seed, each of which is given once
// at the most, in any order. It reports whether they are options it has.
func seedOptions(args []string) (seed.Options, bool) {
	var options seed.Options
	for _, arg := range args {
		switch {
		case arg == seedIfMissing && !options.OnlyIfMissing:
			options.OnlyIfMissing = true
		case arg == seedNoHistory && !options.WithoutHistory:
			options.WithoutHistory = true
		default:
			return seed.Options{}, false
		}
	}
	return options, true
}

// runSeed deletes the demo workspaces and creates the ones of the seed
// again, with their links, a key each and a history of the ninety days
// before, which it writes and waits to see counted. It prints the keys
// once, as one JSON object on standard output: only a hash of each secret
// is stored, so what is printed is the only copy. Nothing of a key goes to
// the log.
//
// Asked to seed only if the demo workspaces are missing, it does that only
// when the database has none. When it has one it changes nothing, says so
// in the log and prints an object with no workspace in it. Asked for no
// history, it writes none.
func runSeed(ctx context.Context, options seed.Options, getenv func(string) string, stdout io.Writer, logger *slog.Logger) error {
	env := environment{getenv: getenv}
	databaseURL := env.require("CORINTO_DATABASE_URL")
	redirectDomain := env.require("CORINTO_REDIRECT_DOMAIN")
	if err := env.err(); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("configure the database pool: %w", err)
	}
	defer pool.Close()
	checkCtx, cancel := context.WithTimeout(ctx, schemaCheckTimeout)
	defer cancel()
	if err := postgres.CheckSchema(checkCtx, pool); err != nil {
		return err
	}

	seeded, err := seed.Run(ctx, pool, link.NewStore(pool, redirectDomain, rand.Reader), auth.NewKeys(pool, rand.Reader),
		logger, time.Now(), options)
	if err != nil {
		return err
	}
	if options.OnlyIfMissing && seeded == nil {
		logger.InfoContext(ctx, "the demo workspaces are there already; nothing was seeded")
	}
	type printedWorkspace struct {
		ID    string   `json:"id"`
		Name  string   `json:"name"`
		KeyID string   `json:"key_id"`
		Key   string   `json:"key"`
		Links []string `json:"links"`
	}
	printed := struct {
		Workspaces []printedWorkspace `json:"workspaces"`
	}{make([]printedWorkspace, len(seeded))}
	for i, workspace := range seeded {
		printed.Workspaces[i] = printedWorkspace{workspace.ID, workspace.Name, workspace.KeyID, workspace.Key, workspace.Links}
		// Logged before it is printed, like a key the operator issues: if
		// the output cannot be written, the log still names the key, and a
		// key nobody saw can be revoked.
		logger.InfoContext(ctx, "demo workspace seeded",
			"workspace", workspace.ID, "key", workspace.KeyID, "links", len(workspace.Links), "events", workspace.Events)
	}
	if err := json.NewEncoder(stdout).Encode(printed); err != nil {
		return fmt.Errorf("print the keys of the seed: %w", err)
	}
	return nil
}
