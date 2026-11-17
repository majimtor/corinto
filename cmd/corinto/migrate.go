package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/postgres"
)

// runMigrate applies the migrations the database does not have yet and
// exits. It is the only command that changes the schema.
func runMigrate(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	env := environment{getenv: getenv}
	databaseURL := env.require("CORINTO_DATABASE_URL")
	if err := env.err(); err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("configure the database pool: %w", err)
	}
	defer pool.Close()

	applied, err := postgres.Migrate(ctx, pool)
	for _, migration := range applied {
		logger.InfoContext(ctx, "migration applied",
			"version", migration.Version, "name", migration.Name, "duration", migration.Duration.String())
	}
	if err != nil {
		return err
	}
	built, err := postgres.BuiltVersion()
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "schema up to date", "version", built, "applied", len(applied))
	return nil
}
