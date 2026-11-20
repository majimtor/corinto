package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/health"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/rollup"
)

// runWorker runs the role worker: it adds the events of the inbox to the
// aggregates, deletes once a day what has been kept for its time, and
// answers the health endpoints.
func runWorker(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
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

	// Like the api: no start against a schema the binary was not built
	// with, and so none without the database.
	checkCtx, cancel := context.WithTimeout(ctx, schemaCheckTimeout)
	defer cancel()
	if err := postgres.CheckSchema(checkCtx, pool); err != nil {
		return err
	}

	var lc net.ListenConfig
	healthListener, err := lc.Listen(ctx, "tcp", healthAddr)
	if err != nil {
		return fmt.Errorf("listen for health checks on %s: %w", healthAddr, err)
	}
	logger.InfoContext(ctx, "health endpoints listening", "addr", healthListener.Addr().String())

	// The consumer, the retention job and the health endpoints live and
	// stop together.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		rollup.NewConsumer(pool, logger).Run(ctx)
	}()
	logger.InfoContext(ctx, "the inbox is being consumed")
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		rollup.NewRetention(pool, ingest.NewStore(pool, logger), logger).Run(ctx)
	}()
	// Ready while the database answers: without it nothing is processed,
	// and what is accepted waits.
	err = health.Serve(ctx, healthListener, pool.Ping, nil, logger)
	stop()
	<-consumed
	<-swept
	return err
}
