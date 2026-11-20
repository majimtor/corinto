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

// seedIfMissing is the option that makes the seed do nothing when the
// database already has a demo workspace.
const seedIfMissing = "--if-missing"

// runSeed deletes the demo workspaces and creates the ones of the seed
// again, with their links and a key each. It prints the keys once, as one
// JSON object on standard output: only a hash of each secret is stored, so
// what is printed is the only copy. Nothing of a key goes to the log.
//
// With onlyIfMissing it does that only when the database has no demo
// workspace. When it has one it changes nothing, says so in the log and
// prints an object with no workspace in it.
func runSeed(ctx context.Context, onlyIfMissing bool, getenv func(string) string, stdout io.Writer, logger *slog.Logger) error {
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

	plant := seed.Run
	if onlyIfMissing {
		plant = seed.RunIfMissing
	}
	seeded, err := plant(ctx, pool, link.NewStore(pool, redirectDomain, rand.Reader), auth.NewKeys(pool, rand.Reader), time.Now())
	if err != nil {
		return err
	}
	if onlyIfMissing && seeded == nil {
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
			"workspace", workspace.ID, "key", workspace.KeyID, "links", len(workspace.Links))
	}
	if err := json.NewEncoder(stdout).Encode(printed); err != nil {
		return fmt.Errorf("print the keys of the seed: %w", err)
	}
	return nil
}
