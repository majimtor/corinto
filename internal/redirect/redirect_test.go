package redirect_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/redirect"
	"github.com/majimtor/corinto/internal/resolve"
)

// The moment these tests happen at.
var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// links is what the copy of these tests reads: a fixed list.
type links []link.Served

func (l links) Served(context.Context) ([]link.Served, time.Time, error) { return l, now, nil }

func (l links) ServedSince(context.Context, time.Time) ([]link.Served, time.Time, error) {
	return l, now, nil
}

// answer is a response, read to its end.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// host starts the host of the short links over a list of links and returns
// its address. Its clock stands still, so a test has the forty requests a
// client address makes at once and no more.
func host(t *testing.T, served ...link.Served) string {
	t.Helper()
	return hostAt(t, func() time.Time { return now }, "", served...)
}

// hostAt starts the host with a clock and a list of trusted proxies.
func hostAt(t *testing.T, clock func() time.Time, proxies string, served ...link.Served) string {
	t.Helper()
	address, _ := hostThatRecords(t, clock, proxies, served...)
	return address
}

// hostThatRecords starts the host and returns its address and what its
// responses leave behind.
func hostThatRecords(t *testing.T, clock func() time.Time, proxies string, served ...link.Served) (string, *left) {
	t.Helper()
	behind := &left{counts: make(map[ingest.Counter][]time.Time)}
	server := httptest.NewServer(handlerOver(t, clock, proxies, behind, io.Discard, served...))
	t.Cleanup(server.Close)
	return server.URL, behind
}

// handlerOver returns the handler of the host over a list of links, with
// what takes its events and where its log goes.
func handlerOver(t *testing.T, clock func() time.Time, proxies string, events redirect.Recorder, log io.Writer, served ...link.Served) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(log, nil))
	copied := resolve.New(links(served), logger)
	if err := copied.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	trusted, err := limits.ParseProxies(proxies)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := redirect.New(t.Context(), redirect.Settings{
		Links: copied, Logger: logger, Now: clock, TrustedProxies: trusted, Rules: rules(t), Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// rules returns the rules as the binary embeds them, read once.
func rules(t *testing.T) *classify.Classifier {
	t.Helper()
	read, err := readRules()
	if err != nil {
		t.Fatal(err)
	}
	return read
}

var readRules = sync.OnceValues(classify.New)

// left is what the responses of a test left behind: the events, in the
// order in which they were handed over, and the moments of each count.
type left struct {
	mu     sync.Mutex
	events []ingest.Event
	counts map[ingest.Counter][]time.Time
}

func (l *left) Record(event ingest.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *left) Count(name ingest.Counter, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[name] = append(l.counts[name], at)
}

// behind returns the events and how many times each counter was counted.
func (l *left) behind() ([]ingest.Event, map[ingest.Counter]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	counts := make(map[ingest.Counter]int)
	for name, moments := range l.counts {
		counts[name] = len(moments)
	}
	return slices.Clone(l.events), counts
}

// The links of these tests. Each identifier says what its link is.
func catalogue() []link.Served {
	soon, past := now.Add(time.Second), now.Add(-time.Second)
	return []link.Served{
		{ID: "1", WorkspaceID: "acme", Identifier: "k7m2xq9d", State: link.StateActive, Resolved: "https://example.com/landing?ref=home&utm_source=newsletter#top", Version: 1},
		{ID: "2", WorkspaceID: "globex", Identifier: "spring-sale", State: link.StateActive, Resolved: "https://example.com/sale", Version: 1},
		{ID: "3", WorkspaceID: "acme", Identifier: "expires-soon", State: link.StateActive, Resolved: "https://example.com/soon", ExpiresAt: &soon, Version: 1},
		{ID: "4", WorkspaceID: "acme", Identifier: "expired", State: link.StateActive, Resolved: "https://example.com/expired", ExpiresAt: &past, Version: 1},
		{ID: "5", WorkspaceID: "acme", Identifier: "expires-now", State: link.StateActive, Resolved: "https://example.com/now", ExpiresAt: &now, Version: 1},
		{ID: "6", WorkspaceID: "globex", Identifier: "disabled", State: link.StateDisabled, Resolved: "https://example.com/disabled", Version: 2},
		{ID: "7", WorkspaceID: "acme", Identifier: "blocked", State: link.StateBlocked, Resolved: "https://example.com/blocked", Version: 2},
	}
}

// ask sends a request as it is written and does not follow where it leads.
func ask(t *testing.T, method, url string, headers ...string) answer {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			request.Host = headers[i+1]
			continue
		}
		request.Header.Set(headers[i], headers[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	checkEveryResponse(t, method+" "+request.URL.RequestURI(), response, body)
	return answer{response.StatusCode, response.Header, body}
}

// checkEveryResponse asserts what the contract says of every response.
func checkEveryResponse(t *testing.T, what string, response *http.Response, body []byte) {
	t.Helper()
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control = %q, want no-store", what, got)
	}
	if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", what, got)
	}
	for name := range response.Header {
		if name == "Set-Cookie" || strings.HasPrefix(name, "Access-Control-") {
			t.Errorf("%s: the response carries %s", what, name)
		}
	}
	if len(body) > 0 && !strings.Contains(response.Header.Get("Content-Type"), "; charset=utf-8") {
		t.Errorf("%s: a body is sent as %q, without its character set", what, response.Header.Get("Content-Type"))
	}
}

func TestAnActiveLinkAnswersWithItsDestinationAndNothingElse(t *testing.T) {
	url := host(t, catalogue()...)
	const landing = "https://example.com/landing?ref=home&utm_source=newsletter#top"
	cases := []struct {
		name     string
		method   string
		path     string
		location string
	}{
		{"a link", http.MethodGet, "/k7m2xq9d", landing},
		{"a link, by its headers alone", http.MethodHead, "/k7m2xq9d", landing},
		{"an alias", http.MethodGet, "/spring-sale", "https://example.com/sale"},
		{"an identifier in upper case", http.MethodGet, "/K7M2XQ9D", landing},
		{"an alias in mixed case", http.MethodGet, "/Spring-Sale", "https://example.com/sale"},
		{"a query string, which plays no part", http.MethodGet, "/k7m2xq9d?utm_source=other&next=https://evil.example/", landing},
		{"an empty query string", http.MethodGet, "/k7m2xq9d?", landing},
		{"a letter written as an escape", http.MethodGet, "/k7m2%78q9d", landing},
		{"a link a second before its expiry", http.MethodGet, "/expires-soon", "https://example.com/soon"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ask(t, c.method, url+c.path)
			if got.status != http.StatusFound || got.header.Get("Location") != c.location {
				t.Errorf("%s %s = %d to %q, want 302 to %q", c.method, c.path, got.status, got.header.Get("Location"), c.location)
			}
			if len(got.body) != 0 || got.header.Get("Content-Type") != "" || got.header.Get("Content-Security-Policy") != "" {
				t.Errorf("a redirect carries a body of %d bytes, as %q", len(got.body), got.header.Get("Content-Type"))
			}
		})
	}
}

// A link that does not redirect and a path that names no link get the same
// answer, byte for byte: it does not say whether the link ever existed.
func TestEverythingElseThatNamesALinkIsOneAndTheSame404(t *testing.T) {
	url := host(t, catalogue()...)
	reference := ask(t, http.MethodGet, url+"/disabled")
	if reference.status != http.StatusNotFound {
		t.Fatalf("a disabled link = %d, want 404", reference.status)
	}
	cases := map[string]string{
		"a blocked link":                   "/blocked",
		"an expired link":                  "/expired",
		"a link at the instant it expires": "/expires-now",
		"an identifier nobody has":         "/bcdfghjk",
		"an identifier of two characters":  "/ab",
		"an identifier of 41 characters":   "/" + strings.Repeat("a", 41),
		"a hyphen at the start":            "/-spring",
		"a hyphen at the end":              "/spring-",
		"an underscore":                    "/spring_sale",
		"a dot":                            "/favicon.ico",
		"what a crawler asks first":        "/robots.txt",
		"two segments":                     "/k7m2xq9d/more",
		"a trailing slash":                 "/k7m2xq9d/",
		"an empty segment":                 "//k7m2xq9d",
		"an encoded slash":                 "/k7m2xq9d%2Fmore",
		"an encoded space":                 "/k7m2%20xq9d",
		"a character outside ASCII":        "/caf%C3%A9",
		// The Kelvin sign and the dotted capital I: by the rules of Unicode
		// their lower case is a k and an i, which would make these paths
		// the identifiers of two links that exist.
		"a sign whose lower case is a k":     "/%E2%84%AA7m2xq9d",
		"a letter whose lower case has an i": "/spr%C4%B0ng-sale",
		"the identifier of the row":          "/1",
		"a path under the well-known one":    "/.well-known/other.txt",
		"the destination where a link goes":  "/https://example.com/sale",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			got := ask(t, http.MethodGet, url+path)
			if got.status != http.StatusNotFound || !bytes.Equal(got.body, reference.body) {
				t.Errorf("GET %s = %d with a body of %d bytes, want the 404 of a disabled link", path, got.status, len(got.body))
			}
			for _, header := range []string{"Content-Type", "Content-Security-Policy", "Content-Length", "Cache-Control"} {
				if got.header.Get(header) != reference.header.Get(header) {
					t.Errorf("GET %s: %s = %q, and a disabled link answers %q", path, header, got.header.Get(header), reference.header.Get(header))
				}
			}
			if got.header.Get("Location") != "" {
				t.Errorf("GET %s leads to %q", path, got.header.Get("Location"))
			}
		})
	}
	if head := ask(t, http.MethodHead, url+"/disabled"); head.status != http.StatusNotFound || len(head.body) != 0 ||
		head.header.Get("Content-Security-Policy") != reference.header.Get("Content-Security-Policy") {
		t.Errorf("HEAD of a disabled link = %d with a body of %d bytes", head.status, len(head.body))
	}
}

// The page of the 404 is one self-contained response, in two languages,
// that can run nothing and load nothing.
func TestThe404PageIsSelfContained(t *testing.T) {
	url := host(t, catalogue()...)
	got := ask(t, http.MethodGet, url+"/bcdfghjk")
	page := string(got.body)

	if contentType := got.header.Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Errorf("the page is sent as %q", contentType)
	}
	for _, part := range []string{
		`<html lang="es">`,
		`<p lang="es">Este enlace no está disponible. Comprueba la dirección.</p>`,
		`<p lang="en">This link is not available. Check the address.</p>`,
		`<span lang="es">Enlace no disponible</span>`,
		`<span lang="en">Link not available</span>`,
		`<meta name="robots" content="noindex">`,
	} {
		if !strings.Contains(page, part) {
			t.Errorf("the page does not have %s", part)
		}
	}
	for _, forbidden := range []string{"<script", "<link", "<img", "<iframe", "<form", "<a ", "src=", "href=", "url(", "@import", "http://", "https://", "style="} {
		if strings.Contains(strings.ToLower(page), forbidden) {
			t.Errorf("the page has %q: it must run nothing and load nothing", forbidden)
		}
	}

	// The policy allows the one inline style of the page, by its hash, and
	// nothing else. The hash is computed here from the page as it arrived.
	style := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindAllStringSubmatch(page, -1)
	if len(style) != 1 {
		t.Fatalf("the page has %d inline styles, want 1", len(style))
	}
	hash := sha256.Sum256([]byte(style[0][1]))
	want := "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) +
		"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	if policy := got.header.Get("Content-Security-Policy"); policy != want {
		t.Errorf("Content-Security-Policy = %q\nwant %q", policy, want)
	}
}

func TestAnyOtherMethodIsNotAllowedOnAnyPath(t *testing.T) {
	url := host(t, catalogue()...)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, "PROPFIND"} {
		for _, path := range []string{"/k7m2xq9d", "/disabled", "/bcdfghjk", "/", "/.well-known/security.txt", "/a/b"} {
			got := ask(t, method, url+path)
			if got.status != http.StatusMethodNotAllowed || got.header.Get("Allow") != "GET, HEAD" || len(got.body) != 0 {
				t.Errorf("%s %s = %d with Allow %q and a body of %d bytes, want 405, GET, HEAD and none",
					method, path, got.status, got.header.Get("Allow"), len(got.body))
			}
			if got.header.Get("Location") != "" {
				t.Errorf("%s %s leads to %q", method, path, got.header.Get("Location"))
			}
		}
	}
}

func TestTheRootLeadsToTheProjectAndSecurityTxtSaysWhereToReport(t *testing.T) {
	url := host(t, catalogue()...)

	for _, path := range []string{"/", "/?ref=somewhere"} {
		root := ask(t, http.MethodGet, url+path)
		if root.status != http.StatusFound || root.header.Get("Location") != "https://github.com/majimtor/corinto" || len(root.body) != 0 {
			t.Errorf("GET %s = %d to %q with a body of %d bytes", path, root.status, root.header.Get("Location"), len(root.body))
		}
	}

	security := ask(t, http.MethodGet, url+"/.well-known/security.txt")
	want := "Contact: https://github.com/majimtor/corinto/security/advisories/new\n" +
		// Ninety days after the process started.
		"Expires: 2027-02-17T12:00:00Z\n" +
		"Policy: https://github.com/majimtor/corinto/blob/main/SECURITY.md\n" +
		"Preferred-Languages: en, es\n"
	if security.status != http.StatusOK || string(security.body) != want {
		t.Errorf("security.txt = %d\n%s\nwant\n%s", security.status, security.body, want)
	}
	if contentType := security.header.Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Errorf("security.txt is sent as %q", contentType)
	}
	if head := ask(t, http.MethodHead, url+"/.well-known/security.txt"); head.status != http.StatusOK || len(head.body) != 0 {
		t.Errorf("HEAD of security.txt = %d with a body of %d bytes", head.status, len(head.body))
	}
	if upper := ask(t, http.MethodGet, url+"/.WELL-KNOWN/SECURITY.TXT"); upper.status != http.StatusOK {
		t.Errorf("security.txt in upper case = %d, want 200: paths are lower-cased", upper.status)
	}
}

// The process serves one redirect domain: whatever name a request arrives
// under, the answer is the same, and none is built from that name.
func TestTheHostOfARequestPlaysNoPart(t *testing.T) {
	url := host(t, catalogue()...)
	for _, name := range []string{"go.example.dev", "evil.example", "localhost:8082", "GO.EXAMPLE.DEV"} {
		redirected := ask(t, http.MethodGet, url+"/k7m2xq9d", "Host", name)
		if redirected.status != http.StatusFound || strings.Contains(redirected.header.Get("Location"), strings.ToLower(name)) {
			t.Errorf("under the name %s a link = %d to %q", name, redirected.status, redirected.header.Get("Location"))
		}
		missing := ask(t, http.MethodGet, url+"/bcdfghjk", "Host", name)
		if missing.status != http.StatusNotFound || strings.Contains(strings.ToLower(string(missing.body)), strings.ToLower(name)) {
			t.Errorf("under the name %s an unknown link = %d, or the page repeats the name", name, missing.status)
		}
	}
}

// A defect inside the handler is a 500 without a body, and what goes to the
// log has nothing of the request in it.
func TestAPanicIsA500ThatLogsNothingOfTheRequest(t *testing.T) {
	written := &bytes.Buffer{}
	// A handler without its copy of the links: the lookup panics.
	broken, err := redirect.New(t.Context(), redirect.Settings{
		Logger: slog.New(slog.NewJSONHandler(written, nil)), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(broken)
	t.Cleanup(server.Close)

	got := ask(t, http.MethodGet, server.URL+"/sentinel-path?token=sentinel-query",
		"User-Agent", "sentinel-agent", "Referer", "https://sentinel-referrer.example/", "Host", "sentinel-host.example")
	if got.status != http.StatusInternalServerError || len(got.body) != 0 {
		t.Errorf("GET = %d with a body of %d bytes, want 500 and none", got.status, len(got.body))
	}
	server.Close()
	log := written.String()
	if !strings.Contains(log, "a redirect panicked") || !strings.Contains(log, `"stack"`) {
		t.Errorf("the panic is not logged: %s", log)
	}
	if strings.Contains(log, "sentinel") || strings.Contains(log, "127.0.0.1") {
		t.Errorf("the log carries something of the request: %s", log)
	}
}

// moving is a clock that a test moves.
type moving struct {
	mu sync.Mutex
	at time.Time
}

func (m *moving) read() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.at
}

func (m *moving) advance(by time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at = m.at.Add(by)
}

// limited starts the host behind a proxy on the loopback interface, which
// is where these tests call from: the address of a client is then whatever
// a request says in its forwarding header.
func limited(t *testing.T) (string, *moving) {
	t.Helper()
	clock := &moving{at: now}
	return hostAt(t, clock.read, "127.0.0.1", catalogue()...), clock
}

// from asks for a path as a client address.
func from(t *testing.T, address, method, url string) answer {
	t.Helper()
	return ask(t, method, url, "X-Forwarded-For", address)
}

// A client address makes twenty requests a second, and forty at once after
// being idle. Past that the answer is a 429 that says how long to wait and
// nothing else.
func TestAClientAddressIsToldToWaitPastItsRate(t *testing.T) {
	url, clock := limited(t)
	const client, neighbour = "203.0.113.7", "203.0.113.8"

	for i := range 40 {
		if got := from(t, client, http.MethodGet, url+"/k7m2xq9d"); got.status != http.StatusFound {
			t.Fatalf("request %d of the burst = %d, want 302", i+1, got.status)
		}
	}
	refused := from(t, client, http.MethodGet, url+"/k7m2xq9d")
	if refused.status != http.StatusTooManyRequests || refused.header.Get("Retry-After") != "1" {
		t.Errorf("request 41 = %d with Retry-After %q, want 429 and 1", refused.status, refused.header.Get("Retry-After"))
	}
	if len(refused.body) != 0 || refused.header.Get("Location") != "" || refused.header.Get("Content-Security-Policy") != "" {
		t.Errorf("the 429 carries a body of %d bytes or leads to %q", len(refused.body), refused.header.Get("Location"))
	}
	if got := from(t, neighbour, http.MethodGet, url+"/k7m2xq9d"); got.status != http.StatusFound {
		t.Errorf("another address = %d, want 302", got.status)
	}

	// One request is earned back every fiftieth of a second, and a refusal
	// takes nothing.
	clock.advance(50 * time.Millisecond)
	if got := from(t, client, http.MethodGet, url+"/k7m2xq9d"); got.status != http.StatusFound {
		t.Errorf("fifty milliseconds later = %d, want 302", got.status)
	}
	if got := from(t, client, http.MethodGet, url+"/k7m2xq9d"); got.status != http.StatusTooManyRequests {
		t.Errorf("and the next one = %d, want 429", got.status)
	}
	// A second later twenty fit, not forty.
	clock.advance(time.Second)
	served := 0
	for served < 100 && from(t, client, http.MethodGet, url+"/k7m2xq9d").status == http.StatusFound {
		served++
	}
	if served != 20 {
		t.Errorf("a second later %d requests were served, want 20", served)
	}
}

// Every request counts, whatever it asks for and whatever it is answered:
// whoever tries identifiers is told to wait, and then a link that exists
// does not answer either.
func TestEveryRequestCountsAgainstItsAddress(t *testing.T) {
	url, _ := limited(t)
	const guesser = "203.0.113.9"
	asked := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/bcdfghjk", http.StatusNotFound},
		{http.MethodGet, "/disabled", http.StatusNotFound},
		{http.MethodPost, "/k7m2xq9d", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusFound},
		{http.MethodGet, "/.well-known/security.txt", http.StatusOK},
		{http.MethodHead, "/k7m2xq9d", http.StatusFound},
		{http.MethodGet, "/a/b/c", http.StatusNotFound},
		{http.MethodDelete, "/", http.StatusMethodNotAllowed},
	}
	for i := range 40 {
		request := asked[i%len(asked)]
		if got := from(t, guesser, request.method, url+request.path); got.status != request.want {
			t.Fatalf("request %d, %s %s = %d, want %d", i+1, request.method, request.path, got.status, request.want)
		}
	}
	for _, request := range asked {
		if got := from(t, guesser, request.method, url+request.path); got.status != http.StatusTooManyRequests {
			t.Errorf("past the rate, %s %s = %d, want 429", request.method, request.path, got.status)
		}
	}
}

// One client is given a whole IPv6 network, and a forwarding header is
// believed only from a proxy that is trusted.
func TestWhatCountsAsOneClient(t *testing.T) {
	t.Run("the addresses of one IPv6 network", func(t *testing.T) {
		url, _ := limited(t)
		for i := range 40 {
			address := "2001:db8:1:2::" + strconv.Itoa(i+1)
			if got := from(t, address, http.MethodGet, url+"/k7m2xq9d"); got.status != http.StatusFound {
				t.Fatalf("request %d = %d, want 302", i+1, got.status)
			}
		}
		if got := from(t, "2001:db8:1:2:ffff::99", http.MethodGet, url+"/k7m2xq9d"); got.status != http.StatusTooManyRequests {
			t.Errorf("another address of the same network = %d, want 429", got.status)
		}
		if got := from(t, "2001:db8:1:3::1", http.MethodGet, url+"/k7m2xq9d"); got.status != http.StatusFound {
			t.Errorf("an address of another network = %d, want 302", got.status)
		}
	})
	t.Run("the connection, when no proxy is trusted", func(t *testing.T) {
		// Each request names another client; they all come from one.
		url := host(t, catalogue()...)
		for i := range 40 {
			if got := from(t, "192.0.2."+strconv.Itoa(i+1), http.MethodGet, url+"/k7m2xq9d"); got.status != http.StatusFound {
				t.Fatalf("request %d = %d, want 302", i+1, got.status)
			}
		}
		if got := from(t, "192.0.2.200", http.MethodGet, url+"/k7m2xq9d"); got.status != http.StatusTooManyRequests {
			t.Errorf("the forty-first request of the connection = %d, want 429", got.status)
		}
	})
}
