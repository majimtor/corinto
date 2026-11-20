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
		{"a process with nothing to say of itself has no status", http.MethodGet, "/statusz", nil, http.StatusNotFound, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handler := health.Handler(func(context.Context) error { return c.ready }, nil, slog.New(slog.DiscardHandler))
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
		nil, slog.New(slog.NewJSONHandler(&log, nil)),
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

// A process that measures something about itself says it at /statusz, as
// JSON, whether it is ready or not: it is when something fails that the
// answer matters.
func TestAProcessSaysWhatItMeasuresAboutItself(t *testing.T) {
	measured := struct {
		Replica struct {
			AgeSeconds float64 `json:"age_seconds"`
			Links      int     `json:"links"`
		} `json:"replica"`
	}{}
	measured.Replica.AgeSeconds, measured.Replica.Links = 1.5, 3
	asked := 0
	status := func() any {
		asked++
		return measured
	}
	for _, ready := range []error{nil, errors.New("database down")} {
		handler := health.Handler(func(context.Context) error { return ready }, status, slog.New(slog.DiscardHandler))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/statusz", nil))
		if want := `{"replica":{"age_seconds":1.5,"links":3}}` + "\n"; recorder.Code != http.StatusOK || recorder.Body.String() != want {
			t.Errorf("GET /statusz = %d %q, want 200 %q", recorder.Code, recorder.Body.String(), want)
		}
		header := recorder.Header()
		if header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Cache-Control") != "no-store" {
			t.Errorf("the headers of the status are %v", header)
		}
	}
	// Asked once for each answer: what it says is of the moment it is asked.
	if asked != 2 {
		t.Errorf("the status was read %d times for two answers", asked)
	}

	handler := health.Handler(func(context.Context) error { return nil }, status, slog.New(slog.DiscardHandler))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/statusz", nil))
	if recorder.Code != http.StatusMethodNotAllowed || asked != 2 {
		t.Errorf("POST /statusz = %d, and the status was read %d times; want 405 and no reading", recorder.Code, asked)
	}

	// A status that cannot be written is said in the log, and not to whoever
	// asks.
	var log bytes.Buffer
	broken := health.Handler(func(context.Context) error { return nil }, func() any { return make(chan int) }, slog.New(slog.NewJSONHandler(&log, nil)))
	recorder = httptest.NewRecorder()
	broken.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/statusz", nil))
	if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != "no status\n" || !strings.Contains(log.String(), "the status could not be written") {
		t.Errorf("a status that cannot be written = %d %q; log: %s", recorder.Code, recorder.Body.String(), log.String())
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
		stopped <- health.Serve(ctx, listener, func(context.Context) error { return nil }, nil, slog.New(slog.DiscardHandler))
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
