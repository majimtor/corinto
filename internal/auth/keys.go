package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/link"
)

// maxNameLength bounds the name of a key, in characters. A name tells the
// keys of a workspace apart in a list; it is not a description.
const maxNameLength = 100

// Why the keys refuse.
var (
	// ErrInvalidKey covers a credential that is not well formed, one whose
	// key does not exist, a wrong secret, a revoked key and an expired one.
	// From outside they cannot be told apart: a different answer for each
	// would say which keys exist and which were once valid.
	ErrInvalidKey = errors.New("the key is not valid")
	// ErrNotFound covers a key that does not exist, one that is already
	// revoked and one of another workspace.
	ErrNotFound      = errors.New("there is no such key in force")
	ErrName          = errors.New("the name of a key has between 1 and 100 printable characters")
	ErrExpiry        = errors.New("the expiry of a key is later than the moment it is issued")
	ErrQuotaExceeded = errors.New("the workspace has reached its quota of keys")
)

// Key is an API key as it is stored: everything but its secret, of which
// only a hash is kept.
type Key struct {
	ID          string
	WorkspaceID string
	Name        string
	Scopes      []Scope
	ExpiresAt   *time.Time
	CreatedAt   time.Time
}

// Draft is what is given to issue a key.
type Draft struct {
	Name      string
	Scopes    []Scope
	ExpiresAt *time.Time
}

// Keys issues, resolves and revokes the API keys.
type Keys struct {
	pool   *pgxpool.Pool
	random io.Reader
}

// NewKeys returns the keys of a database. random is the source of the
// secrets; everything but a test passes crypto/rand.Reader.
func NewKeys(pool *pgxpool.Pool, random io.Reader) *Keys {
	return &Keys{pool: pool, random: random}
}

// Issue creates a key in a workspace and returns it with its credential,
// the text its holder sends. The credential exists only in that return
// value: what is stored is a hash of its secret, so it cannot be shown
// again. A workspace that does not exist or is blocked gives the errors of
// link.LockWorkspace.
func (k *Keys) Issue(ctx context.Context, workspaceID string, draft Draft) (Key, string, error) {
	// The name of a workspace has the same rule, in the package of the
	// links: two rules that happen to agree, not one to share.
	if !utf8.ValidString(draft.Name) || draft.Name == "" || utf8.RuneCountInString(draft.Name) > maxNameLength ||
		strings.ContainsFunc(draft.Name, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return Key{}, "", ErrName
	}
	scopes, err := canonical(draft.Scopes)
	if err != nil {
		return Key{}, "", err
	}

	tx, err := k.pool.Begin(ctx)
	if err != nil {
		return Key{}, "", fmt.Errorf("begin the issue of a key: %w", err)
	}
	// After a commit this does nothing; before one it undoes everything.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The row of the workspace is locked until the commit, so the count
	// below cannot be passed by two issues at once.
	workspace, err := link.LockWorkspace(ctx, tx, workspaceID)
	if err != nil {
		return Key{}, "", err
	}

	// A revoked key takes no room in the quota. An expired one does, until
	// it is revoked: the keys that count are the keys the workspace has.
	// The clock is the one of the database, which also decides when a key
	// has expired.
	var (
		held int
		now  time.Time
	)
	if err := tx.QueryRow(ctx,
		`SELECT count(*), now() FROM api_keys WHERE workspace_id = $1 AND revoked_at IS NULL`, workspaceID,
	).Scan(&held, &now); err != nil {
		return Key{}, "", fmt.Errorf("count the keys of the workspace: %w", err)
	}
	if draft.ExpiresAt != nil && !draft.ExpiresAt.After(now) {
		return Key{}, "", ErrExpiry
	}
	if held >= workspace.MaxKeys {
		return Key{}, "", ErrQuotaExceeded
	}

	secret, err := newSecret(k.random)
	if err != nil {
		return Key{}, "", err
	}
	hash := sha256.Sum256([]byte(secret))
	stored := make([]string, len(scopes))
	for i, scope := range scopes {
		stored[i] = string(scope)
	}
	var id pgtype.UUID
	key := Key{WorkspaceID: workspaceID, Name: draft.Name, Scopes: scopes, ExpiresAt: draft.ExpiresAt}
	if err := tx.QueryRow(ctx,
		`INSERT INTO api_keys (workspace_id, name, secret_hash, scopes, expires_at)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, workspace_id, expires_at, created_at`,
		workspaceID, draft.Name, hash[:], stored, draft.ExpiresAt,
	).Scan(&id, &key.WorkspaceID, &key.ExpiresAt, &key.CreatedAt); err != nil {
		return Key{}, "", fmt.Errorf("insert the key: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Key{}, "", fmt.Errorf("commit the issue of a key: %w", err)
	}
	key.ID = id.String()
	return key, credential(id, secret), nil
}

// Authenticate resolves a credential, as it was presented, to the principal
// it stands for. Every refusal is ErrInvalidKey.
func (k *Keys) Authenticate(ctx context.Context, presented string) (Principal, error) {
	id, secret, wellFormed := parseCredential(presented)
	if !wellFormed {
		return Principal{}, ErrInvalidKey
	}

	// When there is no row nothing here is filled in: the key is not usable,
	// and the comparison below still runs, against a hash that no secret
	// has, so that a key that does not exist takes the same work as a wrong
	// secret.
	var (
		workspace     pgtype.UUID
		stored        = make([]byte, sha256.Size)
		scopes        []string
		usable, stale bool
	)
	err := k.pool.QueryRow(ctx,
		`SELECT workspace_id, secret_hash, scopes,
		        revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()),
		        last_used_on IS DISTINCT FROM (now() AT TIME ZONE 'UTC')::date
		   FROM api_keys WHERE id = $1`, id,
	).Scan(&workspace, &stored, &scopes, &usable, &stale)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, fmt.Errorf("read the key: %w", err)
	}
	hash := sha256.Sum256([]byte(secret))
	matches := subtle.ConstantTimeCompare(hash[:], stored) == 1
	if !matches || !usable {
		return Principal{}, ErrInvalidKey
	}

	// The day of the last use is written when the day changes and not on
	// every request, so that authenticating is a read in all the requests
	// of a day but the first.
	if stale {
		if _, err := k.pool.Exec(ctx,
			`UPDATE api_keys SET last_used_on = (now() AT TIME ZONE 'UTC')::date WHERE id = $1`, id,
		); err != nil {
			return Principal{}, fmt.Errorf("record the use of the key: %w", err)
		}
	}

	principal := Principal{WorkspaceID: workspace.String(), KeyID: id.String(), Scopes: make([]Scope, len(scopes))}
	for i, scope := range scopes {
		principal.Scopes[i] = Scope(scope)
	}
	return principal, nil
}

// Revoke ends a key of a workspace: from the next request on it no longer
// authenticates. The row stays, with its name and its last use.
func (k *Keys) Revoke(ctx context.Context, workspaceID, id string) error {
	workspace, ok := parseID(workspaceID)
	key, ok2 := parseID(id)
	if !ok || !ok2 {
		return ErrNotFound
	}
	tag, err := k.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now()
		  WHERE workspace_id = $1 AND id = $2 AND revoked_at IS NULL`, workspace, key)
	if err != nil {
		return fmt.Errorf("revoke the key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeAnywhere ends a key whatever its workspace and returns it, so that
// whoever revoked it sees what it was. It is the operator's revocation: a
// principal of a workspace goes through Revoke.
func (k *Keys) RevokeAnywhere(ctx context.Context, id string) (Key, error) {
	key, ok := parseID(id)
	if !ok {
		return Key{}, ErrNotFound
	}
	var (
		revoked Key
		scopes  []string
	)
	err := k.pool.QueryRow(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL
		 RETURNING id, workspace_id, name, scopes, expires_at, created_at`, key,
	).Scan(&revoked.ID, &revoked.WorkspaceID, &revoked.Name, &scopes, &revoked.ExpiresAt, &revoked.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, fmt.Errorf("revoke the key: %w", err)
	}
	revoked.Scopes = make([]Scope, len(scopes))
	for i, scope := range scopes {
		revoked.Scopes[i] = Scope(scope)
	}
	return revoked, nil
}

// parseID reads an identifier of the database. One that is not well formed
// names nothing, which is how the caller treats it. The package of the
// links has the same few lines: sharing them would tie the two packages for
// less than it saves.
func parseID(id string) (pgtype.UUID, bool) {
	var parsed pgtype.UUID
	if err := parsed.Scan(id); err != nil || !parsed.Valid {
		return pgtype.UUID{}, false
	}
	return parsed, true
}
