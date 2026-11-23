package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/link"
)

// How long a session lasts. PLAN.md section 9 states the two periods.
const (
	// SessionLifetime is how long a session lasts however much it is used:
	// past it, whoever held it enters again.
	SessionLifetime = 12 * time.Hour
	// SessionIdleFor is how long a session lasts without a request.
	SessionIdleFor = 30 * time.Minute
	// sessionSeenEvery is how often, at the most, the last request of a
	// session is written, so that a page reads the session and does not
	// write it. The idle time is counted from what is written: a session
	// may end up to this long before its idle time is up, and never after.
	sessionSeenEvery = time.Minute
)

// Why the sessions refuse.
var (
	// ErrInvalidSession covers a token that is not well formed, one that no
	// session has, a session past its lifetime, one left idle and one that
	// was closed. From outside they cannot be told apart.
	ErrInvalidSession = errors.New("the session is not valid")
	// ErrNotDemo means that a session was asked for in a workspace that is
	// not a demo. A session opens without a secret, and only a demo
	// workspace is entered that way (ADR 0012).
	ErrNotDemo = errors.New("only a demo workspace is entered without a secret")
)

// Session is a session of the panel as it is stored: everything but its
// token, of which only a hash is kept.
type Session struct {
	ID          string
	WorkspaceID string
	CreatedAt   time.Time
	// ExpiresAt is when the session ends however much it is used.
	ExpiresAt time.Time
}

// Sessions opens, resolves and closes the sessions of the panel.
type Sessions struct {
	pool   *pgxpool.Pool
	random io.Reader
}

// NewSessions returns the sessions of a database. random is the source of
// the tokens; everything but a test passes crypto/rand.Reader.
func NewSessions(pool *pgxpool.Pool, random io.Reader) *Sessions {
	return &Sessions{pool: pool, random: random}
}

// Open opens a session in a demo workspace and returns it with its token,
// the text a browser sends back. The token exists only in that return
// value: what is stored is a hash of it. Every call gives a token of its
// own. A workspace that does not exist or is blocked gives the errors of
// link.LockWorkspace.
func (s *Sessions) Open(ctx context.Context, workspaceID string) (Session, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, "", fmt.Errorf("begin the opening of a session: %w", err)
	}
	// After a commit this does nothing; before one it undoes everything.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The row of the workspace is locked until the commit: a workspace
	// cannot be blocked, which closes its sessions, while one is opened
	// in it.
	workspace, err := link.LockWorkspace(ctx, tx, workspaceID)
	if err != nil {
		return Session{}, "", err
	}
	if !workspace.Demo {
		return Session{}, "", ErrNotDemo
	}

	// A token is made as the secret of a key is, and is as long: 256 bits,
	// which is why a plain hash of it is enough to store.
	token, err := newSecret(s.random)
	if err != nil {
		return Session{}, "", err
	}
	hash := sha256.Sum256([]byte(token))
	var opened Session
	// The clock is the one of the database, which also decides when a
	// session has ended.
	if err := tx.QueryRow(ctx,
		`INSERT INTO sessions (workspace_id, token_hash, expires_at)
		 VALUES ($1, $2, now() + make_interval(secs => $3))
		 RETURNING id, workspace_id, created_at, expires_at`,
		workspace.ID, hash[:], SessionLifetime.Seconds(),
	).Scan(&opened.ID, &opened.WorkspaceID, &opened.CreatedAt, &opened.ExpiresAt); err != nil {
		return Session{}, "", fmt.Errorf("insert the session: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, "", fmt.Errorf("commit the opening of a session: %w", err)
	}
	return opened, token, nil
}

// Authenticate resolves a token, as it was presented, to the principal it
// stands for: the workspace of its session, with every scope, since a
// session is a person in the panel and not a system with a task. Every
// refusal is ErrInvalidSession.
func (s *Sessions) Authenticate(ctx context.Context, presented string) (Principal, error) {
	if !wellFormedToken(presented) {
		return Principal{}, ErrInvalidSession
	}
	// A session is found by the hash of its token, in an index. How long
	// that search takes says nothing that helps to guess a token: what is
	// compared is the hash of the guess, which whoever guesses does not
	// choose.
	hash := sha256.Sum256([]byte(presented))
	var (
		id, workspace string
		expiry        time.Time
		stale         bool
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id, expires_at, last_seen_at <= now() - make_interval(secs => $3)
		   FROM sessions
		  WHERE token_hash = $1 AND expires_at > now() AND last_seen_at > now() - make_interval(secs => $2)`,
		hash[:], SessionIdleFor.Seconds(), sessionSeenEvery.Seconds(),
	).Scan(&id, &workspace, &expiry, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidSession
	}
	if err != nil {
		return Principal{}, fmt.Errorf("read the session: %w", err)
	}
	if stale {
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE id = $1::uuid`, id); err != nil {
			return Principal{}, fmt.Errorf("record the use of the session: %w", err)
		}
	}
	return Principal{WorkspaceID: workspace, SessionID: id, Scopes: Scopes(), ExpiresAt: &expiry}, nil
}

// Close ends the session of a token: from the next request on it no longer
// authenticates. Its row is deleted, since nothing in it is worth keeping.
// A token that no session has is not an error: whoever leaves twice has
// left.
func (s *Sessions) Close(ctx context.Context, presented string) error {
	if !wellFormedToken(presented) {
		return nil
	}
	hash := sha256.Sum256([]byte(presented))
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash[:]); err != nil {
		return fmt.Errorf("close the session: %w", err)
	}
	return nil
}

// DeleteEndedSessions deletes sessions that ended by themselves, past their
// lifetime or left idle, a number of them at the most, and returns how
// many it deleted. They no longer authenticate: this only takes their rows
// away, and the retention run calls it.
func DeleteEndedSessions(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM sessions WHERE id IN (
		     SELECT id FROM sessions
		      WHERE expires_at <= now() OR last_seen_at <= now() - make_interval(secs => $1)
		      LIMIT $2)`, SessionIdleFor.Seconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("delete the sessions that ended: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// CloseSessions ends every session of a workspace, in the transaction of
// the caller, and returns how many it ended. Blocking a workspace calls it,
// so that the block and the end of its sessions are committed together or
// not at all, and so does the seed before it deletes a demo workspace.
func CloseSessions(ctx context.Context, tx pgx.Tx, workspaceID string) (int, error) {
	workspace, ok := parseID(workspaceID)
	if !ok {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `DELETE FROM sessions WHERE workspace_id = $1`, workspace)
	if err != nil {
		return 0, fmt.Errorf("close the sessions of the workspace: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// wellFormedToken reports whether a text has the form of a token. Only the
// exact form passes: anything else is refused before the database is
// asked.
func wellFormedToken(presented string) bool {
	return len(presented) == secretLength && strings.Trim(presented, secretAlphabet) == ""
}
