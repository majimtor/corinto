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
	"testing"
	"testing/iotest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/link"
)

// The form of a token, written here a second time on purpose: the tests
// must not take it from the code they check.
var tokenForm = regexp.MustCompile(`^[a-z2-7]{52}$`)

// demo inserts a demo workspace and returns its identifier.
func demo(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := workspace(t, pool, 10)
	if _, err := pool.Exec(t.Context(), `UPDATE workspaces SET demo = true WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// enter opens a session that must be opened.
func enter(t *testing.T, sessions *auth.Sessions, workspaceID string) (auth.Session, string) {
	t.Helper()
	session, token, err := sessions.Open(t.Context(), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return session, token
}

// turnedAway fails unless a token is refused with the one error every
// refusal gives, and nothing added to it.
func turnedAway(t *testing.T, sessions *auth.Sessions, token string) {
	t.Helper()
	principal, err := sessions.Authenticate(t.Context(), token)
	if !errors.Is(err, auth.ErrInvalidSession) || err.Error() != auth.ErrInvalidSession.Error() {
		t.Errorf("err = %v, want exactly %v", err, auth.ErrInvalidSession)
	}
	if principal.WorkspaceID != "" || principal.SessionID != "" || principal.Scopes != nil || principal.ExpiresAt != nil {
		t.Errorf("a refusal returned the principal %+v", principal)
	}
}

// moved makes a session one that was opened and last used so long ago, by
// the clock of the database, which is the one that decides when a session
// has ended.
func moved(t *testing.T, pool *pgxpool.Pool, id string, opened, seen time.Duration) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE sessions SET created_at = now() - make_interval(secs => $2),
		                     expires_at = now() - make_interval(secs => $2) + make_interval(secs => $4),
		                     last_seen_at = now() - make_interval(secs => $3)
		  WHERE id = $1::uuid`, id, opened.Seconds(), seen.Seconds(), auth.SessionLifetime.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// sinceSeen returns how long ago the last request of a session was
// written.
func sinceSeen(t *testing.T, pool *pgxpool.Pool, id string) time.Duration {
	t.Helper()
	var seconds float64
	if err := pool.QueryRow(t.Context(),
		`SELECT extract(epoch FROM now() - last_seen_at)::float8 FROM sessions WHERE id = $1::uuid`, id).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	return time.Duration(seconds * float64(time.Second))
}

// held returns how many sessions the table holds.
func held(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// PLAN.md section 9: a session of twelve hours with an idle time of thirty
// minutes, which resolves to its workspace.
func TestAnOpenSessionResolvesToItsWorkspaceWithEveryScope(t *testing.T) {
	if auth.SessionLifetime != 12*time.Hour || auth.SessionIdleFor != 30*time.Minute {
		t.Errorf("a session lasts %v and %v without a request, want 12 hours and 30 minutes", auth.SessionLifetime, auth.SessionIdleFor)
	}
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	entered := demo(t, pool)

	session, token := enter(t, sessions, entered)
	if session.ID == "" || session.WorkspaceID != entered || time.Since(session.CreatedAt).Abs() > time.Minute ||
		session.ExpiresAt.Sub(session.CreatedAt) != auth.SessionLifetime {
		t.Errorf("the session opened is %+v", session)
	}
	if !tokenForm.MatchString(token) {
		t.Errorf("the token does not have the stated form (%d characters)", len(token))
	}

	principal, err := sessions.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.WorkspaceID != entered || principal.SessionID != session.ID || principal.KeyID != "" ||
		!slices.Equal(principal.Scopes, auth.Scopes()) {
		t.Errorf("the principal is %+v, want the workspace %s, the session %s, no key and every scope", principal, entered, session.ID)
	}
	if principal.ExpiresAt == nil || !principal.ExpiresAt.Equal(session.ExpiresAt) {
		t.Errorf("the principal says the session ends at %v, want %v", principal.ExpiresAt, session.ExpiresAt)
	}
}

// A new token on each entry, and the table holds a hash of it and nothing
// of whoever entered.
func TestEveryEntryHasATokenOfItsOwnAndOnlyItsHashIsStored(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	entered := demo(t, pool)

	first, token := enter(t, sessions, entered)
	second, other := enter(t, sessions, entered)
	if token == other || first.ID == second.ID {
		t.Fatal("two entries gave the same session")
	}
	for _, valid := range []string{token, other} {
		if _, err := sessions.Authenticate(t.Context(), valid); err != nil {
			t.Errorf("one of two sessions of a workspace gives %v", err)
		}
	}

	var (
		hash []byte
		row  string
	)
	if err := pool.QueryRow(t.Context(),
		`SELECT token_hash, sessions::text FROM sessions WHERE id = $1::uuid`, first.ID).Scan(&hash, &row); err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256([]byte(token)); !bytes.Equal(hash, want[:]) {
		t.Error("what is stored is not the SHA-256 of the token")
	}
	if strings.Contains(row, token) {
		t.Error("the row of the session holds its token")
	}

	// What a session is: a column more is something more kept about
	// whoever is in the panel, and has to be meant.
	var columns []string
	if err := pool.QueryRow(t.Context(),
		`SELECT array_agg(column_name::text ORDER BY ordinal_position) FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'sessions'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if want := []string{"id", "workspace_id", "token_hash", "created_at", "expires_at", "last_seen_at"}; !slices.Equal(columns, want) {
		t.Errorf("a session holds %v, want %v", columns, want)
	}
}

func TestOnlyADemoWorkspaceIsEnteredWithoutASecret(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	ordinary, blocked, entered := workspace(t, pool, 10), demo(t, pool), demo(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE workspaces SET state = 'blocked' WHERE id = $1::uuid`, blocked); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name      string
		workspace string
		opens     *auth.Sessions
		want      error
	}{
		{"a workspace that is not a demo", ordinary, sessions, auth.ErrNotDemo},
		{"a demo workspace that is blocked", blocked, sessions, link.ErrWorkspaceBlocked},
		{"a workspace that does not exist", "00000000-0000-7000-8000-000000000000", sessions, link.ErrWorkspaceNotFound},
		{"something that is not an identifier", "acme", sessions, link.ErrWorkspaceNotFound},
		{"a random source that fails", entered, auth.NewSessions(pool, iotest.ErrReader(errors.New("no entropy"))), auth.ErrRandomSource},
	} {
		t.Run(c.name, func(t *testing.T) {
			session, token, err := c.opens.Open(t.Context(), c.workspace)
			if !errors.Is(err, c.want) || session != (auth.Session{}) || token != "" {
				t.Errorf("err = %v with the session %+v, want %v and no session", err, session, c.want)
			}
		})
	}
	if rows := held(t, pool); rows != 0 {
		t.Errorf("the refused entries stored %d sessions", rows)
	}
	if _, _, err := sessions.Open(t.Context(), entered); err != nil {
		t.Errorf("after the refusals a demo workspace gives %v", err)
	}
}

// A different answer for each case would tell which sessions exist and
// which once did.
func TestEveryRefusalOfASessionIsTheSame(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	entered := demo(t, pool)
	_, valid := enter(t, sessions, entered)

	other := "a"
	if valid[51:] == "a" {
		other = "b"
	}
	for name, token := range map[string]string{
		"nothing":                         "",
		"a token no session has":          valid[:51] + other,
		"a token in upper case":           strings.ToUpper(valid),
		"a token one character short":     valid[1:],
		"a token one character long":      valid + "a",
		"a token outside its alphabet":    valid[:51] + "1",
		"a token with a space around it":  " " + valid[:50] + " ",
		"the hash of a token, and not it": strings.Repeat("0", 64),
	} {
		t.Run(name, func(t *testing.T) { turnedAway(t, sessions, token) })
	}
	t.Run("a session past its lifetime", func(t *testing.T) {
		session, token := enter(t, sessions, entered)
		moved(t, pool, session.ID, auth.SessionLifetime+time.Second, 0)
		turnedAway(t, sessions, token)
	})
	t.Run("a session left idle", func(t *testing.T) {
		session, token := enter(t, sessions, entered)
		moved(t, pool, session.ID, time.Hour, auth.SessionIdleFor+time.Second)
		turnedAway(t, sessions, token)
	})
	t.Run("a session that was closed", func(t *testing.T) {
		_, token := enter(t, sessions, entered)
		if err := sessions.Close(t.Context(), token); err != nil {
			t.Fatal(err)
		}
		turnedAway(t, sessions, token)
	})

	if _, err := sessions.Authenticate(t.Context(), valid); err != nil {
		t.Errorf("after the refusals the valid session gives %v", err)
	}
}

func TestASessionLastsItsLifetimeHoweverMuchItIsUsed(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	session, token := enter(t, sessions, demo(t, pool))

	// A minute short of its lifetime, with a request a moment ago.
	moved(t, pool, session.ID, auth.SessionLifetime-time.Minute, 0)
	if _, err := sessions.Authenticate(t.Context(), token); err != nil {
		t.Errorf("a minute before its lifetime is up the session gives %v", err)
	}
	// Past it, with a request a moment ago all the same.
	moved(t, pool, session.ID, auth.SessionLifetime+time.Second, 0)
	turnedAway(t, sessions, token)
}

func TestASessionEndsWhenItIsLeftIdle(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	session, token := enter(t, sessions, demo(t, pool))

	// A minute short of its idle time: the request is served, and it is
	// the last request from then on.
	moved(t, pool, session.ID, 2*time.Hour, auth.SessionIdleFor-time.Minute)
	if _, err := sessions.Authenticate(t.Context(), token); err != nil {
		t.Fatalf("a minute before its idle time is up the session gives %v", err)
	}
	if idle := sinceSeen(t, pool, session.ID); idle > time.Minute {
		t.Errorf("after a request the session was last seen %v ago", idle)
	}

	// Past it, the request is refused, and a refused request is not a use
	// of the session.
	moved(t, pool, session.ID, 2*time.Hour, auth.SessionIdleFor+time.Second)
	turnedAway(t, sessions, token)
	turnedAway(t, sessions, token)
	if idle := sinceSeen(t, pool, session.ID); idle < auth.SessionIdleFor {
		t.Errorf("a refused request was written as the last one, %v ago", idle)
	}
}

// A page reads the session and does not write it: the last request is
// written once it is a minute old, and not before.
func TestTheLastRequestIsWrittenAtMostOnceAMinute(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	session, token := enter(t, sessions, demo(t, pool))

	moved(t, pool, session.ID, time.Hour, 30*time.Second)
	for range 3 {
		if _, err := sessions.Authenticate(t.Context(), token); err != nil {
			t.Fatal(err)
		}
	}
	if idle := sinceSeen(t, pool, session.ID); idle < 30*time.Second {
		t.Errorf("a request half a minute after the last one was written: last seen %v ago", idle)
	}

	moved(t, pool, session.ID, time.Hour, 90*time.Second)
	if _, err := sessions.Authenticate(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if idle := sinceSeen(t, pool, session.ID); idle > 30*time.Second {
		t.Errorf("a request a minute and a half after the last one was not written: last seen %v ago", idle)
	}
}

func TestClosingEndsASessionAtOnceAndLeavesTheOthers(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	entered := demo(t, pool)
	_, leaving := enter(t, sessions, entered)
	_, staying := enter(t, sessions, entered)

	// Nothing that is not the token of a session closes one.
	for _, token := range []string{"", "acme", strings.ToUpper(leaving), leaving[1:] + "a"} {
		if err := sessions.Close(t.Context(), token); err != nil {
			t.Errorf("closing %d characters that are not a session: %v", len(token), err)
		}
	}
	if rows := held(t, pool); rows != 2 {
		t.Fatalf("%d sessions are left, want the two that were opened", rows)
	}

	if err := sessions.Close(t.Context(), leaving); err != nil {
		t.Fatal(err)
	}
	turnedAway(t, sessions, leaving)
	if _, err := sessions.Authenticate(t.Context(), staying); err != nil {
		t.Errorf("the other session of the workspace was closed with it: %v", err)
	}
	// Its row is gone, and leaving twice is not an error.
	if rows := held(t, pool); rows != 1 {
		t.Errorf("%d sessions are left, want the one that stays", rows)
	}
	if err := sessions.Close(t.Context(), leaving); err != nil {
		t.Errorf("closing again: %v", err)
	}
}

// Blocking a workspace ends its sessions in the transaction that blocks
// it: they end together or not at all.
func TestCloseSessionsEndsTheSessionsOfOneWorkspaceInTheTransactionOfTheCaller(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	entered, neighbour := demo(t, pool), demo(t, pool)
	_, first := enter(t, sessions, entered)
	_, second := enter(t, sessions, entered)
	_, foreign := enter(t, sessions, neighbour)

	closeAll := func(workspaceID string, commit bool) int {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(t.Context()) }()
		ended, err := auth.CloseSessions(t.Context(), tx, workspaceID)
		if err != nil {
			t.Fatal(err)
		}
		if commit {
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		return ended
	}

	if ended := closeAll(entered, false); ended != 2 {
		t.Errorf("%d sessions ended, want the 2 of the workspace", ended)
	}
	for _, token := range []string{first, second} {
		if _, err := sessions.Authenticate(t.Context(), token); err != nil {
			t.Errorf("a closing that was undone ended a session: %v", err)
		}
	}
	if ended := closeAll("acme", true); ended != 0 {
		t.Errorf("something that is not an identifier ended %d sessions", ended)
	}

	if ended := closeAll(entered, true); ended != 2 {
		t.Errorf("%d sessions ended, want 2", ended)
	}
	turnedAway(t, sessions, first)
	turnedAway(t, sessions, second)
	if _, err := sessions.Authenticate(t.Context(), foreign); err != nil {
		t.Errorf("the session of another workspace was ended: %v", err)
	}
	if ended := closeAll(entered, true); ended != 0 {
		t.Errorf("closing again ended %d sessions, want none", ended)
	}
}

// What the retention run takes away: the rows of the sessions that ended
// by themselves, in batches.
func TestOnlyTheSessionsThatEndedAreDeletedAndNoMoreThanAsked(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	entered := demo(t, pool)
	_, live := enter(t, sessions, entered)
	resting, restingToken := enter(t, sessions, entered)
	moved(t, pool, resting.ID, auth.SessionLifetime-time.Minute, auth.SessionIdleFor-time.Minute)
	for _, ended := range []struct{ opened, seen time.Duration }{
		{auth.SessionLifetime + time.Second, 0},
		{auth.SessionLifetime + time.Hour, auth.SessionIdleFor + time.Hour},
		{time.Hour, auth.SessionIdleFor + time.Second},
	} {
		session, _ := enter(t, sessions, entered)
		moved(t, pool, session.ID, ended.opened, ended.seen)
	}

	for _, want := range []int{2, 1, 0} {
		deleted, err := auth.DeleteEndedSessions(t.Context(), pool, 2)
		if err != nil {
			t.Fatal(err)
		}
		if deleted != want {
			t.Errorf("a batch of two deleted %d sessions, want %d", deleted, want)
		}
	}
	if rows := held(t, pool); rows != 2 {
		t.Errorf("%d sessions are left, want the two that have not ended", rows)
	}
	for _, token := range []string{live, restingToken} {
		if _, err := sessions.Authenticate(t.Context(), token); err != nil {
			t.Errorf("a session that had not ended was deleted: %v", err)
		}
	}
}

// A blocked workspace has no session. An entry that arrives while the
// workspace is being blocked waits for the block, and is then refused:
// without the lock on the workspace it would read the workspace as it was
// and leave a session the block had already looked for.
func TestNobodyEntersAWorkspaceWhileItIsBeingBlocked(t *testing.T) {
	pool := database(t)
	sessions := auth.NewSessions(pool, rand.Reader)
	entered := demo(t, pool)
	_, inside := enter(t, sessions, entered)

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, _, err := link.BlockWorkspace(t.Context(), tx, entered); err != nil {
		t.Fatal(err)
	}
	if ended, err := auth.CloseSessions(t.Context(), tx, entered); err != nil || ended != 1 {
		t.Fatalf("the block closed %d sessions (%v), want the one inside", ended, err)
	}

	arrived := make(chan error, 1)
	go func() {
		_, _, err := sessions.Open(t.Context(), entered)
		arrived <- err
	}()
	select {
	case err := <-arrived:
		t.Fatalf("an entry did not wait for the block that was on its way: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-arrived; !errors.Is(err, link.ErrWorkspaceBlocked) {
		t.Errorf("the entry that waited: err = %v, want %v", err, link.ErrWorkspaceBlocked)
	}
	turnedAway(t, sessions, inside)
	if rows := held(t, pool); rows != 0 {
		t.Errorf("the blocked workspace is left with %d sessions", rows)
	}
}
