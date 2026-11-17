package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/health"
	"github.com/majimtor/corinto/internal/postgres"
)

// schemaCheckTimeout bounds the check of the schema version at start, so
// that a database that does not answer stops the start instead of hanging
// it.
const schemaCheckTimeout = 10 * time.Second

// runAPI runs the role api. The role has no operations yet: it checks the
// schema, holds the database pool and answers its health endpoints.
func runAPI(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	env := environment{getenv: getenv}
	databaseURL := env.require("CORINTO_DATABASE_URL")
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

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", healthAddr)
	if err != nil {
		return fmt.Errorf("listen for health checks on %s: %w", healthAddr, err)
	}
	logger.InfoContext(ctx, "health endpoints listening", "addr", listener.Addr().String())

	return health.Serve(ctx, listener, pool.Ping, logger)
}
