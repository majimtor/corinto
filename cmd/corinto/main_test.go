package main

import (
	"bytes"
	"context"
	"io"
	"strings"
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
		{"extra argument to migrate", []string{"migrate", "up"}},
		{"extra argument to redirect", []string{"redirect", "now"}},
		{"extra argument to seed", []string{"seed", "again"}},
		{"an option the seed does not have", []string{"seed", "--again"}},
		{"extra argument after the option of the seed", []string{"seed", "--if-missing", "now"}},
		{"the option of the seed given to another command", []string{"migrate", "--if-missing"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(t.Context(), c.args, environmentOf(nil), io.Discard, &stderr); code != exitUsage {
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
	if code := run(t.Context(), []string{"api"}, environmentOf(nil), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	for _, name := range []string{"CORINTO_DATABASE_URL", "CORINTO_API_ADDR", "CORINTO_HEALTH_ADDR", "CORINTO_REDIRECT_DOMAIN"} {
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
		"CORINTO_DATABASE_URL":    "postgres://corinto:" + sentinel + "@db:not-a-port/corinto",
		"CORINTO_API_ADDR":        "127.0.0.1:0",
		"CORINTO_HEALTH_ADDR":     "127.0.0.1:0",
		"CORINTO_REDIRECT_DOMAIN": "go.example.dev",
	}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"api"}, environmentOf(values), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if strings.Contains(stderr.String(), sentinel) {
		t.Errorf("the log carries the password: %q", stderr.String())
	}
}

func TestAPIRefusesToStartWithoutTheDatabase(t *testing.T) {
	// A sentinel, not a credential: the test fails if the error that reports
	// the missing database echoes the password of the URL.
	const sentinel = "sentinel-not-a-password"
	values := map[string]string{
		// Nothing listens on port 1 of the loopback interface.
		"CORINTO_DATABASE_URL":    "postgres://corinto:" + sentinel + "@127.0.0.1:1/corinto?connect_timeout=1",
		"CORINTO_API_ADDR":        "127.0.0.1:0",
		"CORINTO_HEALTH_ADDR":     "127.0.0.1:0",
		"CORINTO_REDIRECT_DOMAIN": "go.example.dev",
	}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"api"}, environmentOf(values), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "schema version") {
		t.Errorf("the log does not say that the schema could not be checked: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "health endpoints listening") {
		t.Errorf("the process started to listen without having checked the schema: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), sentinel) {
		t.Errorf("the log carries the password: %q", stderr.String())
	}
}

func TestMigrateNamesItsMissingVariable(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"migrate"}, environmentOf(nil), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "CORINTO_DATABASE_URL") {
		t.Errorf("stderr does not name CORINTO_DATABASE_URL: %q", stderr.String())
	}
}

func TestSeedNamesEveryMissingVariable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"seed"}, environmentOf(nil), &stdout, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	for _, name := range []string{"CORINTO_DATABASE_URL", "CORINTO_REDIRECT_DOMAIN"} {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("stderr does not name %s: %q", name, stderr.String())
		}
	}
	if stdout.Len() != 0 {
		t.Errorf("a seed that did not run printed %q", stdout.String())
	}
}

func TestAPIRefusesAListOfProxiesItCannotRead(t *testing.T) {
	values := map[string]string{
		"CORINTO_DATABASE_URL":    "postgres://corinto@127.0.0.1:1/corinto?connect_timeout=1",
		"CORINTO_API_ADDR":        "127.0.0.1:0",
		"CORINTO_HEALTH_ADDR":     "127.0.0.1:0",
		"CORINTO_REDIRECT_DOMAIN": "go.example.dev",
		"CORINTO_TRUSTED_PROXIES": "10.0.0.0/8, the-load-balancer",
	}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"api"}, environmentOf(values), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	for _, want := range []string{"CORINTO_TRUSTED_PROXIES", "the-load-balancer"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not say %q: %q", want, stderr.String())
		}
	}
}

func TestWorkerNamesEveryMissingVariable(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"worker"}, environmentOf(nil), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	for _, name := range []string{"CORINTO_DATABASE_URL", "CORINTO_HEALTH_ADDR"} {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("stderr does not name %s: %q", name, stderr.String())
		}
	}
	// It reads nothing of the other processes.
	for _, name := range []string{"CORINTO_API_ADDR", "CORINTO_REDIRECT_ADDR", "CORINTO_REDIRECT_DOMAIN"} {
		if strings.Contains(stderr.String(), name) {
			t.Errorf("stderr names %s, which the worker does not read: %q", name, stderr.String())
		}
	}
}

// A worker that cannot check its schema must not touch the events.
func TestWorkerRefusesToStartWithoutTheDatabase(t *testing.T) {
	const sentinel = "sentinel-not-a-password"
	values := map[string]string{
		// Nothing listens on port 1 of the loopback interface.
		"CORINTO_DATABASE_URL": "postgres://corinto:" + sentinel + "@127.0.0.1:1/corinto?connect_timeout=1",
		"CORINTO_HEALTH_ADDR":  "127.0.0.1:0",
	}
	// A worker that started would run until it is stopped: the time it is
	// given here is the one a refusal never needs.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	if code := run(ctx, []string{"worker"}, environmentOf(values), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "schema version") {
		t.Errorf("the log does not say that the schema could not be checked: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "listening") || strings.Contains(stderr.String(), "consumed") || strings.Contains(stderr.String(), sentinel) {
		t.Errorf("the process started without having checked the schema, or logged the password: %q", stderr.String())
	}
}

func TestRedirectNamesEveryMissingVariable(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"redirect"}, environmentOf(nil), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	for _, name := range []string{"CORINTO_DATABASE_URL", "CORINTO_REDIRECT_ADDR", "CORINTO_HEALTH_ADDR", "CORINTO_REDIRECT_DOMAIN"} {
		if !strings.Contains(stderr.String(), name) {
			t.Errorf("stderr does not name %s: %q", name, stderr.String())
		}
	}
}

// A redirect without its links would answer that none exists. It does not
// start without the database.
func TestRedirectRefusesToStartWithoutTheDatabase(t *testing.T) {
	const sentinel = "sentinel-not-a-password"
	values := map[string]string{
		// Nothing listens on port 1 of the loopback interface.
		"CORINTO_DATABASE_URL":    "postgres://corinto:" + sentinel + "@127.0.0.1:1/corinto?connect_timeout=1",
		"CORINTO_REDIRECT_ADDR":   "127.0.0.1:0",
		"CORINTO_HEALTH_ADDR":     "127.0.0.1:0",
		"CORINTO_REDIRECT_DOMAIN": "go.example.dev",
	}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"redirect"}, environmentOf(values), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if strings.Contains(stderr.String(), "listening") || strings.Contains(stderr.String(), sentinel) {
		t.Errorf("the process listened without its links, or logged the password: %q", stderr.String())
	}
}

func TestAPIRefusesAFormOfTheSessionCookieItDoesNotKnow(t *testing.T) {
	values := map[string]string{
		"CORINTO_DATABASE_URL":    "postgres://corinto@127.0.0.1:1/corinto?connect_timeout=1",
		"CORINTO_API_ADDR":        "127.0.0.1:0",
		"CORINTO_HEALTH_ADDR":     "127.0.0.1:0",
		"CORINTO_REDIRECT_DOMAIN": "go.example.dev",
		"CORINTO_SESSION_COOKIE":  "plain",
	}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"api"}, environmentOf(values), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	for _, want := range []string{"CORINTO_SESSION_COOKIE", "plain", "secure", "loopback"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not say %q: %q", want, stderr.String())
		}
	}
}

func TestRedirectRefusesAListOfProxiesItCannotRead(t *testing.T) {
	values := map[string]string{
		"CORINTO_DATABASE_URL":    "postgres://corinto@127.0.0.1:1/corinto?connect_timeout=1",
		"CORINTO_REDIRECT_ADDR":   "127.0.0.1:0",
		"CORINTO_HEALTH_ADDR":     "127.0.0.1:0",
		"CORINTO_REDIRECT_DOMAIN": "go.example.dev",
		"CORINTO_TRUSTED_PROXIES": "the-load-balancer",
	}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"redirect"}, environmentOf(values), io.Discard, &stderr); code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	for _, want := range []string{"CORINTO_TRUSTED_PROXIES", "the-load-balancer"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not say %q: %q", want, stderr.String())
		}
	}
}
