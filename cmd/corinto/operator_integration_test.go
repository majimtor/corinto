//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

// migrated returns the environment of a database with the whole schema and
// a pool on it, for what a test checks behind the command.
func migrated(t *testing.T) (func(string) string, *pgxpool.Pool) {
	t.Helper()
	databaseURL := postgrestest.NewDatabase(t)
	env := environmentFor(databaseURL)
	var log bytes.Buffer
	if code := run(t.Context(), []string{"migrate"}, env, io.Discard, &log); code != exitOK {
		t.Fatalf("migrate exit code = %d; log: %s", code, log.String())
	}
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return env, pool
}

// operate runs one action of the operator and returns its exit code, what
// it printed and what it logged.
func operate(t *testing.T, env func(string) string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), append([]string{"operator"}, args...), env, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// printed runs an action that must succeed and returns the object it
// printed.
func printed(t *testing.T, env func(string) string, args ...string) map[string]any {
	t.Helper()
	code, stdout, log := operate(t, env, args...)
	if code != exitOK {
		t.Fatalf("operator %s: exit code = %d; log: %s", args[0], code, log)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(stdout), &object); err != nil {
		t.Fatalf("operator %s did not print one JSON object: %v", args[0], err)
	}
	return object
}

// count returns how many rows a table holds.
func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var rows int
	// The name is one of the two written in this file.
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestTheOperatorCreatesAWorkspaceAndIssuesAndRevokesItsKey(t *testing.T) {
	env, pool := migrated(t)
	keys := auth.NewKeys(pool, rand.Reader)

	workspace := printed(t, env, "create-workspace", "--name", "Acme")
	want := map[string]any{
		"id": workspace["id"], "name": "Acme", "demo": false, "may_choose_aliases": false,
		"allowed_destinations": []any{}, "max_links": 1000.0, "max_keys": 10.0,
	}
	for field, value := range want {
		if got := workspace[field]; !equalJSON(got, value) {
			t.Errorf("the workspace printed has %s = %v, want %v", field, got, value)
		}
	}
	workspaceID, _ := workspace["id"].(string)
	if workspaceID == "" || len(workspace) != len(want) {
		t.Fatalf("the workspace printed is %v", workspace)
	}

	code, stdout, log := operate(t, env, "issue-key", "--workspace", workspaceID, "--name", "Deploy pipeline",
		"--scope", "links:write", "--scope", "links:read", "--expires", "2099-01-31T00:00:00Z")
	if code != exitOK {
		t.Fatalf("issue-key: exit code = %d; log: %s", code, log)
	}
	var issued struct {
		ID          string   `json:"id"`
		WorkspaceID string   `json:"workspace_id"`
		Name        string   `json:"name"`
		Scopes      []string `json:"scopes"`
		ExpiresAt   string   `json:"expires_at"`
		Key         string   `json:"key"`
	}
	if err := json.Unmarshal([]byte(stdout), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.WorkspaceID != workspaceID || issued.Name != "Deploy pipeline" ||
		!slices.Equal(issued.Scopes, []string{"links:read", "links:write"}) || issued.ExpiresAt != "2099-01-31T00:00:00Z" {
		t.Errorf("the key printed is %+v", issued)
	}
	// The key is printed once and logged never: the log names the key by
	// its identifier.
	if strings.Count(stdout, issued.Key) != 1 || issued.Key == "" {
		t.Error("the key is not printed exactly once")
	}
	_, secret, _ := strings.Cut(strings.TrimPrefix(issued.Key, "cor_"), "_")
	if secret == "" || strings.Contains(log, secret) {
		t.Error("the log of issue-key carries the secret of the key")
	}
	if !strings.Contains(log, `"action":"issue-key"`) || !strings.Contains(log, issued.ID) {
		t.Errorf("the log does not name the action and the key: %s", log)
	}

	principal, err := keys.Authenticate(t.Context(), issued.Key)
	if err != nil {
		t.Fatalf("the key printed does not authenticate: %v", err)
	}
	if principal.WorkspaceID != workspaceID || principal.KeyID != issued.ID {
		t.Errorf("the key printed resolves to %+v", principal)
	}

	revoked := printed(t, env, "revoke-key", "--key", issued.ID)
	if revoked["id"] != issued.ID || revoked["workspace_id"] != workspaceID || revoked["name"] != "Deploy pipeline" {
		t.Errorf("the revocation printed %v", revoked)
	}
	if _, err := keys.Authenticate(t.Context(), issued.Key); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("after revoke-key the key gives %v, want %v", err, auth.ErrInvalidKey)
	}
	if code, stdout, _ := operate(t, env, "revoke-key", "--key", issued.ID); code != exitFailure || stdout != "" {
		t.Errorf("revoking again: exit code = %d and %q printed, want %d and nothing", code, stdout, exitFailure)
	}
}

// equalJSON compares two values as json.Unmarshal returns them.
func equalJSON(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && bytes.Equal(left, right)
}

func TestTheOperatorGivesAWorkspaceItsQuotasAndItsPolicy(t *testing.T) {
	env, pool := migrated(t)

	demo := printed(t, env, "create-workspace", "--name", "Demo", "--demo")
	if demo["demo"] != true || demo["max_links"] != 100.0 || demo["max_keys"] != 10.0 {
		t.Errorf("a demo workspace without quotas is %v, want 100 links and 10 keys", demo)
	}

	chosen := printed(t, env, "create-workspace", "--name", "Acme", "--aliases", "--max-links", "0", "--max-keys", "3",
		"--allow", "HTTPS://Docs.Example.com:443/guides", "--allow", "https://example.org")
	allowed := []any{
		map[string]any{"scheme": "https", "host": "docs.example.com", "path": "/guides"},
		map[string]any{"scheme": "https", "host": "example.org", "path": "/"},
	}
	if chosen["may_choose_aliases"] != true || chosen["max_links"] != 0.0 || chosen["max_keys"] != 3.0 ||
		!equalJSON(chosen["allowed_destinations"], allowed) {
		t.Errorf("the workspace printed is %v", chosen)
	}

	// What was printed is what was stored.
	var (
		aliases            bool
		maxLinks, maxKeys  int
		entries            int
		state, firstPrefix string
	)
	if err := pool.QueryRow(t.Context(),
		`SELECT may_choose_aliases, max_links, max_keys, jsonb_array_length(allowed_destinations),
		        state, allowed_destinations -> 0 ->> 'host'
		   FROM workspaces WHERE id = $1`, chosen["id"],
	).Scan(&aliases, &maxLinks, &maxKeys, &entries, &state, &firstPrefix); err != nil {
		t.Fatal(err)
	}
	if !aliases || maxLinks != 0 || maxKeys != 3 || entries != 2 || state != "active" || firstPrefix != "docs.example.com" {
		t.Errorf("the row is aliases %v, %d links, %d keys, %d entries, %s, %s",
			aliases, maxLinks, maxKeys, entries, state, firstPrefix)
	}
}

func TestTheOperatorPrintsNothingAndStoresNothingWhenItIsRefused(t *testing.T) {
	env, pool := migrated(t)
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme", "--max-keys", "1")["id"].(string)
	printed(t, env, "issue-key", "--workspace", workspaceID, "--name", "Deploy pipeline", "--scope", "links:read")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"an entry of the allowlist with a query", []string{"create-workspace", "--name", "Acme", "--allow", "https://example.com/?q=1"}, "neither a query nor a fragment"},
		{"an entry of the allowlist that is an address", []string{"create-workspace", "--name", "Acme", "--allow", "http://127.0.0.1/"}, "public domain name"},
		{"a negative quota", []string{"create-workspace", "--name", "Acme", "--max-links", "-1"}, "zero or more"},
		{"a scope that does not exist", []string{"issue-key", "--workspace", workspaceID, "--name", "n", "--scope", "links:delete"}, "known scopes"},
		{"an expiry that has passed", []string{"issue-key", "--workspace", workspaceID, "--name", "n", "--scope", "links:read", "--expires", "2020-01-31T00:00:00Z"}, "expiry"},
		{"a key over the quota", []string{"issue-key", "--workspace", workspaceID, "--name", "n", "--scope", "links:read"}, "quota of keys"},
		{"a workspace that does not exist", []string{"issue-key", "--workspace", "00000000-0000-7000-8000-000000000000", "--name", "n", "--scope", "links:read"}, "no such workspace"},
		{"a key that does not exist", []string{"revoke-key", "--key", workspaceID}, "no such key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, log := operate(t, env, c.args...)
			if code != exitFailure || stdout != "" {
				t.Errorf("exit code = %d and %q printed, want %d and nothing", code, stdout, exitFailure)
			}
			if !strings.Contains(log, c.want) || strings.Contains(log, `"msg":"operator action"`) {
				t.Errorf("the log does not say %q, or records an action: %s", c.want, log)
			}
		})
	}
	if workspaces, keys := count(t, pool, "workspaces"), count(t, pool, "api_keys"); workspaces != 1 || keys != 1 {
		t.Errorf("after the refusals there are %d workspaces and %d keys, want 1 and 1", workspaces, keys)
	}
}

func TestTheOperatorRefusesADatabaseThatWasNotMigrated(t *testing.T) {
	env := environmentFor(postgrestest.NewDatabase(t))
	code, stdout, log := operate(t, env, "create-workspace", "--name", "Acme")
	if code != exitFailure || stdout != "" {
		t.Errorf("exit code = %d and %q printed, want %d and nothing", code, stdout, exitFailure)
	}
	if !strings.Contains(log, "corinto migrate") {
		t.Errorf("the log does not say what to run: %s", log)
	}
}

// newLink stores a link of a workspace, which no action of the operator
// does, and returns it.
func newLink(t *testing.T, pool *pgxpool.Pool, workspaceID string) link.Link {
	t.Helper()
	created, err := link.NewStore(pool, redirectDomain, rand.Reader).Create(t.Context(), workspaceID,
		link.Draft{Destination: "https://example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// stateOf returns the state of a link.
func stateOf(t *testing.T, pool *pgxpool.Pool, linkID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(), `SELECT state FROM links WHERE id = $1`, linkID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestTheOperatorBlocksALinkByItsShortIdentifier(t *testing.T) {
	env, pool := migrated(t)
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	reported, bystander := newLink(t, pool, workspaceID), newLink(t, pool, workspaceID)

	blocked := printed(t, env, "block-link", "--identifier", reported.Identifier)
	want := map[string]any{
		"id": reported.ID, "workspace_id": workspaceID, "domain": redirectDomain,
		"identifier": reported.Identifier, "state": "blocked",
	}
	if !equalJSON(blocked, want) {
		t.Errorf("block-link printed %v, want %v", blocked, want)
	}
	if got := stateOf(t, pool, reported.ID); got != "blocked" {
		t.Errorf("the link reported is %q, want it blocked", got)
	}
	if got := stateOf(t, pool, bystander.ID); got != "active" {
		t.Errorf("another link is %q, want it active", got)
	}

	// The row of a link is not what a report carries, and it names nothing.
	code, stdout, log := operate(t, env, "block-link", "--identifier", bystander.ID)
	if code != exitFailure || stdout != "" || !strings.Contains(log, "no such link") {
		t.Errorf("exit code = %d and %q printed, want %d and nothing; log: %s", code, stdout, exitFailure, log)
	}
}

func TestTheOperatorBlocksAWorkspaceWithItsLinksAndItsKeys(t *testing.T) {
	env, pool := migrated(t)
	keys := auth.NewKeys(pool, rand.Reader)
	issueKey := func(workspaceID string) string {
		t.Helper()
		key, _ := printed(t, env, "issue-key", "--workspace", workspaceID, "--name", "Deploy pipeline",
			"--scope", "links:read")["key"].(string)
		return key
	}

	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	otherID, _ := printed(t, env, "create-workspace", "--name", "Globex")["id"].(string)
	links := []link.Link{newLink(t, pool, workspaceID), newLink(t, pool, workspaceID)}
	credentials := []string{issueKey(workspaceID), issueKey(workspaceID)}
	otherLink, otherCredential := newLink(t, pool, otherID), issueKey(otherID)

	blocked := printed(t, env, "block-workspace", "--workspace", workspaceID)
	want := map[string]any{"id": workspaceID, "name": "Acme", "links_blocked": 2, "keys_revoked": 2}
	if !equalJSON(blocked, want) {
		t.Errorf("block-workspace printed %v, want %v", blocked, want)
	}

	var state string
	if err := pool.QueryRow(t.Context(), `SELECT state FROM workspaces WHERE id = $1`, workspaceID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "blocked" {
		t.Errorf("the workspace is %q, want it blocked", state)
	}
	for _, blockedLink := range links {
		if got := stateOf(t, pool, blockedLink.ID); got != "blocked" {
			t.Errorf("a link of the workspace is %q, want it blocked", got)
		}
	}
	for _, credential := range credentials {
		if _, err := keys.Authenticate(t.Context(), credential); !errors.Is(err, auth.ErrInvalidKey) {
			t.Errorf("a key of the workspace gives %v, want %v", err, auth.ErrInvalidKey)
		}
	}
	if got := stateOf(t, pool, otherLink.ID); got != "active" {
		t.Errorf("the link of another workspace is %q, want it active", got)
	}
	if _, err := keys.Authenticate(t.Context(), otherCredential); err != nil {
		t.Errorf("the key of another workspace gives %v, want none", err)
	}

	// Nothing more can be issued in it, and blocking it again is not an
	// error.
	code, stdout, log := operate(t, env, "issue-key", "--workspace", workspaceID, "--name", "n", "--scope", "links:read")
	if code != exitFailure || stdout != "" || !strings.Contains(log, "workspace is blocked") {
		t.Errorf("issue-key in the blocked workspace: exit code = %d and %q printed; log: %s", code, stdout, log)
	}
	again := printed(t, env, "block-workspace", "--workspace", workspaceID)
	if !equalJSON(again, map[string]any{"id": workspaceID, "name": "Acme", "links_blocked": 0, "keys_revoked": 0}) {
		t.Errorf("blocking again printed %v", again)
	}

	code, stdout, log = operate(t, env, "block-workspace", "--workspace", "00000000-0000-7000-8000-000000000000")
	if code != exitFailure || stdout != "" || !strings.Contains(log, "no such workspace") {
		t.Errorf("a workspace that does not exist: exit code = %d and %q printed; log: %s", code, stdout, log)
	}
}

// The key the operator prints opens the API: the whole way, from the
// command to the database and back through the process that serves.
func TestAKeyIssuedByTheOperatorOpensTheAPI(t *testing.T) {
	env, _ := migrated(t)
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	issued := printed(t, env, "issue-key", "--workspace", workspaceID, "--name", "Deploy pipeline",
		"--scope", "links:read", "--scope", "metrics:read")
	keyID, _ := issued["id"].(string)
	credential, _ := issued["key"].(string)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &lockedBuffer{}
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"api"}, env, io.Discard, stderr) }()
	url := "http://" + announced(t, stderr, "api listening") + "/v1/key"

	// The client checks each answer against the contract.
	client := contracttest.Client(t)
	ask := func(authorization string) (int, string) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}

	status, body := ask("Bearer " + credential)
	want := map[string]any{
		"id": keyID, "workspace_id": workspaceID, "scopes": []string{"links:read", "metrics:read"}, "expires_at": nil,
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil || status != http.StatusOK || !equalJSON(got, want) {
		t.Errorf("GET /v1/key with the key = %d %s, want 200 %v", status, body, want)
	}
	if status, _ := ask(""); status != http.StatusUnauthorized {
		t.Errorf("GET /v1/key without a key = %d, want 401", status)
	}

	// Revoking takes effect on the next request: the API reads the key
	// each time.
	printed(t, env, "revoke-key", "--key", keyID)
	if status, _ := ask("Bearer " + credential); status != http.StatusUnauthorized {
		t.Errorf("GET /v1/key with the revoked key = %d, want 401", status)
	}

	// The log of the process names the key and never carries it.
	_, secret, _ := strings.Cut(strings.TrimPrefix(credential, "cor_"), "_")
	if log := stderr.String(); !strings.Contains(log, keyID) || strings.Contains(log, secret) {
		t.Error("the log of the api process does not name the key, or carries its secret")
	}

	cancel()
	select {
	case code := <-exited:
		if code != exitOK {
			t.Errorf("exit code = %d after a clean stop; log: %s", code, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the process did not stop")
	}
}
