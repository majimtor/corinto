//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/postgres/postgrestest"
)

// The host that serves the short links of these tests.
const redirectDomain = "go.example.dev"

func TestMain(m *testing.M) { postgrestest.Main(m) }

// environmentFor returns what the binary reads, pointed at a database.
func environmentFor(databaseURL string) func(string) string {
	return environmentOf(map[string]string{
		"CORINTO_DATABASE_URL":    databaseURL,
		"CORINTO_API_ADDR":        "127.0.0.1:0",
		"CORINTO_REDIRECT_ADDR":   "127.0.0.1:0",
		"CORINTO_HEALTH_ADDR":     "127.0.0.1:0",
		"CORINTO_REDIRECT_DOMAIN": redirectDomain,
	})
}

func TestMigrateThenTheAPIStartsAndIsReady(t *testing.T) {
	env := environmentFor(postgrestest.NewDatabase(t))

	var first bytes.Buffer
	if code := run(t.Context(), []string{"migrate"}, env, io.Discard, &first); code != exitOK {
		t.Fatalf("migrate exit code = %d, want %d; log: %s", code, exitOK, first.String())
	}
	if !strings.Contains(first.String(), "migration applied") {
		t.Errorf("the first run does not log what it applied: %s", first.String())
	}

	// Running it again changes nothing and is not an error.
	var second bytes.Buffer
	if code := run(t.Context(), []string{"migrate"}, env, io.Discard, &second); code != exitOK {
		t.Fatalf("second migrate exit code = %d, want %d; log: %s", code, exitOK, second.String())
	}
	if strings.Contains(second.String(), "migration applied") {
		t.Errorf("the second run applied something: %s", second.String())
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &lockedBuffer{}
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"api"}, env, io.Discard, stderr) }()

	addr := announced(t, stderr, "health endpoints listening")
	if got := status(t, "http://"+addr+"/livez"); got != http.StatusOK {
		t.Errorf("/livez = %d, want %d", got, http.StatusOK)
	}
	if got := status(t, "http://"+addr+"/readyz"); got != http.StatusOK {
		t.Errorf("/readyz = %d, want %d", got, http.StatusOK)
	}

	// The API answers on its own port, and what it answers is checked
	// against the contract by the client.
	client := contracttest.Client(t)
	apiURL := "http://" + announced(t, stderr, "api listening")
	for path, want := range map[string]int{
		"/v1/health":       http.StatusOK,
		"/v1/openapi.yaml": http.StatusOK,
		"/v1/nothing":      http.StatusNotFound,
		"/readyz":          http.StatusNotFound,
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, apiURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Errorf("GET %s on the port of the API = %d, want %d", path, response.StatusCode, want)
		}
	}
	// And the port of the probes does not serve the API.
	if got := status(t, "http://"+addr+"/v1/health"); got != http.StatusNotFound {
		t.Errorf("/v1/health on the port of the probes = %d, want %d", got, http.StatusNotFound)
	}

	cancel()
	select {
	case code := <-exited:
		if code != exitOK {
			t.Errorf("exit code = %d after a clean stop, want %d; log: %s", code, exitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the process did not stop")
	}
}

// refusal bounds a start that must be refused. If the process started
// anyway it would serve until the deadline and exit cleanly, and the test
// fails by its exit code instead of waiting for the timeout of go test.
const refusal = 15 * time.Second

func TestTheAPIRefusesADatabaseThatWasNotMigrated(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), refusal)
	defer cancel()
	var stderr bytes.Buffer
	code := run(ctx, []string{"api"}, environmentFor(postgrestest.NewDatabase(t)), io.Discard, &stderr)
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	// The version of the binary grows with every migration, so it is asked
	// for and not written here.
	built, err := postgres.BuiltVersion()
	if err != nil {
		t.Fatal(err)
	}
	binary := fmt.Sprintf("the binary at %d", built)
	for _, want := range []string{"the database is at version 0", binary, "corinto migrate"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the log does not say %q: %s", want, stderr.String())
		}
	}
}

// A binary older than the database must not run against it either.
func TestTheAPIRefusesADatabaseAheadOfTheBinary(t *testing.T) {
	databaseURL := postgrestest.NewDatabase(t)
	env := environmentFor(databaseURL)
	var log bytes.Buffer
	if code := run(t.Context(), []string{"migrate"}, env, io.Discard, &log); code != exitOK {
		t.Fatalf("migrate exit code = %d; log: %s", code, log.String())
	}

	// What a newer binary would have left behind.
	conn, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	if _, err := conn.Exec(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (99, true)`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), refusal)
	defer cancel()
	var stderr bytes.Buffer
	if code := run(ctx, []string{"api"}, env, io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "the database is at version 99") {
		t.Errorf("the log does not name the version of the database: %s", stderr.String())
	}
}

// announced waits for the log line with which the process says that
// something listens, and returns the address in it.
func announced(t *testing.T, log *lockedBuffer, message string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		scanner := bufio.NewScanner(strings.NewReader(log.String()))
		for scanner.Scan() {
			var line struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Msg == message {
				return line.Addr
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the process never logged %q; log: %s", message, log.String())
	return ""
}

func status(t *testing.T, url string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.StatusCode
}

// lockedBuffer is a buffer that the process under test writes and the test
// reads at the same time.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
