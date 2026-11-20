//go:build integration

package main

import (
	"context"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
)

// relay stands between the processes of a test and their database, so
// that the test can take the database away and give it back. While it is
// cut nothing listens where the database was and every connection through
// it is closed: what a process sees when its database goes.
type relay struct {
	// target is where the database really is.
	target string

	mu sync.Mutex
	// address is where the relay listens, the same before a cut and after.
	address  string
	listener net.Listener
	open     map[net.Conn]struct{}
}

// newRelay starts a relay to the server of a database URL, and returns it
// with the URL of the same database through the relay.
func newRelay(t *testing.T, databaseURL string) (*relay, string) {
	t.Helper()
	target, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	between := &relay{target: target.Host, address: "127.0.0.1:0", open: make(map[net.Conn]struct{})}
	between.restore(t)
	t.Cleanup(between.cut)
	through := *target
	through.Host = between.address
	return between, through.String()
}

// restore makes the database reachable, at the address it had.
func (r *relay) restore(t *testing.T) {
	t.Helper()
	var config net.ListenConfig
	// The context of the test would end the listener with the test, and the
	// processes of a test stop after it.
	listener, err := config.Listen(context.WithoutCancel(t.Context()), "tcp", r.address)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.listener, r.address = listener, listener.Addr().String()
	r.mu.Unlock()
	go r.accept(listener)
}

// cut takes the database away: nothing listens, and every connection is
// closed.
func (r *relay) cut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listener != nil {
		_ = r.listener.Close()
		r.listener = nil
	}
	for conn := range r.open {
		_ = conn.Close()
	}
	clear(r.open)
}

func (r *relay) accept(listener net.Listener) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		var dialer net.Dialer
		server, err := dialer.DialContext(context.Background(), "tcp", r.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		r.mu.Lock()
		if r.listener != listener {
			// Cut between the two ends.
			r.mu.Unlock()
			_ = client.Close()
			_ = server.Close()
			continue
		}
		r.open[client], r.open[server] = struct{}{}, struct{}{}
		r.mu.Unlock()
		for _, pair := range [][2]net.Conn{{client, server}, {server, client}} {
			go func() {
				_, _ = io.Copy(pair[0], pair[1])
				_ = pair[0].Close()
				_ = pair[1].Close()
			}()
		}
	}
}
