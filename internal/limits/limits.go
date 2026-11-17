// Package limits holds the initial limits and quotas of PLAN.md section 9,
// one constant per value. They are code and not configuration: they do not
// differ between installations, so a change to one is a change that is
// reviewed, and nothing can start with a value nobody chose.
package limits

import "time"

// The quotas a workspace is created with. They are the initial values: the
// operator can give a workspace others when creating it.
const (
	// MaxLinks is how many links a workspace holds, deleted ones not
	// counted. It bounds what one workspace adds to the copy of the links
	// that the redirect keeps in memory.
	MaxLinks = 1000
	// MaxLinksDemo is the same quota for a demo workspace, which anyone can
	// enter without a secret: what a stranger can create is kept small.
	MaxLinksDemo = 100
	// MaxKeys is how many keys a workspace holds, revoked ones not counted.
	// It bounds the credentials in force that a workspace has to keep track
	// of.
	MaxKeys = 10
)

// What the server of the API allows a request. A client that is slower or
// sends more than this is cut off, so that it cannot hold a connection or
// the memory of the process.
const (
	// APIReadHeaderTimeout is how long a client has to send the headers of
	// a request.
	APIReadHeaderTimeout = 5 * time.Second
	// APIReadTimeout is how long a client has to send a whole request, body
	// included.
	APIReadTimeout = 10 * time.Second
	// APIWriteTimeout is how long the server has to answer, from the end of
	// the headers of the request. No operation of the API comes near it, and
	// when the process stops it is also how long requests in flight are
	// waited for: none can last longer.
	APIWriteTimeout = 15 * time.Second
	// APIIdleTimeout is how long a connection is kept open between two
	// requests.
	APIIdleTimeout = 60 * time.Second
	// APIMaxBodyBytes is the largest body of a request: 16 kB, far above
	// what any operation takes.
	APIMaxBodyBytes = 16_000
)
