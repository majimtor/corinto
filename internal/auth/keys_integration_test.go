//go:build integration

package auth_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

func TestMain(m *testing.M) { postgrestest.Main(m) }

// database returns a pool on a migrated database of its own.
func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), postgrestest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// workspace inserts a workspace with room for a number of keys and returns
// its identifier.
func workspace(t *testing.T, pool *pgxpool.Pool, maxKeys int) string {
	t.Helper()
	var id string
	err := pool.QueryRow(t.Context(),
		`INSERT INTO workspaces (name, max_links, max_keys) VALUES ('Acme', 1000, $1) RETURNING id`, maxKeys).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// issue creates a key that must be created.
func issue(t *testing.T, keys *auth.Keys, workspaceID string, scopes ...auth.Scope) (auth.Key, string) {
	t.Helper()
	key, credential, err := keys.Issue(t.Context(), workspaceID, auth.Draft{Name: "Deploy pipeline", Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return key, credential
}

// parts splits a credential into the identifier of its key and its secret.
func parts(t *testing.T, credential string) (string, string) {
	t.Helper()
	fields := strings.Split(credential, "_")
	if len(fields) != 3 || fields[0] != "cor" {
		t.Fatalf("a credential has %d parts, want the prefix, the identifier and the secret", len(fields))
	}
	return fields[1], fields[2]
}

// refused fails unless a credential is refused with the one error every
// refusal gives, and nothing added to it.
func refused(t *testing.T, keys *auth.Keys, credential string) {
	t.Helper()
	principal, err := keys.Authenticate(t.Context(), credential)
	if !errors.Is(err, auth.ErrInvalidKey) || err.Error() != auth.ErrInvalidKey.Error() {
		t.Errorf("err = %v, want exactly %v", err, auth.ErrInvalidKey)
	}
	if principal.WorkspaceID != "" || principal.KeyID != "" || principal.Scopes != nil {
		t.Errorf("a refusal returned the principal %+v", principal)
	}
}

func TestAnIssuedKeyResolvesToItsWorkspaceAndItsScopes(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	owner := workspace(t, pool, 10)

	key, credential := issue(t, keys, owner, auth.ScopeMetricsRead, auth.ScopeLinksRead, auth.ScopeMetricsRead)
	want := []auth.Scope{auth.ScopeLinksRead, auth.ScopeMetricsRead}
	if key.WorkspaceID != owner || key.Name != "Deploy pipeline" || !slices.Equal(key.Scopes, want) ||
		key.ExpiresAt != nil || key.CreatedAt.IsZero() {
		t.Errorf("the key issued is %+v", key)
	}
	if !regexp.MustCompile(`^cor_[0-9a-f]{32}_[a-z2-7]{52}$`).MatchString(credential) {
		t.Errorf("the credential does not have the stated form (%d characters)", len(credential))
	}
	if identifier, _ := parts(t, credential); identifier != strings.ReplaceAll(key.ID, "-", "") {
		t.Errorf("the credential names the key %s, want %s", identifier, key.ID)
	}

	principal, err := keys.Authenticate(t.Context(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if principal.WorkspaceID != owner || principal.KeyID != key.ID || !slices.Equal(principal.Scopes, want) {
		t.Errorf("the principal is %+v, want the workspace %s, the key %s and %v", principal, owner, key.ID, want)
	}
	if !principal.Allows(auth.ScopeLinksRead) || principal.Allows(auth.ScopeLinksWrite) {
		t.Error("the principal does not allow exactly what the key was issued with")
	}
}

func TestOnlyAHashOfTheSecretIsStored(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	key, credential := issue(t, keys, workspace(t, pool, 10), auth.ScopeLinksRead)
	_, secret := parts(t, credential)

	var (
		hash []byte
		row  string
	)
	if err := pool.QueryRow(t.Context(),
		`SELECT secret_hash, api_keys::text FROM api_keys WHERE id = $1`, key.ID).Scan(&hash, &row); err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256([]byte(secret)); !bytes.Equal(hash, want[:]) {
		t.Error("what is stored is not the SHA-256 of the secret")
	}
	if strings.Contains(row, secret) {
		t.Error("the row of the key holds its secret")
	}
}

// A different answer for each case would tell which keys exist and which
// were once valid.
func TestEveryRefusalIsTheSame(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	owner := workspace(t, pool, 10)
	_, valid := issue(t, keys, owner, auth.ScopeLinksRead)
	identifier, secret := parts(t, valid)

	t.Run("a key that does not exist", func(t *testing.T) {
		other := "0"
		if identifier[31:] == "0" {
			other = "1"
		}
		refused(t, keys, "cor_"+identifier[:31]+other+"_"+secret)
	})
	t.Run("the secret of another key", func(t *testing.T) {
		_, another := issue(t, keys, owner, auth.ScopeLinksRead)
		_, otherSecret := parts(t, another)
		refused(t, keys, "cor_"+identifier+"_"+otherSecret)
	})
	t.Run("a revoked key", func(t *testing.T) {
		key, credential := issue(t, keys, owner, auth.ScopeLinksRead)
		if err := keys.Revoke(t.Context(), owner, key.ID); err != nil {
			t.Fatal(err)
		}
		refused(t, keys, credential)
	})
	t.Run("an expired key", func(t *testing.T) {
		key, credential := issue(t, keys, owner, auth.ScopeLinksRead)
		if _, err := pool.Exec(t.Context(),
			`UPDATE api_keys SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second'
			  WHERE id = $1`, key.ID); err != nil {
			t.Fatal(err)
		}
		refused(t, keys, credential)
	})
	t.Run("something that is not a credential", func(t *testing.T) {
		refused(t, keys, strings.ToUpper(valid))
	})

	if _, err := keys.Authenticate(t.Context(), valid); err != nil {
		t.Errorf("after the refusals the valid key gives %v", err)
	}
}

func TestAKeyWithAnExpiryWorksUntilThen(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	owner := workspace(t, pool, 10)

	expiry := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	key, credential, err := keys.Issue(t.Context(), owner,
		auth.Draft{Name: "Launch week", Scopes: []auth.Scope{auth.ScopeLinksWrite}, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	if key.ExpiresAt == nil || !key.ExpiresAt.Equal(expiry) {
		t.Errorf("the key expires at %v, want %v", key.ExpiresAt, expiry)
	}
	if _, err := keys.Authenticate(t.Context(), credential); err != nil {
		t.Errorf("before its expiry the key gives %v", err)
	}
}

func TestRevokingEndsAKeyAtOnceAndKeepsItsRow(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	owner, stranger := workspace(t, pool, 10), workspace(t, pool, 10)
	key, credential := issue(t, keys, owner, auth.ScopeLinksRead)
	if _, err := keys.Authenticate(t.Context(), credential); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		workspace, id string
	}{
		{"another workspace", stranger, key.ID},
		{"a workspace that is not an identifier", "acme", key.ID},
		{"a key that is not an identifier", owner, "deploy-pipeline"},
		{"a key that does not exist", owner, stranger},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := keys.Revoke(t.Context(), c.workspace, c.id); !errors.Is(err, auth.ErrNotFound) {
				t.Errorf("err = %v, want %v", err, auth.ErrNotFound)
			}
			if _, err := keys.Authenticate(t.Context(), credential); err != nil {
				t.Errorf("the key was revoked all the same: %v", err)
			}
		})
	}

	if err := keys.Revoke(t.Context(), owner, key.ID); err != nil {
		t.Fatal(err)
	}
	refused(t, keys, credential)
	if err := keys.Revoke(t.Context(), owner, key.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("revoking again: err = %v, want %v", err, auth.ErrNotFound)
	}

	var (
		name          string
		revoked, used bool
	)
	if err := pool.QueryRow(t.Context(),
		`SELECT name, revoked_at IS NOT NULL, last_used_on IS NOT NULL FROM api_keys WHERE id = $1`, key.ID,
	).Scan(&name, &revoked, &used); err != nil {
		t.Fatalf("the row of a revoked key: %v", err)
	}
	if name != "Deploy pipeline" || !revoked || !used {
		t.Errorf("the row of the revoked key: name %q, revoked %v, last use kept %v", name, revoked, used)
	}
}

// The operator stands above the workspaces: it revokes a key by its
// identifier alone, and is told which key it was.
func TestTheOperatorRevokesAKeyWhateverItsWorkspace(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	owner := workspace(t, pool, 10)
	key, credential := issue(t, keys, owner, auth.ScopeLinksWrite, auth.ScopeLinksRead)
	_, untouched := issue(t, keys, owner, auth.ScopeLinksRead)

	for name, id := range map[string]string{
		"something that is not an identifier": "deploy-pipeline",
		"an identifier that is not a key":     owner,
	} {
		t.Run(name, func(t *testing.T) {
			if revoked, err := keys.RevokeAnywhere(t.Context(), id); !errors.Is(err, auth.ErrNotFound) || revoked.ID != "" {
				t.Errorf("err = %v with the key %q, want %v and none", err, revoked.ID, auth.ErrNotFound)
			}
		})
	}

	revoked, err := keys.RevokeAnywhere(t.Context(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.ID != key.ID || revoked.WorkspaceID != owner || revoked.Name != key.Name ||
		!slices.Equal(revoked.Scopes, key.Scopes) || !revoked.CreatedAt.Equal(key.CreatedAt) {
		t.Errorf("the key revoked is %+v, want %+v", revoked, key)
	}
	refused(t, keys, credential)
	if _, err := keys.Authenticate(t.Context(), untouched); err != nil {
		t.Errorf("another key of the workspace was revoked with it: %v", err)
	}
	if _, err := keys.RevokeAnywhere(t.Context(), key.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("revoking again: err = %v, want %v", err, auth.ErrNotFound)
	}
}

// Twenty issues at once in a workspace with room for three keys: exactly
// three succeed. Without the lock on the workspace, several would count two
// keys and all would insert.
func TestTheQuotaOfKeysHoldsUnderConcurrency(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	owner := workspace(t, pool, 3)

	var (
		wait      sync.WaitGroup
		mu        sync.Mutex
		issued    []string
		overQuota int
	)
	for range 20 {
		wait.Go(func() {
			key, _, err := keys.Issue(t.Context(), owner,
				auth.Draft{Name: "Deploy pipeline", Scopes: []auth.Scope{auth.ScopeLinksRead}})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				issued = append(issued, key.ID)
			case errors.Is(err, auth.ErrQuotaExceeded):
				overQuota++
			default:
				t.Errorf("an issue failed with %v", err)
			}
		})
	}
	wait.Wait()
	if len(issued) != 3 || overQuota != 17 {
		t.Fatalf("%d keys issued and %d refused by the quota, want 3 and 17", len(issued), overQuota)
	}

	// An expired key still belongs to the workspace and takes its room.
	if _, err := pool.Exec(t.Context(),
		`UPDATE api_keys SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second'
		  WHERE id = $1`, issued[0]); err != nil {
		t.Fatal(err)
	}
	draft := auth.Draft{Name: "Deploy pipeline", Scopes: []auth.Scope{auth.ScopeLinksRead}}
	if _, _, err := keys.Issue(t.Context(), owner, draft); !errors.Is(err, auth.ErrQuotaExceeded) {
		t.Errorf("with an expired key among the three: err = %v, want %v", err, auth.ErrQuotaExceeded)
	}
	// A revoked one does not: there is room for one more, and only one.
	if err := keys.Revoke(t.Context(), owner, issued[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := keys.Issue(t.Context(), owner, draft); err != nil {
		t.Errorf("after revoking a key: err = %v, want none", err)
	}
	if _, _, err := keys.Issue(t.Context(), owner, draft); !errors.Is(err, auth.ErrQuotaExceeded) {
		t.Errorf("with the room taken again: err = %v, want %v", err, auth.ErrQuotaExceeded)
	}
}

func TestIssueRefusesWhatCannotBeAKey(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	owner := workspace(t, pool, 10)
	var blocked string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO workspaces (name, max_links, max_keys, state) VALUES ('Acme', 1000, 10, 'blocked') RETURNING id`,
	).Scan(&blocked); err != nil {
		t.Fatal(err)
	}

	read := []auth.Scope{auth.ScopeLinksRead}
	past := time.Now().Add(-time.Hour)
	// A character of three bytes, so that the limit is seen to count
	// characters.
	wide := string(rune(0x4e2d))
	cases := []struct {
		name      string
		workspace string
		draft     auth.Draft
		want      error
	}{
		{"no name", owner, auth.Draft{Scopes: read}, auth.ErrName},
		{"a name of 101 characters", owner, auth.Draft{Name: strings.Repeat(wide, 101), Scopes: read}, auth.ErrName},
		{"a name with a line break", owner, auth.Draft{Name: "Deploy\npipeline", Scopes: read}, auth.ErrName},
		{"a name with a tab", owner, auth.Draft{Name: "Deploy\tpipeline", Scopes: read}, auth.ErrName},
		{"a name that is not text", owner, auth.Draft{Name: string([]byte{'a', 0xff}), Scopes: read}, auth.ErrName},
		{"no scope", owner, auth.Draft{Name: "Deploy pipeline"}, auth.ErrScopes},
		{"a scope that does not exist", owner, auth.Draft{Name: "Deploy pipeline", Scopes: []auth.Scope{"links:delete"}}, auth.ErrScopes},
		{"an expiry that has passed", owner, auth.Draft{Name: "Deploy pipeline", Scopes: read, ExpiresAt: &past}, auth.ErrExpiry},
		{"a blocked workspace", blocked, auth.Draft{Name: "Deploy pipeline", Scopes: read}, link.ErrWorkspaceBlocked},
		{"a workspace that does not exist", "00000000-0000-7000-8000-000000000000", auth.Draft{Name: "Deploy pipeline", Scopes: read}, link.ErrWorkspaceNotFound},
		{"a workspace that is not an identifier", "acme", auth.Draft{Name: "Deploy pipeline", Scopes: read}, link.ErrWorkspaceNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, credential, err := keys.Issue(t.Context(), c.workspace, c.draft)
			if !errors.Is(err, c.want) || key.ID != "" || credential != "" {
				t.Errorf("err = %v with the key %q, want %v and no key", err, key.ID, c.want)
			}
		})
	}
	t.Run("a random source that fails", func(t *testing.T) {
		broken := auth.NewKeys(pool, iotest.ErrReader(errors.New("no entropy")))
		_, credential, err := broken.Issue(t.Context(), owner, auth.Draft{Name: "Deploy pipeline", Scopes: read})
		if !errors.Is(err, auth.ErrRandomSource) || credential != "" {
			t.Errorf("err = %v, want %v and no credential", err, auth.ErrRandomSource)
		}
	})

	var stored int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM api_keys`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("the refused issues stored %d keys", stored)
	}

	t.Run("a name of 100 characters is a name", func(t *testing.T) {
		if _, _, err := keys.Issue(t.Context(), owner, auth.Draft{Name: strings.Repeat(wide, 100), Scopes: read}); err != nil {
			t.Errorf("err = %v, want none", err)
		}
	})
}

// The day of the last use is written when it changes and not on every
// request: authenticating is a read in all the requests of a day but the
// first.
func TestTheDayOfTheLastUseIsWrittenOnceADay(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)
	key, credential := issue(t, keys, workspace(t, pool, 10), auth.ScopeLinksRead)
	identifier, _ := parts(t, credential)

	// state returns whether the key has a last use, whether it is today,
	// and the transaction that last wrote its row.
	state := func() (bool, bool, string) {
		t.Helper()
		var (
			used, today bool
			writer      string
		)
		if err := pool.QueryRow(t.Context(),
			`SELECT last_used_on IS NOT NULL, coalesce(last_used_on = (now() AT TIME ZONE 'UTC')::date, false), xmin::text
			   FROM api_keys WHERE id = $1`, key.ID).Scan(&used, &today, &writer); err != nil {
			t.Fatal(err)
		}
		return used, today, writer
	}

	if used, _, _ := state(); used {
		t.Fatal("a key that was never presented has a last use")
	}
	_, another := issue(t, keys, key.WorkspaceID, auth.ScopeLinksRead)
	_, otherSecret := parts(t, another)
	refused(t, keys, "cor_"+identifier+"_"+otherSecret)
	if used, _, _ := state(); used {
		t.Error("a refused request counted as a use of the key")
	}

	if _, err := keys.Authenticate(t.Context(), credential); err != nil {
		t.Fatal(err)
	}
	_, today, writer := state()
	if !today {
		t.Fatal("after its first request the last use of the key is not today")
	}
	for range 3 {
		if _, err := keys.Authenticate(t.Context(), credential); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, after := state(); after != writer {
		t.Error("a later request of the same day wrote the row of the key again")
	}

	if _, err := pool.Exec(t.Context(),
		`UPDATE api_keys SET last_used_on = last_used_on - 1 WHERE id = $1`, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Authenticate(t.Context(), credential); err != nil {
		t.Fatal(err)
	}
	if _, today, _ := state(); !today {
		t.Error("the first request of a new day did not move the last use to that day")
	}
}

// The scopes are written twice, in the code and in a check of the schema.
// This keeps the two lists equal.
func TestTheSchemaAndTheCodeNameTheSameScopes(t *testing.T) {
	pool := database(t)
	keys := auth.NewKeys(pool, rand.Reader)

	_, credential := issue(t, keys, workspace(t, pool, 10), auth.Scopes()...)
	principal, err := keys.Authenticate(t.Context(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(principal.Scopes, auth.Scopes()) {
		t.Errorf("a key with every scope holds %v, want %v", principal.Scopes, auth.Scopes())
	}

	var check string
	if err := pool.QueryRow(t.Context(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid = 'api_keys'::regclass AND conname = 'api_keys_scopes_check'`).Scan(&check); err != nil {
		t.Fatalf("read the check of the scopes: %v", err)
	}
	var inSchema []auth.Scope
	for _, match := range regexp.MustCompile(`'([a-z]+:[a-z]+)'`).FindAllStringSubmatch(check, -1) {
		inSchema = append(inSchema, auth.Scope(match[1]))
	}
	if !slices.Equal(inSchema, auth.Scopes()) {
		t.Errorf("the schema accepts %v and the code knows %v", inSchema, auth.Scopes())
	}
}
