package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/health"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/redirect"
	"github.com/majimtor/corinto/internal/resolve"
)

// runRedirect runs the role redirect: it loads a copy of the links, keeps
// it up to date and answers on the host of the short links from it.
func runRedirect(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	env := environment{getenv: getenv}
	databaseURL := env.require("CORINTO_DATABASE_URL")
	redirectAddr := env.require("CORINTO_REDIRECT_ADDR")
	healthAddr := env.require("CORINTO_HEALTH_ADDR")
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

	// Without its links the process would answer that none exists. It does
	// not start without the database; once it has started, it does not
	// need it to answer.
	links := resolve.New(link.NewStore(pool, redirectDomain, rand.Reader), logger)
	if err := links.Load(ctx); err != nil {
		return fmt.Errorf("load the links: %w", err)
	}
	logger.InfoContext(ctx, "links loaded", "links", links.Len())

	var lc net.ListenConfig
	redirectListener, err := lc.Listen(ctx, "tcp", redirectAddr)
	if err != nil {
		return fmt.Errorf("listen for redirects on %s: %w", redirectAddr, err)
	}
	healthListener, err := lc.Listen(ctx, "tcp", healthAddr)
	if err != nil {
		_ = redirectListener.Close()
		return fmt.Errorf("listen for health checks on %s: %w", healthAddr, err)
	}
	logger.InfoContext(ctx, "redirect listening", "addr", redirectListener.Addr().String())
	logger.InfoContext(ctx, "health endpoints listening", "addr", healthListener.Addr().String())

	// The servers live and stop together, and the copy stops being
	// refreshed with them.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	go links.Run(ctx)
	// Ready from here on, with the database or without it: the process
	// answers from its copy, and how old the copy is goes to the log.
	ready := func(context.Context) error { return nil }
	ended := make(chan error, 2)
	go func() { ended <- redirect.Serve(ctx, redirectListener, redirect.New(links, logger, time.Now), logger) }()
	go func() { ended <- health.Serve(ctx, healthListener, ready, logger) }()
	first := <-ended
	stop()
	return errors.Join(first, <-ended)
}
