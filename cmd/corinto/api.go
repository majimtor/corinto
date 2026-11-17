package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/api"
	"github.com/majimtor/corinto/internal/health"
	"github.com/majimtor/corinto/internal/postgres"
)

// schemaCheckTimeout bounds the check of the schema version at start, so
// that a database that does not answer stops the start instead of hanging
// it.
const schemaCheckTimeout = 10 * time.Second

// runAPI runs the role api: it checks the schema, holds the database pool,
// serves the management API on one port and answers the health endpoints on
// another.
func runAPI(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	env := environment{getenv: getenv}
	databaseURL := env.require("CORINTO_DATABASE_URL")
	apiAddr := env.require("CORINTO_API_ADDR")
	healthAddr := env.require("CORINTO_HEALTH_ADDR")
	if err := env.err(); err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("configure the database pool: %w", err)
	}
	defer pool.Close()

	// A process does not start against a schema it was not built with, and
	// so it does not start without the database either: there is no way to
	// know. Once started, an outage shows in the readiness endpoint.
	checkCtx, cancel := context.WithTimeout(ctx, schemaCheckTimeout)
	defer cancel()
	if err := postgres.CheckSchema(checkCtx, pool); err != nil {
		return err
	}

	// Built before anything listens: a server that does not serve exactly
	// its contract does not start.
	handler, err := api.New(ctx, logger)
	if err != nil {
		return err
	}

	var lc net.ListenConfig
	apiListener, err := lc.Listen(ctx, "tcp", apiAddr)
	if err != nil {
		return fmt.Errorf("listen for the API on %s: %w", apiAddr, err)
	}
	healthListener, err := lc.Listen(ctx, "tcp", healthAddr)
	if err != nil {
		_ = apiListener.Close()
		return fmt.Errorf("listen for health checks on %s: %w", healthAddr, err)
	}
	logger.InfoContext(ctx, "api listening", "addr", apiListener.Addr().String())
	logger.InfoContext(ctx, "health endpoints listening", "addr", healthListener.Addr().String())

	// The two servers live and stop together: when one ends, for whatever
	// reason, the other is told to, and the process waits for both.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	ended := make(chan error, 2)
	go func() { ended <- api.Serve(ctx, apiListener, handler, logger) }()
	go func() { ended <- health.Serve(ctx, healthListener, pool.Ping, logger) }()
	first := <-ended
	stop()
	return errors.Join(first, <-ended)
}
