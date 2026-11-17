package link

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// maxDestinationLength bounds a destination as it is stored and as it is
// served, with the campaign parameters appended: it is the value that
// travels in the Location header.
const maxDestinationLength = 2048

// Why a destination is refused. The API maps each one to a code of the
// contract.
var (
	ErrDestinationTooLong     = errors.New("a destination has at most 2,048 characters")
	ErrDestinationCharacters  = errors.New("a destination is written in printable ASCII, already percent-encoded and without backslashes")
	ErrDestinationScheme      = errors.New("a destination starts with http:// or https://")
	ErrDestinationCredentials = errors.New("a destination carries no credentials")
	ErrDestinationHost        = errors.New("the host of a destination is a public domain name")
	ErrDestinationPort        = errors.New("the port of a destination is a number between 1 and 65535")
	ErrDestinationDotSegment  = errors.New("the path of a destination has no . or .. segments")
	ErrDestinationRedirects   = errors.New("a destination cannot be the redirect domain")
	ErrDestinationNotAllowed  = errors.New("the destination matches no entry of the allowlist")
)

// specialUse are the suffixes reserved for names that only mean something
// inside a machine or a private network. The plan refuses localhost so that
// a link cannot point into the network of whoever follows it; these are the
// other names that say so on their face. A public name can still resolve to
// a private address, and no rule over the name prevents that.
var specialUse = []string{".localhost", ".local", ".internal", ".home.arpa"}

// Policy is what a workspace adds to the rules every destination meets.
type Policy struct {
	// RedirectHost is the host that serves short links. A destination that
	// pointed at it would lead to another link, or to itself.
	RedirectHost string
	// Allowed lists what a destination must match. Empty means anything.
	Allowed []AllowedPrefix
}

// AllowedPrefix is one entry of an allowlist. Host is compared with the
// host of the destination and its port, when it is not the default one of
// the scheme: another port is another service. Path is a prefix matched by
// whole segments, so that /docs allows /docs/guide and not /docs-other.
type AllowedPrefix struct {
	Scheme string
	Host   string
	Path   string
}

// ParseDestination validates where a link leads and returns it in the form
// in which it is stored and served.
//
// Go reads URLs as RFC 3986 says and browsers as the WHATWG URL Standard
// says. Everything the two can read differently is refused instead of
// repaired, so that the host this server sees is the host a browser opens.
// What is normalised is only what cannot change the meaning: the case of the
// scheme and of the host, a default or empty port, and an empty path. Path,
// query and fragment are kept byte for byte.
func ParseDestination(raw string, policy Policy) (string, error) {
	if len(raw) > maxDestinationLength {
		return "", ErrDestinationTooLong
	}
	if !printableASCII(raw) || !wellEscaped(raw) {
		return "", ErrDestinationCharacters
	}

	// A browser also accepts http:example.com and http:/example.com; Go sees
	// no host in them. Only the full form is taken.
	scheme, rest, found := strings.Cut(raw, "://")
	scheme = strings.ToLower(scheme)
	if !found || (scheme != "http" && scheme != "https") {
		return "", ErrDestinationScheme
	}

	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	if strings.Contains(authority, "@") {
		return "", ErrDestinationCredentials
	}
	host, port, err := splitAuthority(authority, scheme)
	if err != nil {
		return "", err
	}
	if host == strings.ToLower(policy.RedirectHost) {
		return "", ErrDestinationRedirects
	}

	path, suffix := tail, ""
	if i := strings.IndexAny(tail, "?#"); i >= 0 {
		path, suffix = tail[:i], tail[i:]
	}
	if path == "" {
		path = "/"
	}
	if hasDotSegment(path) {
		return "", ErrDestinationDotSegment
	}

	hostPort := host
	if port != "" {
		hostPort += ":" + port
	}
	destination := scheme + "://" + hostPort + path + suffix

	// The last word is Go's own parser: what it reads must be what was
	// written above.
	parsed, err := url.Parse(destination)
	if err != nil || parsed.Scheme != scheme || parsed.Host != hostPort || parsed.User != nil || parsed.Opaque != "" {
		return "", ErrDestinationCharacters
	}

	if !allowed(policy.Allowed, scheme, hostPort, path) {
		return "", ErrDestinationNotAllowed
	}
	return destination, nil
}

// printableASCII reports whether s has no control character, no space, no
// byte outside ASCII and no backslash. A browser drops or changes all of
// them, and a header cannot carry most of them.
func printableASCII(s string) bool {
	for i := range len(s) {
		if c := s[i]; c <= ' ' || c > '~' || c == '\\' {
			return false
		}
	}
	return true
}

// wellEscaped reports whether every percent sign starts an escape of two
// hexadecimal digits.
func wellEscaped(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return false
		}
		i += 2
	}
	return true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// splitAuthority returns the host in lower case and the port, empty when it
// is absent or the default one of the scheme.
func splitAuthority(authority, scheme string) (host, port string, err error) {
	host = authority
	if i := strings.LastIndexByte(authority, ':'); i >= 0 && !strings.ContainsAny(authority, "[]") {
		host, port = authority[:i], authority[i+1:]
	}
	host = strings.ToLower(host)
	if !publicDomainName(host) {
		return "", "", ErrDestinationHost
	}
	if port == "" {
		// No port, or a colon with nothing after it, which a browser ignores.
		return host, "", nil
	}
	for i := range len(port) {
		if port[i] < '0' || port[i] > '9' {
			return "", "", ErrDestinationPort
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", "", ErrDestinationPort
	}
	if (scheme == "http" && number == 80) || (scheme == "https" && number == 443) {
		return host, "", nil
	}
	// Written again from the number, so that 0080 and 80 are one port.
	return host, strconv.Itoa(number), nil
}

// publicDomainName reports whether host, already in lower case, is a domain
// name of at least two labels that no parser takes for an address.
func publicDomainName(host string) bool {
	if len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		// A single label: localhost, or a name of the visitor's own network.
		return false
	}
	for _, label := range labels {
		// An empty label is a doubled or trailing dot. A browser keeps the
		// trailing one and Go's resolver does not treat it the same way.
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	// A browser reads a host whose last label is a number, decimal or
	// hexadecimal, as an IPv4 address; Go reads it as a name.
	last := labels[len(labels)-1]
	if strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		return false
	}
	for _, suffix := range specialUse {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	return true
}

// hasDotSegment reports whether a path has a segment that a browser would
// resolve: . or .., written plainly or with %2e. A browser removes them
// before it opens the page and Go does not, so /docs/../admin would pass an
// allowlist for /docs and open /admin.
func hasDotSegment(path string) bool {
	for segment := range strings.SplitSeq(path, "/") {
		switch strings.ReplaceAll(strings.ToLower(segment), "%2e", ".") {
		case ".", "..":
			return true
		}
	}
	return false
}

// allowed reports whether a destination matches an entry of the allowlist.
// An empty list allows everything.
func allowed(entries []AllowedPrefix, scheme, hostPort, path string) bool {
	if len(entries) == 0 {
		return true
	}
	for _, entry := range entries {
		if strings.ToLower(entry.Scheme) != scheme || strings.ToLower(entry.Host) != hostPort {
			continue
		}
		if pathWithin(path, entry.Path) {
			return true
		}
	}
	return false
}

// pathWithin reports whether path is prefix itself or lies under it, by
// whole segments.
func pathWithin(path, prefix string) bool {
	if prefix == "" || prefix == "/" {
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || strings.HasSuffix(prefix, "/") || path[len(prefix)] == '/'
}
