// Package ingest takes an access event from the response that owes it to
// the inbox table, where it is durable: a bounded buffer in memory, the
// batches that empty it, and the counts that say what was lost on the way
// (docs/delivery-semantics.md).
package ingest

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"
	"uuid"

	"github.com/majimtor/corinto/internal/classify"
)

// Outcome is what the response that owes an event was.
type Outcome string

// The outcomes. No other response owes an event (contract/redirect.md).
const (
	// OutcomeRedirected is a redirect to the destination of an active link.
	OutcomeRedirected Outcome = "redirected"
	// OutcomeInactive is the 404 of a link that is disabled, expired or
	// blocked.
	OutcomeInactive Outcome = "inactive"
)

// Event is one access to a short link. It carries what PLAN.md section 5
// lists and nothing that identifies a visitor: no address, no user agent,
// no more of the referrer than its host
// (docs/adr/0008-access-event-model-and-data-minimisation.md).
type Event struct {
	// ID is given by the buffer when it takes the event.
	ID          string
	LinkID      string
	WorkspaceID string
	// OccurredAt is the time of the access by the clock of the process that
	// served it. It is kept to the second.
	OccurredAt time.Time
	Outcome    Outcome
	// Method is GET or HEAD.
	Method string
	// ReferrerHost is what ReferrerHost made of the header, or nothing.
	ReferrerHost   string
	Classification classify.Classification
}

// newID returns the identifier of an event that occurred at a time: a UUID
// of version 7 whose time is cut to the second. The identifiers of one
// second are next to each other in an index, and none says more of the
// time of an access than the event does. RFC 9562 lets an implementation
// alter the time of a UUID (section 6.1); the other 74 bits are random.
func newID(at time.Time) string {
	var id uuid.UUID
	// Read fills the whole of it or stops the program: it returns no error.
	_, _ = rand.Read(id[:])
	var milliseconds [8]byte
	binary.BigEndian.PutUint64(milliseconds[:], uint64(at.Unix())*1000) //nolint:gosec // The time of an access is after 1970.
	copy(id[:6], milliseconds[2:])
	id[6] = id[6]&0x0f | 0x70 // Version 7.
	id[8] = id[8]&0x3f | 0x80 // The variant of RFC 9562.
	return id.String()
}

const (
	// maxHost is the longest name of a host, in bytes.
	maxHost = 253
	// maxLabel is the longest label of that name.
	maxLabel = 63
	// maxAuthority is how much of a referrer is read after its scheme. The
	// host is at its beginning; the rest of the address is never looked at.
	maxAuthority = 512
)

// ReferrerHost returns what an event keeps of the Referer header of a
// request: the host, in lower case and without a port, when the header is
// an http or https address whose host is a domain name. Anything else
// gives nothing: an address of another scheme, a host that is an IP
// address, a name outside the grammar of host names. The path, the query
// and the credentials of the address are never kept.
func ReferrerHost(referer string) string {
	rest, found := "", false
	for _, scheme := range []string{"https://", "http://"} {
		if len(referer) >= len(scheme) && strings.EqualFold(referer[:len(scheme)], scheme) {
			rest, found = referer[len(scheme):], true
			break
		}
	}
	if !found {
		return ""
	}
	// The authority ends at the path, the query or the fragment.
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
	}
	if len(authority) > maxAuthority {
		return ""
	}
	// What is before the last at sign are credentials.
	host := authority[strings.LastIndexByte(authority, '@')+1:]
	// What is after a colon is a port. An IPv6 address has colons of its
	// own and brackets around it, which no name has.
	if colon := strings.IndexByte(host, ':'); colon >= 0 {
		port := host[colon+1:]
		if strings.Trim(port, "0123456789") != "" {
			return ""
		}
		host = host[:colon]
	}
	host = strings.TrimSuffix(lowerASCII(host), ".")
	if !isDomainName(host) {
		return ""
	}
	return host
}

// lowerASCII lowers the letters A to Z and nothing else: a name outside
// ASCII is not kept, and must not become one that is.
func lowerASCII(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' }) < 0 {
		return s
	}
	lowered := []byte(s)
	for i, c := range lowered {
		if c >= 'A' && c <= 'Z' {
			lowered[i] = c + ('a' - 'A')
		}
	}
	return string(lowered)
}

// isDomainName reports whether a host, already in lower case, is a name
// and not an address: labels of letters, digits and hyphens between dots,
// the last of which begins with a letter. Every top-level domain does, and
// no IPv4 address in any of its spellings.
func isDomainName(host string) bool {
	if host == "" || len(host) > maxHost {
		return false
	}
	last := host
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > maxLabel || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			if c := label[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
		last = label
	}
	return last[0] >= 'a' && last[0] <= 'z'
}
