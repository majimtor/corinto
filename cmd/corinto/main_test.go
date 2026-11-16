package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// environmentOf returns a getenv that knows only the given variables.
func environmentOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestRunRejectsWrongUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"serve"}},
		{"extra argument", []string{"api", "now"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(t.Context(), c.args, environmentOf(nil), &stderr); code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.HasPrefix(stderr.String(), "usage: corinto") {
				t.Errorf("stderr does not start with the usage text: %q", stderr.String())
			}
		})
	}
}

func TestAPINamesEveryMissingVariable(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"api"}, environmentOf(nil), &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	for _, name := range []string{"CORINTO_DATABASE_URL", "CORINTO_HEALTH_ADDR"} {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("stderr does not name %s: %q", name, stderr.String())
		}
	}
}

func TestAPIKeepsThePasswordOutOfTheLog(t *testing.T) {
	// A sentinel, not a credential: the URL is malformed on purpose, and the
	// test fails if the error that reports it echoes the password.
	const sentinel = "sentinel-not-a-password"
	values := map[string]string{
		"CORINTO_DATABASE_URL": "postgres://corinto:" + sentinel + "@db:not-a-port/corinto",
		"CORINTO_HEALTH_ADDR":  "127.0.0.1:0",
	}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"api"}, environmentOf(values), &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if strings.Contains(stderr.String(), sentinel) {
		t.Errorf("the log carries the password: %q", stderr.String())
	}
}

func TestAPIStartsWithoutTheDatabaseAndSaysItIsNotReady(t *testing.T) {
	values := map[string]string{
		// Nothing listens on port 1 of the loopback interface.
		"CORINTO_DATABASE_URL": "postgres://corinto@127.0.0.1:1/corinto?connect_timeout=1",
		"CORINTO_HEALTH_ADDR":  "127.0.0.1:0",
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &lockedBuffer{}
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"api"}, environmentOf(values), stderr) }()

	addr := healthAddr(t, stderr)
	if got := status(t, "http://"+addr+"/livez"); got != http.StatusOK {
		t.Errorf("/livez = %d, want %d", got, http.StatusOK)
	}
	if got := status(t, "http://"+addr+"/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want %d", got, http.StatusServiceUnavailable)
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

// healthAddr waits for the log line that announces the health endpoints and
// returns the address in it.
func healthAddr(t *testing.T, log *lockedBuffer) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		scanner := bufio.NewScanner(strings.NewReader(log.String()))
		for scanner.Scan() {
			var line struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Msg == "health endpoints listening" {
				return line.Addr
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the process never announced its health endpoints; log: %s", log.String())
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
