package postgrestest

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// A server that does not start is removed and started once more. Docker on
// a busy machine sometimes does not have a container ready in time, and a
// run that failed for that says nothing about the code under test.
func TestAServerThatDoesNotStartIsStartedOnceMore(t *testing.T) {
	slow := errors.New("mapped port: context deadline exceeded")
	down := errors.New("cannot connect to the Docker daemon")
	// Containers are told apart by their address: none of them is real.
	first, second := &tcpostgres.PostgresContainer{}, &tcpostgres.PostgresContainer{}
	type attempt struct {
		container *tcpostgres.PostgresContainer
		err       error
	}
	for _, c := range []struct {
		name     string
		attempts []attempt
		// stopped is whether the run is interrupted after its first start.
		stopped bool
		want    *tcpostgres.PostgresContainer
		err     error
		// started is how many starts there were, and removed which
		// containers were removed, in order.
		started int
		removed []*tcpostgres.PostgresContainer
		said    int
	}{
		{"a server that starts", []attempt{{first, nil}}, false, first, nil, 1, nil, 0},
		{"one that starts the second time", []attempt{{nil, slow}, {second, nil}}, false, second, nil, 2, []*tcpostgres.PostgresContainer{nil}, 1},
		{"one that left its container behind", []attempt{{first, slow}, {second, nil}}, false, second, nil, 2, []*tcpostgres.PostgresContainer{first}, 1},
		{"one that never starts", []attempt{{first, slow}, {second, down}, {nil, nil}}, false, nil, down, 2, []*tcpostgres.PostgresContainer{first, second}, 1},
		{"a run that is interrupted", []attempt{{first, slow}, {second, nil}}, true, nil, slow, 1, []*tcpostgres.PostgresContainer{first}, 0},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		var (
			startedTimes int
			removed      []*tcpostgres.PostgresContainer
			said         bytes.Buffer
		)
		start := func(context.Context) (*tcpostgres.PostgresContainer, error) {
			// The container of a start that failed is gone before the next.
			if startedTimes != len(removed) {
				t.Errorf("%s: start %d began with %d containers removed", c.name, startedTimes+1, len(removed))
			}
			next := c.attempts[startedTimes]
			startedTimes++
			if c.stopped {
				cancel()
			}
			return next.container, next.err
		}
		got, err := started(ctx, start, func(container *tcpostgres.PostgresContainer) { removed = append(removed, container) }, &said)
		cancel()
		if got != c.want || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("%s: started = %p, %v; want %p, %v", c.name, got, err, c.want, c.err)
		}
		if startedTimes != c.started || !slices.Equal(removed, c.removed) {
			t.Errorf("%s: %d starts and %d containers removed, want %d and %d", c.name, startedTimes, len(removed), c.started, len(c.removed))
		}
		// That it happened is said once for each start that is tried again,
		// with its reason.
		if lines := strings.Count(said.String(), "\n"); lines != c.said || (c.said > 0 && !strings.Contains(said.String(), slow.Error())) {
			t.Errorf("%s: %d lines were written, want %d: %q", c.name, lines, c.said, said.String())
		}
	}
	if starts != 2 {
		t.Errorf("a server is started up to %d times, want 2: one more than the first", starts)
	}
}
