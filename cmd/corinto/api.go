package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/health"
)

// runAPI runs the role api. In this slice the role has no operations yet: it
// holds the database pool and answers its health endpoints.
func runAPI(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	env := environment{getenv: getenv}
	databaseURL := env.require("CORINTO_DATABASE_URL")
	healthAddr := env.require("CORINTO_HEALTH_ADDR")
	if err := env.err(); err != nil {
		return err
	}

	// The pool connects on first use, so the process starts even when
	// PostgreSQL is not up yet and says so through its readiness endpoint.
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("configure the database pool: %w", err)
	}
	defer pool.Close()

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", healthAddr)
	if err != nil {
		return fmt.Errorf("listen for health checks on %s: %w", healthAddr, err)
	}
	logger.InfoContext(ctx, "health endpoints listening", "addr", listener.Addr().String())

	return health.Serve(ctx, listener, pool.Ping, logger)
}
