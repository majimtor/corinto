package redirect_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/redirect"
)

// shared is a log that the server writes while the test reads.
type shared struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (s *shared) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffer.Write(p)
}

func (s *shared) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffer.String()
}

// serving starts Serve on a port of the loopback interface and returns its
// address, its log and what stops it and waits for it.
func serving(t *testing.T, handler http.Handler) (string, *shared, func() error) {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	written := &shared{}
	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() {
		ended <- redirect.Serve(ctx, listener, handler, slog.New(slog.NewJSONHandler(written, nil)))
	}()
	// Stopping it twice waits once: a test stops it to read its log, and
	// the end of the test stops it again.
	stop := sync.OnceValue(func() error {
		cancel()
		select {
		case err := <-ended:
			return err
		case <-time.After(10 * time.Second):
			return errors.New("Serve did not stop with its context")
		}
	})
	t.Cleanup(func() { _ = stop() })
	return "http://" + listener.Addr().String(), written, stop
}

// get asks for a path and returns the status. These tests are about the
// server, not about what the host of the short links answers.
func get(t *testing.T, url string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Error(err)
		return 0
	}
	_ = response.Body.Close()
	return response.StatusCode
}

func TestServeAnswersUntilItsContextEndsAndLetsARequestFinish(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	url, _, stop := serving(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	answered := make(chan int, 1)
	go func() { answered <- get(t, url+"/slow") }()
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	// The server is stopping, and the request that was in flight is still
	// let to finish.
	select {
	case err := <-stopped:
		t.Fatalf("Serve returned with a request in flight: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if status := <-answered; status != http.StatusNoContent {
		t.Errorf("the request in flight was answered %d, want 204", status)
	}
	if err := <-stopped; err != nil {
		t.Errorf("Serve returned %v after a clean stop", err)
	}
}

// What the HTTP server itself has to say can name the address of a
// visitor. The log says that it spoke, and not what it said.
func TestWhatTheServerSaysIsLoggedWithoutItsText(t *testing.T) {
	url, written, stop := serving(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		// The server complains about this in its own words.
		w.WriteHeader(http.StatusOK)
	}))
	if status := get(t, url+"/twice"); status != http.StatusNoContent {
		t.Fatalf("GET = %d, want 204", status)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	log := written.String()
	if !strings.Contains(log, "reported a problem") {
		t.Errorf("what the server said did not reach the log: %s", log)
	}
	if strings.Contains(log, "superfluous") || strings.Contains(log, "WriteHeader") {
		t.Errorf("the log carries the text of the server: %s", log)
	}
}
