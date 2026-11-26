// Package limits holds the initial limits and quotas of PLAN.md section 9,
// one constant per value, and what applies the ones that are counted in
// time: the limiter, and the address of the client it counts by. The values
// are code and not configuration: they do not differ between installations,
// so a change to one is a change that is reviewed, and nothing can start
// with a value nobody chose.
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

// What the API allows in a minute. Past it the answer is 429 until there is
// room again.
const (
	// APIRequestsPerMinute is how many requests one key makes in a minute:
	// two a second, sustained, which a script that manages links does not
	// reach and a loop out of control does.
	APIRequestsPerMinute = 120
	// APIWritesPerMinute is how many of those requests may change
	// something. Every change reaches the copy of the links that the
	// redirect keeps in memory.
	APIWritesPerMinute = 60
	// APIFailedAuthenticationsPerMinute is how many requests without a
	// valid key one client address makes before the address is refused,
	// with a valid key too. A secret has 256 bits, so this is not what
	// keeps a key from being guessed: it keeps a stranger from making the
	// database answer for every guess.
	APIFailedAuthenticationsPerMinute = 10
)

// What the panel allows one client address.
const (
	// DemoEntriesPerMinute is how many sessions one client address opens
	// in a minute through the demo entry, which asks for no secret: a
	// person enters once or twice, and whatever enters faster is filling
	// the table of the sessions.
	DemoEntriesPerMinute = 10
)

// What the host of the short links allows a request. A request to it is a
// line and a few headers, and its answer is smaller still: whatever takes
// longer or sends more is not a visitor following a link.
const (
	// RedirectReadHeaderTimeout is how long a client has to send the
	// headers of a request.
	RedirectReadHeaderTimeout = 5 * time.Second
	// RedirectReadTimeout is how long a client has to send a whole request.
	// No request to this host has a body worth waiting for.
	RedirectReadTimeout = 5 * time.Second
	// RedirectWriteTimeout is how long the server has to answer. It is also
	// how long requests in flight are waited for when the process stops.
	RedirectWriteTimeout = 5 * time.Second
	// RedirectIdleTimeout is how long a connection is kept open between two
	// requests.
	RedirectIdleTimeout = 30 * time.Second
	// RedirectMaxHeaderBytes is the most a request line and its headers
	// take. A browser sends a few hundred bytes; a path of a megabyte is
	// not a short link.
	RedirectMaxHeaderBytes = 16 << 10
)

// What the host of the short links allows one client address. A person
// follows a link now and then; a page of links opened at once is the burst.
// Whatever asks faster for longer is trying identifiers, and is told to
// wait.
const (
	// RedirectRequestsPerSecond is how many requests one client address
	// makes in a second, sustained.
	RedirectRequestsPerSecond = 20
	// RedirectBurst is how many requests one client address makes at once
	// after being idle.
	RedirectBurst = 40
)
