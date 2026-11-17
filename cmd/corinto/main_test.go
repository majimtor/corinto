package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
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
		"CORINTO_DATABASE_URL": "postgres://corinto:" + sentinel + "@127.0.0.1:1/corinto?connect_timeout=1",
		"CORINTO_HEALTH_ADDR":  "127.0.0.1:0",
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
