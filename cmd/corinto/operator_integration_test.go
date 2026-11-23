//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/postgres"
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

func TestTheOperatorBlocksAWorkspaceWithItsLinksItsKeysAndItsSessions(t *testing.T) {
	env, pool := migrated(t)
	keys := auth.NewKeys(pool, rand.Reader)
	sessions := auth.NewSessions(pool, rand.Reader)
	// A session opens in a demo workspace alone, and the operator creates
	// none: the seed does.
	enter := func(workspaceID string) string {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `UPDATE workspaces SET demo = true WHERE id = $1::uuid`, workspaceID); err != nil {
			t.Fatal(err)
		}
		_, token, err := sessions.Open(t.Context(), workspaceID)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
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
	tokens := []string{enter(workspaceID), enter(workspaceID), enter(workspaceID)}
	otherLink, otherCredential, otherToken := newLink(t, pool, otherID), issueKey(otherID), enter(otherID)

	blocked := printed(t, env, "block-workspace", "--workspace", workspaceID)
	want := map[string]any{"id": workspaceID, "name": "Acme", "links_blocked": 2, "keys_revoked": 2, "sessions_closed": 3}
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
	for _, token := range tokens {
		if _, err := sessions.Authenticate(t.Context(), token); !errors.Is(err, auth.ErrInvalidSession) {
			t.Errorf("a session of the workspace gives %v, want %v", err, auth.ErrInvalidSession)
		}
	}
	if _, err := sessions.Authenticate(t.Context(), otherToken); err != nil {
		t.Errorf("the session of another workspace gives %v, want none", err)
	}

	// Nothing more can be issued in it, nobody enters it, and blocking it
	// again is not an error.
	code, stdout, log := operate(t, env, "issue-key", "--workspace", workspaceID, "--name", "n", "--scope", "links:read")
	if code != exitFailure || stdout != "" || !strings.Contains(log, "workspace is blocked") {
		t.Errorf("issue-key in the blocked workspace: exit code = %d and %q printed; log: %s", code, stdout, log)
	}
	if _, _, err := sessions.Open(t.Context(), workspaceID); !errors.Is(err, link.ErrWorkspaceBlocked) {
		t.Errorf("a session in the blocked workspace: %v, want %v", err, link.ErrWorkspaceBlocked)
	}
	again := printed(t, env, "block-workspace", "--workspace", workspaceID)
	if !equalJSON(again, map[string]any{"id": workspaceID, "name": "Acme", "links_blocked": 0, "keys_revoked": 0, "sessions_closed": 0}) {
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

// A link created through the API is the link the operator blocks: the two
// commands and the database between them, end to end.
func TestALinkCreatedThroughTheAPIIsTheOneTheOperatorBlocks(t *testing.T) {
	env, _ := migrated(t)
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	credential, _ := printed(t, env, "issue-key", "--workspace", workspaceID, "--name", "Deploy pipeline",
		"--scope", "links:read", "--scope", "links:write")["key"].(string)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &lockedBuffer{}
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"api"}, env, io.Discard, stderr) }()
	api := "http://" + announced(t, stderr, "api listening")

	client := contracttest.Client(t)
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		var sent io.Reader
		if body != "" {
			sent = strings.NewReader(body)
		}
		request, err := http.NewRequestWithContext(t.Context(), method, api+path, sent)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+credential)
		switch {
		case body != "" && method == http.MethodPatch:
			request.Header.Set("Content-Type", "application/merge-patch+json")
		case body != "":
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		read, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		var answered map[string]any
		if len(read) > 0 {
			if err := json.Unmarshal(read, &answered); err != nil {
				t.Fatal(err)
			}
		}
		return response.StatusCode, answered
	}

	status, created := call(http.MethodPost, "/v1/links", `{"destination":"https://example.com/landing","campaign":{"source":"newsletter"}}`)
	if status != http.StatusCreated || created["domain"] != redirectDomain || created["state"] != "active" ||
		created["resolved_destination"] != "https://example.com/landing?utm_source=newsletter" {
		t.Fatalf("POST /v1/links = %d %v", status, created)
	}
	id, _ := created["id"].(string)
	identifier, _ := created["identifier"].(string)

	// The workspace changes its link and switches it off and on.
	if status, changed := call(http.MethodPatch, "/v1/links/"+id, `{"title":"Launch","campaign":{"medium":"email"}}`); status != http.StatusOK ||
		changed["title"] != "Launch" || changed["resolved_destination"] != "https://example.com/landing?utm_source=newsletter&utm_medium=email" {
		t.Errorf("PATCH of the link = %d %v", status, changed)
	}
	if status, disabled := call(http.MethodPost, "/v1/links/"+id+"/disable", ""); status != http.StatusOK || disabled["state"] != "disabled" {
		t.Errorf("disable = %d %v", status, disabled)
	}
	if status, enabled := call(http.MethodPost, "/v1/links/"+id+"/enable", ""); status != http.StatusOK || enabled["state"] != "active" {
		t.Errorf("enable = %d %v", status, enabled)
	}

	blocked := printed(t, env, "block-link", "--identifier", identifier)
	if blocked["id"] != id || blocked["workspace_id"] != workspaceID {
		t.Errorf("block-link printed %v, want the link %s of the workspace %s", blocked, id, workspaceID)
	}
	if status, read := call(http.MethodGet, "/v1/links/"+id, ""); status != http.StatusOK || read["state"] != "blocked" {
		t.Errorf("GET of the link after the block = %d with the state %v, want 200 and blocked", status, read["state"])
	}
	if status, listed := call(http.MethodGet, "/v1/links", ""); status != http.StatusOK || len(listed["items"].([]any)) != 1 {
		t.Errorf("GET /v1/links = %d %v, want the one link", status, listed)
	}
	// Once blocked, the workspace can only read it and delete it.
	if status, refused := call(http.MethodPatch, "/v1/links/"+id, `{"title":"Again"}`); status != http.StatusConflict || refused["code"] != "link_blocked" {
		t.Errorf("PATCH of the blocked link = %d %v, want 409 link_blocked", status, refused)
	}
	if status, _ := call(http.MethodDelete, "/v1/links/"+id, ""); status != http.StatusNoContent {
		t.Errorf("DELETE of the blocked link = %d, want 204", status)
	}
	if status, _ := call(http.MethodGet, "/v1/links/"+id, ""); status != http.StatusNotFound {
		t.Errorf("GET of the deleted link = %d, want 404", status)
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

// The sixth action: the operator sends back to the inbox the events that
// failed on every attempt, one or all of them, and the worker processes
// them (docs/delivery-semantics.md, statement 5).
func TestTheOperatorSendsDeadLettersBackToTheInbox(t *testing.T) {
	env, pool := migrated(t)
	workspaceID, _ := printed(t, env, "create-workspace", "--name", "Acme")["id"].(string)
	var linkID string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO links (workspace_id, domain, identifier, destination, resolved_destination)
		 VALUES ($1, $2, 'k7m2xq9d', 'https://example.com/', 'https://example.com/') RETURNING id::text`,
		workspaceID, redirectDomain).Scan(&linkID); err != nil {
		t.Fatal(err)
	}

	// Three events that the redirect served, accepted, and failed on every
	// attempt of a worker.
	inbox := ingest.NewStore(pool, slog.New(slog.DiscardHandler))
	events := make([]ingest.Event, 3)
	for i := range events {
		events[i] = ingest.Event{
			ID: uuid.NewV7().String(), LinkID: linkID, WorkspaceID: workspaceID,
			OccurredAt: time.Date(2026, time.November, 19, 12, i, 0, 0, time.UTC), Outcome: ingest.OutcomeRedirected, Method: http.MethodGet,
			Classification: classify.Classification{
				Class: classify.ClassPresumedHuman, Rule: "presumed_human", Version: 1,
				Device: classify.DeviceDesktop, Browser: classify.BrowserChrome,
			},
		}
	}
	if settled, err := inbox.Accept(t.Context(), events); err != nil || settled != 3 {
		t.Fatalf("Accept settled %d events with the error %v", settled, err)
	}
	failing, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = failing.Rollback(context.WithoutCancel(t.Context())) }()
	for _, event := range events {
		for range ingest.MaxAttempts {
			if _, err := ingest.Fail(t.Context(), failing, event, postgres.Refusal{Code: "23514", Constraint: "a_constraint"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := failing.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	state := func() (dead, counted, pending int) {
		t.Helper()
		err := pool.QueryRow(t.Context(),
			`SELECT (SELECT count(*) FROM event_dead_letters),
			        (SELECT coalesce(sum(value), 0) FROM pipeline_counters WHERE name = 'dead_lettered'),
			        (SELECT count(*) FROM event_inbox WHERE processed_at IS NULL AND attempts = 0 AND next_attempt_at IS NULL)`).
			Scan(&dead, &counted, &pending)
		if err != nil {
			t.Fatal(err)
		}
		return dead, counted, pending
	}
	if dead, counted, pending := state(); dead != 3 || counted != 3 || pending != 0 {
		t.Fatalf("%d dead letters, %d counted and %d pending; want 3, 3 and none", dead, counted, pending)
	}

	// One, by the identifier the worker logged.
	code, stdout, log := operate(t, env, "retry-dead-letters", "--event", events[1].ID)
	if code != exitOK || strings.TrimSpace(stdout) != `{"events":1}` {
		t.Fatalf("retry-dead-letters --event: exit code = %d, printed %q; log: %s", code, stdout, log)
	}
	if !strings.Contains(log, `"action":"retry-dead-letters"`) || !strings.Contains(log, `"event":"`+events[1].ID+`"`) {
		t.Errorf("the log does not name the action and its event: %s", log)
	}
	if dead, counted, pending := state(); dead != 2 || counted != 2 || pending != 1 {
		t.Fatalf("%d dead letters, %d counted and %d pending; want 2, 2 and 1", dead, counted, pending)
	}

	// What is not a dead letter is refused, and nothing is printed.
	for name, id := range map[string]string{
		"the event that was sent back": events[1].ID, "an identifier of nothing": "0193a000-0000-7000-8000-0000000000ff",
		"something that is not an identifier": "not-an-identifier",
	} {
		code, stdout, log := operate(t, env, "retry-dead-letters", "--event", id)
		if code != exitFailure || stdout != "" || !strings.Contains(log, "there is no such dead-lettered event") {
			t.Errorf("%s: exit code = %d, printed %q; log: %s", name, code, stdout, log)
		}
	}
	if dead, counted, pending := state(); dead != 2 || counted != 2 || pending != 1 {
		t.Fatalf("%d dead letters, %d counted and %d pending; want 2, 2 and 1", dead, counted, pending)
	}

	// All the rest, and then there are none.
	if got := printed(t, env, "retry-dead-letters", "--all")["events"]; got != float64(2) {
		t.Fatalf("retry-dead-letters --all sent %v events back, want 2", got)
	}
	if dead, counted, pending := state(); dead != 0 || counted != 0 || pending != 3 {
		t.Fatalf("%d dead letters, %d counted and %d pending; want none, none and 3", dead, counted, pending)
	}
	if got := printed(t, env, "retry-dead-letters", "--all")["events"]; got != float64(0) {
		t.Errorf("with no dead letters, retry-dead-letters --all sent %v events back", got)
	}

	// A worker processes them like any other event.
	process(t, env, "worker")
	deadline := time.Now().Add(15 * time.Second)
	for {
		var figures, left int
		if err := pool.QueryRow(t.Context(),
			`SELECT (SELECT coalesce(sum(events), 0) FROM link_hourly), (SELECT count(*) FROM event_inbox WHERE processed_at IS NULL)`).
			Scan(&figures, &left); err != nil {
			t.Fatal(err)
		}
		if figures == 3 && left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 15 seconds the figures have %d events and %d are pending, want 3 and none", figures, left)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Accepted once, whatever happened to them afterwards.
	var accepted int
	if err := pool.QueryRow(t.Context(), `SELECT coalesce(sum(value), 0) FROM pipeline_counters WHERE name = 'accepted'`).Scan(&accepted); err != nil || accepted != 3 {
		t.Errorf("%d events are counted as accepted (%v), want 3", accepted, err)
	}
}
