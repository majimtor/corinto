package link_test

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/majimtor/corinto/internal/link"
)

func TestParseDestinationNormalisesOnlyWhatKeepsTheMeaning(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"already normal", "https://example.com/a?b=c#d", "https://example.com/a?b=c#d"},
		{"the case of the scheme and of the host", "HTTPS://Example.COM/Path", "https://example.com/Path"},
		{"an empty path", "https://example.com", "https://example.com/"},
		{"an empty path before a query", "https://example.com?b=c", "https://example.com/?b=c"},
		{"an empty path before a fragment", "https://example.com#d", "https://example.com/#d"},
		{"the default port of https", "https://example.com:443/a", "https://example.com/a"},
		{"the default port of http", "http://example.com:80/a", "http://example.com/a"},
		{"another port is kept", "https://example.com:8443/a", "https://example.com:8443/a"},
		{"the port of the other scheme is kept", "http://example.com:443/a", "http://example.com:443/a"},
		{"a port written with zeros", "http://example.com:0080/a", "http://example.com/a"},
		{"a colon with no port", "https://example.com:/a", "https://example.com/a"},
		{"a punycode host", "https://xn--bcher-kva.example/a", "https://xn--bcher-kva.example/a"},
		{"escapes in the path are kept as written", "https://example.com/a%2Fb%2fc", "https://example.com/a%2Fb%2fc"},
		{"the query is kept byte for byte", "https://example.com/?q=a+b&x=%7B1%7D", "https://example.com/?q=a+b&x=%7B1%7D"},
		{"a dot inside a segment is not a dot segment", "https://example.com/v1.2/.hidden/a..b", "https://example.com/v1.2/.hidden/a..b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := link.ParseDestination(c.raw, link.Policy{})
			if err != nil {
				t.Fatalf("ParseDestination(%q) error = %v", c.raw, err)
			}
			if got != c.want {
				t.Errorf("ParseDestination(%q) = %q, want %q", c.raw, got, c.want)
			}
			again, err := link.ParseDestination(got, link.Policy{})
			if err != nil || again != got {
				t.Errorf("the normal form %q is not stable: %q, %v", got, again, err)
			}
		})
	}
}

func TestParseDestinationRefusesWhatTwoParsersReadDifferently(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"empty", "", link.ErrDestinationScheme},
		{"longer than the limit", "https://example.com/" + strings.Repeat("a", 2048), link.ErrDestinationTooLong},

		{"a backslash, which a browser reads as a slash", `https://example.com\@evil.example/`, link.ErrDestinationCharacters},
		{"a space", "https://example.com/a b", link.ErrDestinationCharacters},
		{"a tab, which a browser removes", "https://exa\tmple.com/", link.ErrDestinationCharacters},
		{"a line break", "https://example.com/\n", link.ErrDestinationCharacters},
		{"a control character", "https://example.com/\x00", link.ErrDestinationCharacters},
		{"a character outside ASCII in the path", "https://example.com/caf\u00e9", link.ErrDestinationCharacters},
		{"a host outside ASCII", "https://b\u00fccher.example/", link.ErrDestinationCharacters},
		{"a percent sign that starts no escape", "https://example.com/100%", link.ErrDestinationCharacters},
		{"an escape with a wrong digit", "https://example.com/%zz", link.ErrDestinationCharacters},

		{"no scheme", "example.com/a", link.ErrDestinationScheme},
		{"another scheme", "ftp://example.com/a", link.ErrDestinationScheme},
		{"a script", "javascript://example.com/%0Aalert(1)", link.ErrDestinationScheme},
		{"a scheme without slashes, which a browser accepts", "http:example.com", link.ErrDestinationScheme},
		{"a scheme with one slash, which a browser accepts", "http:/example.com", link.ErrDestinationScheme},
		{"relative to the scheme", "//example.com/a", link.ErrDestinationScheme},

		{"a user", "https://user@example.com/", link.ErrDestinationCredentials},
		{"a user and a password", "https://user:secret@example.com/", link.ErrDestinationCredentials},
		{"a host disguised as a user", "https://example.com@evil.example/", link.ErrDestinationCredentials},

		{"no host", "https:///a", link.ErrDestinationHost},
		{"localhost", "http://localhost/a", link.ErrDestinationHost},
		{"a single label", "http://intranet/a", link.ErrDestinationHost},
		{"an IPv4 address", "http://192.168.1.1/a", link.ErrDestinationHost},
		{"an IPv4 address in one number", "http://example.3232235777/a", link.ErrDestinationHost},
		{"a hexadecimal last label", "http://example.0x7f/a", link.ErrDestinationHost},
		{"an IPv6 address", "http://[::1]/a", link.ErrDestinationHost},
		{"an IPv6 address with a port", "http://[::1]:8080/a", link.ErrDestinationHost},
		{"a trailing dot", "https://example.com./a", link.ErrDestinationHost},
		{"a doubled dot", "https://example..com/a", link.ErrDestinationHost},
		{"a label that starts with a hyphen", "https://-example.com/a", link.ErrDestinationHost},
		{"an underscore in the host", "https://exa_mple.com/a", link.ErrDestinationHost},
		{"an escape in the host", "https://exa%6dple.com/a", link.ErrDestinationHost},
		{"a label of 64 characters", "https://" + strings.Repeat("a", 64) + ".com/a", link.ErrDestinationHost},
		{"a name under localhost", "http://app.localhost/a", link.ErrDestinationHost},
		{"a name of a local network", "http://printer.local/a", link.ErrDestinationHost},
		{"a name marked internal", "http://wiki.corp.internal/a", link.ErrDestinationHost},
		{"a name of a home network", "http://router.home.arpa/a", link.ErrDestinationHost},

		{"a port that is not a number", "https://example.com:https/a", link.ErrDestinationPort},
		{"port zero", "https://example.com:0/a", link.ErrDestinationPort},
		{"a port above the range", "https://example.com:65536/a", link.ErrDestinationPort},
		{"a signed port", "https://example.com:+80/a", link.ErrDestinationPort},

		{"a segment that climbs", "https://example.com/docs/../admin", link.ErrDestinationDotSegment},
		{"a segment that stays", "https://example.com/./a", link.ErrDestinationDotSegment},
		{"a climbing segment at the end", "https://example.com/docs/..", link.ErrDestinationDotSegment},
		{"a climbing segment with escapes", "https://example.com/docs/%2e%2E/admin", link.ErrDestinationDotSegment},
		{"a climbing segment half escaped", "https://example.com/docs/.%2e/admin", link.ErrDestinationDotSegment},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := link.ParseDestination(c.raw, link.Policy{})
			if !errors.Is(err, c.want) {
				t.Fatalf("ParseDestination(%q) = %q, %v; want %v", c.raw, got, err, c.want)
			}
			if got != "" {
				t.Errorf("ParseDestination(%q) = %q with an error, want nothing", c.raw, got)
			}
		})
	}
}

func TestADestinationCannotBeTheRedirectDomain(t *testing.T) {
	policy := link.Policy{RedirectHost: "go.example.dev"}
	for _, raw := range []string{"https://go.example.dev/k7m2xq9d", "http://GO.example.DEV:8080/"} {
		if _, err := link.ParseDestination(raw, policy); !errors.Is(err, link.ErrDestinationRedirects) {
			t.Errorf("ParseDestination(%q) error = %v, want %v", raw, err, link.ErrDestinationRedirects)
		}
	}
	// Another name under the same domain is another host.
	if _, err := link.ParseDestination("https://www.example.dev/", policy); err != nil {
		t.Errorf("a different host is refused: %v", err)
	}
}

func TestTheAllowlistMatchesSchemeHostAndWholeSegments(t *testing.T) {
	policy := link.Policy{Allowed: []link.AllowedPrefix{
		{Scheme: "https", Host: "example.com", Path: "/docs"},
		{Scheme: "https", Host: "shop.example.com:8443", Path: "/"},
		{Scheme: "https", Host: "Blog.Example.com", Path: "/2026/"},
	}}
	cases := []struct {
		name    string
		raw     string
		allowed bool
	}{
		{"the prefix itself", "https://example.com/docs", true},
		{"under the prefix", "https://example.com/docs/guide?x=1", true},
		{"a longer name that starts the same", "https://example.com/docs-other", false},
		{"another path", "https://example.com/admin", false},
		{"the root, with a prefix below it", "https://example.com/", false},
		{"another scheme", "http://example.com/docs", false},
		{"another host", "https://www.example.com/docs", false},
		{"another port is another service", "https://example.com:8443/docs", false},
		{"an entry with a port", "https://shop.example.com:8443/cart", true},
		{"the same host without that port", "https://shop.example.com/cart", false},
		{"an entry written in upper case", "https://blog.example.com/2026/november", true},
		{"a prefix that ends in a slash needs it", "https://blog.example.com/2026", false},
		{"a climb out of the prefix never gets this far", "https://example.com/docs/../admin", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := link.ParseDestination(c.raw, policy)
			if c.allowed && err != nil {
				t.Errorf("ParseDestination(%q) error = %v, want it allowed", c.raw, err)
			}
			if !c.allowed && err == nil {
				t.Errorf("ParseDestination(%q) is allowed, want it refused", c.raw)
			}
		})
	}
}

// urlTestCase is one case of the URL test data of the web platform: what a
// browser makes of an input.
type urlTestCase struct {
	Input    string `json:"input"`
	Failure  bool   `json:"failure"`
	Protocol string `json:"protocol"`
	Hostname string `json:"hostname"`
	Port     string `json:"port"`
}

// Whatever this package accepts, a browser must be able to open, and the
// scheme, host and port that Go reads in the stored form must be the ones
// the browser reads in the input. The data is a selection of the web
// platform's own (testdata/urltestdata.NOTICE.txt).
func TestAnAcceptedDestinationIsReadTheSameByABrowser(t *testing.T) {
	data, err := os.ReadFile("testdata/urltestdata.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}

	cases, accepted := 0, 0
	for _, entry := range entries {
		var c urlTestCase
		if err := json.Unmarshal(entry, &c); err != nil {
			t.Fatal(err)
		}
		cases++
		destination, err := link.ParseDestination(c.Input, link.Policy{})
		if err != nil {
			continue // refusing is always safe
		}
		accepted++
		if c.Failure {
			t.Errorf("%q is accepted as %q, and a browser cannot open it", c.Input, destination)
			continue
		}
		seen, err := url.Parse(destination)
		if err != nil {
			t.Errorf("%q is accepted as %q, which Go cannot read: %v", c.Input, destination, err)
			continue
		}
		if seen.Scheme+":" != c.Protocol || seen.Hostname() != c.Hostname || seen.Port() != c.Port {
			t.Errorf("%q is stored as %q: Go reads %s //%s port %q, a browser reads %s //%s port %q",
				c.Input, destination, seen.Scheme, seen.Hostname(), seen.Port(), c.Protocol, c.Hostname, c.Port)
		}
	}
	t.Logf("%d cases, %d accepted", cases, accepted)
	// The data must still be exercising something: a format change that
	// made every case fail to load would otherwise pass.
	if cases < 300 || accepted < 30 {
		t.Errorf("%d cases and %d accepted: the data is not being read as expected", cases, accepted)
	}
}

// Whatever comes in, what is accepted is stable, bounded and free of
// everything the rules refuse.
func FuzzParseDestination(f *testing.F) {
	for _, seed := range []string{
		"https://example.com/a?b=c#d", "HTTP://Example.com:80", "https://example.com/docs/../admin",
		`https://example.com\@evil.example/`, "http://[::1]/", "https://user@example.com/",
		"https://example.com:0080/%2e%2E/", "http:example.com", "https://example.0x7f/", "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		destination, err := link.ParseDestination(raw, link.Policy{})
		if err != nil {
			if destination != "" {
				t.Fatalf("ParseDestination(%q) returns %q with an error", raw, destination)
			}
			return
		}
		if again, err := link.ParseDestination(destination, link.Policy{}); err != nil || again != destination {
			t.Fatalf("the normal form of %q, %q, is not stable: %q, %v", raw, destination, again, err)
		}
		if len(destination) > 2048+1 {
			// One more than the input at most: the slash of an empty path.
			t.Fatalf("%q grew to %d characters", raw, len(destination))
		}
		seen, err := url.Parse(destination)
		if err != nil {
			t.Fatalf("Go cannot read the accepted %q: %v", destination, err)
		}
		if seen.User != nil || (seen.Scheme != "http" && seen.Scheme != "https") {
			t.Fatalf("%q has credentials or another scheme", destination)
		}
		host := seen.Hostname()
		if host != strings.ToLower(host) || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
			t.Fatalf("%q has a host that is not a lower-case domain name: %q", destination, host)
		}
		for _, c := range []byte(destination) {
			if c <= ' ' || c > '~' || c == '\\' {
				t.Fatalf("%q has a character outside printable ASCII", destination)
			}
		}
		for segment := range strings.SplitSeq(seen.EscapedPath(), "/") {
			switch strings.ReplaceAll(strings.ToLower(segment), "%2e", ".") {
			case ".", "..":
				t.Fatalf("%q has a dot segment", destination)
			}
		}
	})
}
