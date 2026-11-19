package ingest

import (
	"encoding/binary"
	"regexp"
	"strings"
	"testing"
	"time"
	"uuid"
)

// The identifier of an event says its second and nothing finer: the time
// of an access is kept to the second (PLAN.md section 17), and an
// identifier that carried the millisecond would keep more.
func TestTheIdentifierOfAnEventSaysItsSecondAndNoMore(t *testing.T) {
	at := time.Date(2026, time.November, 19, 12, 30, 45, 987_654_321, time.UTC)
	seen := make(map[string]bool)
	for range 1000 {
		id := newID(at)
		parsed, err := uuid.Parse(id)
		if err != nil {
			t.Fatalf("%q is not a UUID: %v", id, err)
		}
		if version, variant := parsed[6]>>4, parsed[8]>>6; version != 7 || variant != 0b10 {
			t.Fatalf("%s is of version %d and variant %b, want 7 and 10", id, version, variant)
		}
		milliseconds := binary.BigEndian.Uint64(append([]byte{0, 0}, parsed[:6]...))
		if want := uint64(at.Unix()) * 1000; milliseconds != want { //nolint:gosec // A time after 1970.
			t.Fatalf("%s carries the time %d, want %d: the second, with no milliseconds", id, milliseconds, want)
		}
		seen[id] = true
	}
	if len(seen) != 1000 {
		t.Errorf("a thousand identifiers of one second are %d different ones", len(seen))
	}
	// The identifiers of a second come before the ones of the next: they
	// are next to each other in an index.
	if earlier, later := newID(at), newID(at.Add(time.Second)); earlier >= later {
		t.Errorf("%s, of a second, does not come before %s, of the next", earlier, later)
	}
}

func TestWhatAnEventKeepsOfAReferrer(t *testing.T) {
	label := strings.Repeat("a", 63)
	longest := label + "." + label + "." + label + "." + strings.Repeat("a", 57) + ".com"
	cases := []struct {
		name    string
		referer string
		want    string
	}{
		{"a page", "https://example.com/", "example.com"},
		{"a site without a path", "https://example.com", "example.com"},
		{"a page with a path, a query and a fragment", "http://example.com/a/b?c=d#e", "example.com"},
		{"a query right after the host", "https://example.com?c=d", "example.com"},
		{"a fragment right after the host", "https://example.com#e", "example.com"},
		{"capital letters", "HTTPS://News.Example.COM/Path", "news.example.com"},
		{"a port", "https://example.com:8443/x", "example.com"},
		{"an empty port", "https://example.com:/x", "example.com"},
		{"credentials", "https://user:secret@example.com/x", "example.com"},
		{"credentials with an at sign", "https://user@home:secret@example.com/x", "example.com"},
		{"a name that ends in a dot", "https://example.com./", "example.com"},
		{"a name outside ASCII in the form the header carries", "https://xn--bcher-kva.example/", "xn--bcher-kva.example"},
		{"a name of one label", "http://intranet/", "intranet"},
		{"a short name", "https://t.co/abc", "t.co"},
		{"a label of 63 bytes and a name of 253", "https://" + longest + "/", longest},

		{"no header", "", ""},
		{"no scheme", "example.com/page", ""},
		{"an application", "android-app://com.example.reader/", ""},
		{"another scheme", "ftp://example.com/", ""},
		{"a scheme without an address", "about:blank", ""},
		{"an IPv4 address", "https://192.0.2.7/x", ""},
		{"an IPv4 address with a port", "http://192.0.2.7:8080/", ""},
		{"an IPv6 address", "https://[2001:db8::1]/x", ""},
		{"an IPv6 address with a port", "https://[2001:db8::1]:8080/x", ""},
		{"an IPv4 address as one number", "https://3221225991/", ""},
		{"an IPv4 address in hexadecimal", "https://0xc0.0.2.7/", ""},
		{"an IPv4 address cut short", "https://192.7/", ""},
		{"a name outside ASCII", "https://bücher.example/", ""},
		{"a letter that only looks like one of ASCII", "https://Kelvin.example/", ""},
		{"an underscore", "https://exa_mple.com/", ""},
		{"a space", "https://exa mple.com/", ""},
		{"an escape", "https://example.com%2f.other.example/", ""},
		{"a label that begins with a hyphen", "https://-example.com/", ""},
		{"a label that ends with a hyphen", "https://example-.com/", ""},
		{"an empty label", "https://example..com/", ""},
		{"no host", "https:///path", ""},
		{"nothing after the scheme", "https://", ""},
		{"a port that is not a number", "https://example.com:port/", ""},
		{"a label of 64 bytes", "https://" + label + "a.com/", ""},
		{"a name of 254 bytes", "https://" + label + "." + label + "." + label + "." + strings.Repeat("a", 58) + ".com/", ""},
		{"more before the host than is read", "https://" + strings.Repeat("a", maxAuthority) + "@example.com/", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReferrerHost(c.referer); got != c.want {
				t.Errorf("ReferrerHost(%q) = %q, want %q", c.referer, got, c.want)
			}
		})
	}
	if len(longest) != maxHost {
		t.Fatalf("the longest name of the test has %d bytes, want %d", len(longest), maxHost)
	}
}

// Whatever the header carries, what is kept is nothing, or a name of host
// in lower case that the header had.
func FuzzReferrerHost(f *testing.F) {
	f.Add("https://example.com/a?b#c")
	f.Add("HTTP://user:secret@Example.COM:8080")
	f.Add("https://[2001:db8::1]:443/")
	f.Add("https://192.0.2.7/")
	f.Add("android-app://com.example.reader/")
	name := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	f.Fuzz(func(t *testing.T, referer string) {
		host := ReferrerHost(referer)
		if host == "" {
			return
		}
		if !name.MatchString(host) || len(host) > maxHost {
			t.Errorf("ReferrerHost(%q) = %q, which is not a name of host", referer, host)
		}
		if last := host[strings.LastIndexByte(host, '.')+1:]; last[0] < 'a' || last[0] > 'z' {
			t.Errorf("ReferrerHost(%q) = %q, whose last label could be of an address", referer, host)
		}
		if !strings.Contains(lowerASCII(referer), host) {
			t.Errorf("ReferrerHost(%q) = %q, which the header does not have", referer, host)
		}
	})
}
