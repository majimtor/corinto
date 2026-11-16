package health_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/health"
)

func TestHandler(t *testing.T) {
	down := errors.New("database down")
	cases := []struct {
		name   string
		method string
		path   string
		ready  error
		status int
		body   string // checked only when not empty
	}{
		{"live", http.MethodGet, "/livez", nil, http.StatusOK, "live\n"},
		{"live does not depend on readiness", http.MethodGet, "/livez", down, http.StatusOK, "live\n"},
		{"ready", http.MethodGet, "/readyz", nil, http.StatusOK, "ready\n"},
		{"not ready", http.MethodGet, "/readyz", down, http.StatusServiceUnavailable, "not ready\n"},
		{"a probe cannot write", http.MethodPost, "/readyz", nil, http.StatusMethodNotAllowed, ""},
		{"nothing else is served", http.MethodGet, "/metrics", nil, http.StatusNotFound, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handler := health.Handler(func(context.Context) error { return c.ready }, slog.New(slog.DiscardHandler))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), c.method, c.path, nil))

			if recorder.Code != c.status {
				t.Errorf("status = %d, want %d", recorder.Code, c.status)
			}
			if c.body != "" && recorder.Body.String() != c.body {
				t.Errorf("body = %q, want %q", recorder.Body.String(), c.body)
			}
			// Every response with a body declares its type and forbids sniffing.
			if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
		})
	}
}

func TestTheReasonGoesToTheLogAndNotToTheResponse(t *testing.T) {
	var log bytes.Buffer
	handler := health.Handler(
		func(context.Context) error { return errors.New("password authentication failed") },
		slog.New(slog.NewJSONHandler(&log, nil)),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

	if strings.Contains(recorder.Body.String(), "password") {
		t.Errorf("the response gives the reason away: %q", recorder.Body.String())
	}
	if !strings.Contains(log.String(), "password authentication failed") {
		t.Errorf("the log does not carry the reason: %q", log.String())
	}
}

func TestServeStopsWhenItsContextIsCancelled(t *testing.T) {
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stopped := make(chan error, 1)
	go func() {
		stopped <- health.Serve(ctx, listener, func(context.Context) error { return nil }, slog.New(slog.DiscardHandler))
	}()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String()+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Serve returned %v after a clean stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
