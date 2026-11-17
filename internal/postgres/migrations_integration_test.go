//go:build integration

package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

// The code PostgreSQL gives to a row without a value it requires.
const notNullViolation = "23502"

// The quota of keys arrived in the second migration. A workspace created
// before it takes the initial value; one created after it must be given
// the value, because the column keeps no default.
func TestAWorkspaceOlderThanTheQuotaOfKeysTakesTheInitialOne(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, postgrestest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	// Only this test stops halfway: nothing outside the tests applies the
	// migrations up to a version.
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	var older string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, max_links) VALUES ('Acme', 1000) RETURNING id`).Scan(&older); err != nil {
		t.Fatal(err)
	}

	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var quota int
	if err := pool.QueryRow(ctx, `SELECT max_keys FROM workspaces WHERE id = $1`, older).Scan(&quota); err != nil {
		t.Fatal(err)
	}
	if quota != 10 {
		t.Errorf("the older workspace has a quota of %d keys, want 10", quota)
	}

	_, err = pool.Exec(ctx, `INSERT INTO workspaces (name, max_links) VALUES ('Acme', 1000)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != notNullViolation {
		t.Errorf("a new workspace without a quota of keys: err = %v, want the code %s", err, notNullViolation)
	}
}
