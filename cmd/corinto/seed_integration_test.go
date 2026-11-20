//go:build integration

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

// seededWorkspace is a demo workspace as the seed command prints it.
type seededWorkspace struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	KeyID string   `json:"key_id"`
	Key   string   `json:"key"`
	Links []string `json:"links"`
}

// seedOnce runs the seed command, which must succeed, and returns what it
// printed and what it logged.
func seedOnce(t *testing.T, env func(string) string) ([]seededWorkspace, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"seed"}, env, &stdout, &stderr); code != exitOK {
		t.Fatalf("seed exit code = %d; log: %s", code, stderr.String())
	}
	var printed struct {
		Workspaces []seededWorkspace `json:"workspaces"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil || strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("the seed did not print one JSON object on one line: %v: %q", err, stdout.String())
	}
	return printed.Workspaces, stderr.String()
}

// The seed leaves two demo workspaces that can be used at once: the key it
// prints for each opens the API, the links redirect, and a key of one
// cannot reach what is of the other.
func TestTheSeedPrintsKeysThatOpenTheAPIAndLinksThatRedirect(t *testing.T) {
	env, pool := migrated(t)
	seeded, log := seedOnce(t, env)
	if len(seeded) != 2 || len(seeded[0].Links) != 10 || len(seeded[1].Links) != 6 || seeded[0].Name != "Lighthouse Books" || seeded[1].Name != "Cascade Cycles" {
		t.Fatalf("the seed printed %+v, want its two workspaces with 10 and 6 links", seeded)
	}
	// The log names each workspace and its key, and never carries a key.
	for _, workspace := range seeded {
		_, secret, _ := strings.Cut(strings.TrimPrefix(workspace.Key, "cor_"), "_")
		if !strings.HasPrefix(workspace.Key, "cor_") || len(secret) < 40 {
			t.Fatalf("the key of %s is not a credential", workspace.Name)
		}
		if !strings.Contains(log, workspace.ID) || !strings.Contains(log, workspace.KeyID) || strings.Contains(log, secret) {
			t.Errorf("the log does not name %s and its key, or carries the secret", workspace.Name)
		}
	}
	if strings.Count(log, "demo workspace seeded") != 2 {
		t.Errorf("the log of the seed: %s", log)
	}

	api := "http://" + announced(t, process(t, env, "api"), "api listening")
	client := contracttest.Client(t)
	ask := func(credential, path string) (int, []byte) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, api+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+credential)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	type page struct {
		Items []struct {
			ID, Identifier string
			Resolved       string `json:"resolved_destination"`
		} `json:"items"`
	}
	var first, second page
	for n, listed := range []*page{&first, &second} {
		status, body := ask(seeded[n].Key, "/v1/links")
		if err := json.Unmarshal(body, listed); err != nil || status != http.StatusOK || len(listed.Items) != len(seeded[n].Links) {
			t.Fatalf("with the key of %s, GET /v1/links = %d with %d links (%v), want its %d", seeded[n].Name, status, len(listed.Items), err, len(seeded[n].Links))
		}
		for i, item := range listed.Items {
			if item.Identifier != seeded[n].Links[i] {
				t.Errorf("link %d of %s is %s, want %s", i, seeded[n].Name, item.Identifier, seeded[n].Links[i])
			}
		}
	}
	// What the two workspaces are for: a key of one against a link of the
	// other.
	if status, _ := ask(seeded[0].Key, "/v1/links/"+second.Items[0].ID); status != http.StatusNotFound {
		t.Errorf("a link of %s read with the key of %s = %d, want 404", seeded[1].Name, seeded[0].Name, status)
	}
	if status, _ := ask(seeded[1].Key, "/v1/links/"+second.Items[0].ID); status != http.StatusOK {
		t.Errorf("a link of %s read with its own key = %d, want 200", seeded[1].Name, status)
	}

	// The links redirect, but for the one that has expired.
	short := "http://" + announced(t, process(t, env, "redirect"), "redirect listening")
	if status, destination := follow(t, short+"/spring-reading"); status != http.StatusFound ||
		destination != "https://www.example.com/books/spring-reading?utm_source=newsletter&utm_medium=email&utm_campaign=spring-reading" {
		t.Errorf("the short link of the seed = %d to %q", status, destination)
	}
	if status, destination := follow(t, short+"/workshop-booking"); status != http.StatusFound || destination != "https://www.example.org/workshop/booking" {
		t.Errorf("the short link of the second workspace = %d to %q", status, destination)
	}
	if status, _ := follow(t, short+"/winter-clearance"); status != http.StatusNotFound {
		t.Errorf("the link of the seed that has expired = %d, want 404", status)
	}

	// A second seed leaves new workspaces with new keys. The old keys open
	// nothing: the API reads the key of each request.
	again, _ := seedOnce(t, env)
	if len(again) != 2 || again[0].ID == seeded[0].ID || again[0].Key == seeded[0].Key || again[1].Key == seeded[1].Key {
		t.Fatalf("the second seed printed the workspaces or the keys of the first")
	}
	if status, _ := ask(seeded[0].Key, "/v1/key"); status != http.StatusUnauthorized {
		t.Errorf("the key of the first seed = %d after the second, want 401", status)
	}
	if status, _ := ask(again[0].Key, "/v1/key"); status != http.StatusOK {
		t.Errorf("the key of the second seed = %d, want 200", status)
	}
	if demos, links := count(t, pool, "workspaces"), count(t, pool, "links"); demos != 2 || links != 16 {
		t.Errorf("after two seeds there are %d workspaces and %d links, want 2 and 16", demos, links)
	}
}

// What `make up` runs: the first time it seeds and prints the keys, and
// after that it changes nothing and prints none.
func TestTheSeedThatStartsWithTheSystemSeedsOnce(t *testing.T) {
	env, pool := migrated(t)
	seed := func() ([]seededWorkspace, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{"seed", "--if-missing"}, env, &stdout, &stderr); code != exitOK {
			t.Fatalf("seed --if-missing exit code = %d; log: %s", code, stderr.String())
		}
		var printed struct {
			Workspaces []seededWorkspace `json:"workspaces"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil || strings.Count(stdout.String(), "\n") != 1 || printed.Workspaces == nil {
			t.Fatalf("the seed did not print one JSON object with its workspaces on one line: %v: %q", err, stdout.String())
		}
		return printed.Workspaces, stderr.String()
	}
	first, log := seed()
	if len(first) != 2 || len(first[0].Links) != 10 || !strings.HasPrefix(first[0].Key, "cor_") || strings.Count(log, "demo workspace seeded") != 2 {
		t.Fatalf("the first seed printed %d workspaces; log: %s", len(first), log)
	}
	again, log := seed()
	if len(again) != 0 || !strings.Contains(log, "nothing was seeded") || strings.Contains(log, "demo workspace seeded") {
		t.Fatalf("the second seed printed %d workspaces; log: %s", len(again), log)
	}
	// The workspaces and the keys of the first are the ones that are there.
	keys := auth.NewKeys(pool, rand.Reader)
	for _, workspace := range first {
		if principal, err := keys.Authenticate(t.Context(), workspace.Key); err != nil || principal.WorkspaceID != workspace.ID {
			t.Errorf("the key of %s after a second start: %+v, %v", workspace.Name, principal, err)
		}
	}
	if workspaces, links := count(t, pool, "workspaces"), count(t, pool, "links"); workspaces != 2 || links != 16 {
		t.Errorf("after two starts there are %d workspaces and %d links, want 2 and 16", workspaces, links)
	}
	// Asked without the option, the seed creates them again whatever is
	// there.
	if replaced, _ := seedOnce(t, env); len(replaced) != 2 || replaced[0].ID == first[0].ID {
		t.Error("the seed without the option left the workspaces that were there")
	}
}

func TestTheSeedRefusesADatabaseThatWasNotMigrated(t *testing.T) {
	env := environmentFor(postgrestest.NewDatabase(t))
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"seed"}, env, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit code = %d, want %d; log: %s", code, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "corinto migrate") || stdout.Len() != 0 {
		t.Errorf("the seed did not name the command to run, or printed something: %s%s", stderr.String(), stdout.String())
	}
}
