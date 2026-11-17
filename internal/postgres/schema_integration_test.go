//go:build integration

package postgres_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

// The codes PostgreSQL gives to the violations the schema must produce.
const (
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
	checkViolation      = "23514"
)

func TestMain(m *testing.M) { postgrestest.Main(m) }

// connect returns a pool on an empty database of its own.
func connect(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), postgrestest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// migrated returns a pool on a database with the whole schema.
func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := connect(t)
	if _, err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// violation runs a statement that must fail and returns the code of its
// error.
func violation(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	_, err := pool.Exec(t.Context(), sql, args...)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("the statement did not fail in the database: %v\n%s", err, sql)
	}
	return pgErr.Code
}

func TestMigrateAppliesEverythingOnceAndThenNothing(t *testing.T) {
	pool := connect(t)
	built, err := postgres.BuiltVersion()
	if err != nil {
		t.Fatal(err)
	}

	applied, err := postgres.Migrate(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(applied)) != built || applied[len(applied)-1].Version != built {
		t.Errorf("the first run applied %d migrations up to %d, want %d", len(applied), applied[len(applied)-1].Version, built)
	}
	again, err := postgres.Migrate(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("the second run applied %d migrations, want none", len(again))
	}
}

func TestCheckSchemaOnlyAcceptsTheVersionOfTheBinary(t *testing.T) {
	pool := connect(t)

	if err := postgres.CheckSchema(t.Context(), pool); !errors.Is(err, postgres.ErrSchemaVersion) {
		t.Errorf("an empty database: err = %v, want %v", err, postgres.ErrSchemaVersion)
	}
	// The check only reads. A process other than migrate creates nothing,
	// not even the table that keeps the version.
	var created bool
	if err := pool.QueryRow(t.Context(), `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("checking the schema created the version table")
	}

	if _, err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if err := postgres.CheckSchema(t.Context(), pool); err != nil {
		t.Errorf("a migrated database: err = %v, want none", err)
	}
}

func TestTheSchemaKeepsTheRulesOfALink(t *testing.T) {
	pool := migrated(t)
	ctx := t.Context()

	var workspace string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, max_links) VALUES ('Acme', 1000) RETURNING id`).Scan(&workspace); err != nil {
		t.Fatal(err)
	}
	const insert = `INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination)
		VALUES ($1, 'go.example.dev', $2, 'https://example.com/', 'https://example.com/') RETURNING id`
	var first string
	if err := pool.QueryRow(ctx, insert, workspace, "k7m2xq9d").Scan(&first); err != nil {
		t.Fatal(err)
	}

	t.Run("two links cannot share a domain and an identifier", func(t *testing.T) {
		if code := violation(t, pool, insert, workspace, "k7m2xq9d"); code != uniqueViolation {
			t.Errorf("code = %s, want %s", code, uniqueViolation)
		}
	})
	t.Run("the same identifier on another domain is another link", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination)
			VALUES ($1, 'other.example.dev', 'k7m2xq9d', 'https://example.com/', 'https://example.com/')`, workspace)
		if err != nil {
			t.Errorf("err = %v, want none", err)
		}
	})
	t.Run("a link belongs to a workspace that exists", func(t *testing.T) {
		code := violation(t, pool, insert, "00000000-0000-7000-8000-000000000000", "bcdfghjk")
		if code != foreignKeyViolation {
			t.Errorf("code = %s, want %s", code, foreignKeyViolation)
		}
	})
	t.Run("a state outside the four is refused", func(t *testing.T) {
		if code := violation(t, pool, `UPDATE links SET state = 'expired' WHERE id = $1`, first); code != checkViolation {
			t.Errorf("code = %s, want %s", code, checkViolation)
		}
	})
	t.Run("a link that is not deleted has a destination", func(t *testing.T) {
		if code := violation(t, pool, `UPDATE links SET destination = NULL WHERE id = $1`, first); code != checkViolation {
			t.Errorf("code = %s, want %s", code, checkViolation)
		}
	})
	t.Run("a deleted link cannot keep what it led to", func(t *testing.T) {
		if code := violation(t, pool, `UPDATE links SET state = 'deleted' WHERE id = $1`, first); code != checkViolation {
			t.Errorf("code = %s, want %s", code, checkViolation)
		}
	})
	t.Run("a tombstone keeps its identifier for ever", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE links SET state = 'deleted', destination = NULL,
			resolved_destination = NULL, version = version + 1 WHERE id = $1`, first)
		if err != nil {
			t.Fatalf("deleting a link: %v", err)
		}
		if code := violation(t, pool, insert, workspace, "k7m2xq9d"); code != uniqueViolation {
			t.Errorf("code = %s, want %s: the identifier of a deleted link was issued again", code, uniqueViolation)
		}
	})
}

func TestTheSchemaKeepsTheRulesOfAWorkspace(t *testing.T) {
	pool := migrated(t)
	cases := []struct {
		name string
		sql  string
	}{
		{"a state outside the two", `INSERT INTO workspaces (name, max_links, state) VALUES ('Acme', 10, 'paused')`},
		{"an empty name", `INSERT INTO workspaces (name, max_links) VALUES ('', 10)`},
		{"a negative quota", `INSERT INTO workspaces (name, max_links) VALUES ('Acme', -1)`},
		{"an allowlist that is not a list", `INSERT INTO workspaces (name, max_links, allowed_destinations) VALUES ('Acme', 10, '{}')`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code := violation(t, pool, c.sql); code != checkViolation {
				t.Errorf("code = %s, want %s", code, checkViolation)
			}
		})
	}
}
