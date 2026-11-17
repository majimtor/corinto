// Package postgres holds what every process needs from the database before
// it does anything else: the schema, as forward-only migrations embedded in
// the binary, and the check that the database is at the version the binary
// was built with. The tables and the queries belong to the packages that
// own them.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// The migrations. A file is immutable once merged: migrations/SUMS holds the
// checksum of each one and a test compares them.
//
//go:embed migrations/*.sql
var embedded embed.FS

// undefinedTable is the code PostgreSQL gives to a query over a table that
// does not exist.
const undefinedTable = "42P01"

// ErrSchemaVersion means that the database is not at the version this binary
// was built with.
var ErrSchemaVersion = errors.New("the schema of the database is not the one this binary was built with")

// Applied is one migration that Migrate ran.
type Applied struct {
	Version  int64
	Name     string
	Duration time.Duration
}

func migrations() fs.FS {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		// The directory is embedded by the directive above.
		panic(err)
	}
	return sub
}

// Migrate applies the migrations the database does not have yet, in order,
// and returns the ones it applied. It is the only thing that creates or
// alters tables. A migration that fails stops the run.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]Applied, error) {
	// The library speaks database/sql. Closing this handle returns its
	// connections to the pool and leaves the pool open.
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations())
	if err != nil {
		return nil, fmt.Errorf("read the migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	applied := make([]Applied, 0, len(results))
	for _, result := range results {
		if result.Error == nil {
			applied = append(applied, Applied{
				Version:  result.Source.Version,
				Name:     result.Source.Path,
				Duration: result.Duration,
			})
		}
	}
	if err != nil {
		return applied, fmt.Errorf("apply the migrations: %w", err)
	}
	return applied, nil
}

// BuiltVersion returns the version of the last migration embedded in this
// binary: the number its file name starts with.
func BuiltVersion() (int64, error) {
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		return 0, fmt.Errorf("read the migrations: %w", err)
	}
	var last int64
	for _, entry := range entries {
		number, _, found := strings.Cut(entry.Name(), "_")
		version, err := strconv.ParseInt(number, 10, 64)
		if !found || err != nil || version < 1 {
			return 0, fmt.Errorf("the migration %q is not named with a number, an underscore and a name", entry.Name())
		}
		last = max(last, version)
	}
	if last == 0 {
		return 0, errors.New("the binary embeds no migration")
	}
	return last, nil
}

// CheckSchema fails unless the database is at the version this binary was
// built with. Every process calls it before it starts, and it only reads:
// a process other than migrate never creates anything.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	built, err := BuiltVersion()
	if err != nil {
		return err
	}
	current, err := databaseVersion(ctx, pool)
	if err != nil {
		return fmt.Errorf("read the schema version of the database: %w", err)
	}
	if current != built {
		return fmt.Errorf("%w: the database is at version %d and the binary at %d; run corinto migrate",
			ErrSchemaVersion, current, built)
	}
	return nil
}

// databaseVersion returns the last migration applied to the database, or
// zero for a database that was never migrated.
func databaseVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var version int64
	// The table is the one the migration library keeps. It is read here and
	// not through the library, which would create it when it is missing.
	err := pool.QueryRow(ctx,
		`SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`,
	).Scan(&version)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
		return 0, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return version, err
}
