package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/majimtor/corinto/contract"
	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/auth"
)

// keyring is what resolves a credential in these tests: it knows the ones
// a test gave it, and remembers what it was asked.
type keyring struct {
	mu         sync.Mutex
	principals map[string]auth.Principal
	// failure, when set, is what every call returns: the keys cannot be
	// read.
	failure error
	asked   []string
}

func (k *keyring) Authenticate(_ context.Context, presented string) (auth.Principal, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, presented)
	if k.failure != nil {
		return auth.Principal{}, k.failure
	}
	principal, known := k.principals[presented]
	if !known {
		return auth.Principal{}, auth.ErrInvalidKey
	}
	return principal, nil
}

func (k *keyring) calls() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.asked)
}

// clock is a time the test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

// from hands a request to a handler as if it had come from an address,
// with no network in between.
type from struct {
	handler http.Handler
	address string
}

func (f from) RoundTrip(request *http.Request) (*http.Response, error) {
	served := request.Clone(request.Context())
	served.RemoteAddr = f.address
	served.RequestURI = request.URL.RequestURI()
	if served.Body == nil {
		served.Body = http.NoBody
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, served)
	response := recorder.Result()
	response.Request = request
	return response, nil
}

// The credentials of these tests. They are not keys: the keyring decides
// what each resolves to.
const (
	reader = "credential-of-the-reader"
	writer = "credential-of-the-writer"
)

// guarded returns the handler of the API with a keyring that knows two
// credentials, the clock its limits read and the log it writes.
func guarded(t *testing.T, proxies ...string) (http.Handler, *keyring, *clock, *log) {
	t.Helper()
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return guardedWith(t, document, operations(nil), proxies...)
}

func guardedWith(t *testing.T, document *openapi3.T, serving map[string]http.Handler, proxies ...string) (http.Handler, *keyring, *clock, *log) {
	t.Helper()
	keys := &keyring{principals: map[string]auth.Principal{
		reader: {WorkspaceID: "workspace-1", KeyID: "key-of-the-reader", Scopes: []auth.Scope{auth.ScopeLinksRead}},
		writer: {WorkspaceID: "workspace-1", KeyID: "key-of-the-writer", Scopes: []auth.Scope{auth.ScopeLinksRead, auth.ScopeLinksWrite}},
	}}
	trusted, err := ParseProxies(strings.Join(proxies, ","))
	if err != nil {
		t.Fatal(err)
	}
	written := &log{}
	moved := &clock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	settings := Settings{Logger: slog.New(slog.NewJSONHandler(written, nil)), Keys: keys, TrustedProxies: trusted}
	handler, err := newHandler(t.Context(), document, serving, settings, moved.read)
	if err != nil {
		t.Fatal(err)
	}
	return handler, keys, moved, written
}

// ask sends GET /v1/key from an address, with an Authorization header when
// one is given, through the client that checks every response.
func ask(t *testing.T, handler http.Handler, address, authorization string, headers ...string) (received, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.example/v1/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := contracttest.Over(t, from{handler, address}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return received{response.StatusCode, response.Header}, body
}

func TestAValidKeyIsToldWhichKeyItIs(t *testing.T) {
	handler, keys, _, _ := guarded(t)
	expiry := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	keys.principals["credential-that-expires"] = auth.Principal{
		WorkspaceID: "workspace-2", KeyID: "key-that-expires", Scopes: []auth.Scope{auth.ScopeMetricsRead}, ExpiresAt: &expiry,
	}

	cases := []struct {
		name          string
		authorization string
		want          string
	}{
		{"a key without an expiry", "Bearer " + writer,
			`{"id":"key-of-the-writer","workspace_id":"workspace-1","scopes":["links:read","links:write"],"expires_at":null}`},
		{"a key with an expiry", "Bearer credential-that-expires",
			`{"id":"key-that-expires","workspace_id":"workspace-2","scopes":["metrics:read"],"expires_at":"2027-01-31T00:00:00Z"}`},
		{"the scheme in lower case", "bearer " + reader,
			`{"id":"key-of-the-reader","workspace_id":"workspace-1","scopes":["links:read"],"expires_at":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response, body := ask(t, handler, "203.0.113.7:40000", c.authorization)
			if response.status != http.StatusOK || strings.TrimSpace(string(body)) != c.want {
				t.Errorf("GET /v1/key = %d %s, want 200 %s", response.status, body, c.want)
			}
		})
	}
}

// A different answer for each cause would tell which keys exist. The
// operations that need no key are not touched by any of it.
func TestEveryRequestWithoutAValidKeyGetsTheSameAnswer(t *testing.T) {
	handler, keys, _, _ := guarded(t)
	cases := []struct {
		name          string
		authorization string
		asksTheKeys   bool
	}{
		{"no header", "", false},
		{"another scheme", "Basic " + reader, false},
		{"the scheme alone", "Bearer", false},
		{"the scheme and nothing", "Bearer ", false},
		{"a credential without its scheme", reader, false},
		{"a credential nobody has", "Bearer credential-of-nobody", true},
		{"a valid credential after two spaces", "Bearer  " + reader, true},
	}
	var first []byte
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := keys.calls()
			response, body := ask(t, handler, "203.0.113.7:40000", c.authorization)
			if response.status != http.StatusUnauthorized || code(t, body) != CodeUnauthenticated {
				t.Errorf("GET /v1/key = %d %s, want 401 %s", response.status, code(t, body), CodeUnauthenticated)
			}
			if got := response.header.Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
			if first == nil {
				first = body
			}
			if string(body) != string(first) {
				t.Errorf("the answer differs from the first one: %s", body)
			}
			if asked := keys.calls() > before; asked != c.asksTheKeys {
				t.Errorf("the keys were asked: %v, want %v", asked, c.asksTheKeys)
			}
		})
	}

	for _, public := range []string{"/v1/health", "/v1/openapi.yaml"} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, public, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s without a key = %d, want 200", public, recorder.Code)
		}
	}
}

// Ten requests without a valid key in a minute, and the address is refused
// before its key is looked at: with a valid key too.
func TestAnAddressThatFailsTooOftenIsRefused(t *testing.T) {
	handler, keys, moved, _ := guarded(t)
	const address, neighbour = "203.0.113.7:40000", "203.0.113.8:40000"

	for i := range 10 {
		if response, _ := ask(t, handler, address, "Bearer credential-of-nobody"); response.status != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, response.status)
		}
	}
	asked := keys.calls()
	for name, authorization := range map[string]string{"another failure": "Bearer credential-of-nobody", "a valid key": "Bearer " + reader, "no key": ""} {
		response, body := ask(t, handler, address, authorization)
		if response.status != http.StatusTooManyRequests || code(t, body) != CodeRateLimited {
			t.Errorf("%s after ten failures = %d %s, want 429 %s", name, response.status, code(t, body), CodeRateLimited)
		}
		// One failure is earned back every six seconds.
		if got := response.header.Get("Retry-After"); got != "6" {
			t.Errorf("%s: Retry-After = %q, want 6", name, got)
		}
	}
	if keys.calls() != asked {
		t.Error("the keys were asked while the address was refused")
	}
	if response, _ := ask(t, handler, neighbour, "Bearer "+reader); response.status != http.StatusOK {
		t.Errorf("a valid key from another address = %d, want 200", response.status)
	}

	// A wait is told in whole seconds, rounded up: after half a second
	// there are five and a half to go, and five would be too soon.
	moved.advance(500 * time.Millisecond)
	if response, _ := ask(t, handler, address, ""); response.header.Get("Retry-After") != "6" {
		t.Errorf("half a second later Retry-After = %q, want 6", response.header.Get("Retry-After"))
	}

	// Six seconds after the last failure there is room for one: a valid
	// key passes and takes nothing, and the next failure closes the
	// address again.
	moved.advance(5500 * time.Millisecond)
	if response, _ := ask(t, handler, address, "Bearer "+reader); response.status != http.StatusOK {
		t.Errorf("a valid key six seconds later = %d, want 200", response.status)
	}
	if response, _ := ask(t, handler, address, "Bearer credential-of-nobody"); response.status != http.StatusUnauthorized {
		t.Errorf("one failure six seconds later = %d, want 401", response.status)
	}
	if response, _ := ask(t, handler, address, "Bearer "+reader); response.status != http.StatusTooManyRequests {
		t.Errorf("after that failure = %d, want 429", response.status)
	}

	// A minute later nothing is left of it.
	moved.advance(time.Minute)
	for i := range 10 {
		if response, _ := ask(t, handler, address, "Bearer credential-of-nobody"); response.status != http.StatusUnauthorized {
			t.Fatalf("a minute later, failure %d = %d, want 401", i+1, response.status)
		}
	}
}

// One client is given a whole IPv6 network. If each of its addresses had a
// count of its own, the limit would never be reached.
func TestTheAddressesOfOneIPv6NetworkCountTogether(t *testing.T) {
	handler, _, _, _ := guarded(t)
	for i := range 10 {
		address := "[2001:db8:1:2::" + strconv.Itoa(i+1) + "]:40000"
		if response, _ := ask(t, handler, address, ""); response.status != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, response.status)
		}
	}
	if response, _ := ask(t, handler, "[2001:db8:1:2:ffff::99]:40000", ""); response.status != http.StatusTooManyRequests {
		t.Errorf("another address of the same network = %d, want 429", response.status)
	}
	if response, _ := ask(t, handler, "[2001:db8:1:3::1]:40000", ""); response.status != http.StatusUnauthorized {
		t.Errorf("an address of another network = %d, want 401", response.status)
	}
}

// What a client writes in a forwarding header is believed only as far as
// the trusted proxies reach.
func TestAForwardingHeaderIsBelievedOnlyFromATrustedProxy(t *testing.T) {
	t.Run("from anyone else it changes nothing", func(t *testing.T) {
		handler, _, _, _ := guarded(t, "198.51.100.0/24")
		for i := range 10 {
			// Each request names another client; they all come from one.
			forwarded := "192.0.2." + strconv.Itoa(i+1)
			if response, _ := ask(t, handler, "203.0.113.7:40000", "", "X-Forwarded-For", forwarded); response.status != http.StatusUnauthorized {
				t.Fatalf("failure %d = %d, want 401", i+1, response.status)
			}
		}
		if response, _ := ask(t, handler, "203.0.113.7:40000", "", "X-Forwarded-For", "192.0.2.200"); response.status != http.StatusTooManyRequests {
			t.Errorf("the eleventh request of the address = %d, want 429", response.status)
		}
	})
	t.Run("from a proxy it names the client", func(t *testing.T) {
		handler, _, _, _ := guarded(t, "198.51.100.0/24")
		const proxy = "198.51.100.5:40000"
		for i := range 10 {
			if response, _ := ask(t, handler, proxy, "", "X-Forwarded-For", "192.0.2.1"); response.status != http.StatusUnauthorized {
				t.Fatalf("failure %d = %d, want 401", i+1, response.status)
			}
		}
		cases := []struct {
			name      string
			forwarded string
			want      int
		}{
			{"the same client", "192.0.2.1", http.StatusTooManyRequests},
			{"another client behind the proxy", "192.0.2.2", http.StatusUnauthorized},
			{"the same client, with something it wrote itself before", "192.0.2.77, 192.0.2.1", http.StatusTooManyRequests},
			{"the same client, through a second proxy", "192.0.2.1, 198.51.100.9", http.StatusTooManyRequests},
		}
		for _, c := range cases {
			if response, _ := ask(t, handler, proxy, "", "X-Forwarded-For", c.forwarded); response.status != c.want {
				t.Errorf("%s = %d, want %d", c.name, response.status, c.want)
			}
		}
	})
}

func TestAKeyMakes120RequestsAMinute(t *testing.T) {
	handler, _, moved, _ := guarded(t)
	const address = "203.0.113.7:40000"
	for i := range 120 {
		if response, _ := ask(t, handler, address, "Bearer "+reader); response.status != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, response.status)
		}
	}
	response, body := ask(t, handler, address, "Bearer "+reader)
	if response.status != http.StatusTooManyRequests || code(t, body) != CodeRateLimited {
		t.Errorf("request 121 = %d %s, want 429 %s", response.status, code(t, body), CodeRateLimited)
	}
	// One request is earned back every half second, and a wait is told in
	// whole seconds.
	if got := response.header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	// The limit is of the key, not of the address, and it is not a failure
	// of the address.
	if response, _ := ask(t, handler, address, "Bearer "+writer); response.status != http.StatusOK {
		t.Errorf("another key from the same address = %d, want 200", response.status)
	}

	moved.advance(500 * time.Millisecond)
	if response, _ := ask(t, handler, address, "Bearer "+reader); response.status != http.StatusOK {
		t.Errorf("half a second later = %d, want 200", response.status)
	}
	if response, _ := ask(t, handler, address, "Bearer "+reader); response.status != http.StatusTooManyRequests {
		t.Errorf("and the next one = %d, want 429", response.status)
	}
}

// withWrite returns the contract with one more operation, which changes
// something and asks for the scope links:write. No operation of the
// contract does either yet, and the gate has to be seen to do both.
func withWrite(t *testing.T) (*openapi3.T, map[string]http.Handler) {
	t.Helper()
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	document.Paths.Set("/v1/things", &openapi3.PathItem{Post: &openapi3.Operation{
		OperationID: "createThing",
		Security:    &openapi3.SecurityRequirements{{"apiKey": {"links:write"}}},
	}})
	serving := operations(nil)
	serving["createThing"] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]string{"id": "thing"})
	})
	return document, serving
}

// send calls an operation that only these tests have, so it cannot go
// through the client that checks responses against the contract.
func send(t *testing.T, handler http.Handler, method, path, credential string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	request.RemoteAddr = "203.0.113.7:40000"
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestAKeyWithoutTheScopeOfAnOperationIsRefused(t *testing.T) {
	document, serving := withWrite(t)
	handler, _, _, _ := guardedWith(t, document, serving)

	if created := send(t, handler, http.MethodPost, "/v1/things", writer); created.Code != http.StatusCreated {
		t.Errorf("with the scope = %d, want 201", created.Code)
	}
	refused := send(t, handler, http.MethodPost, "/v1/things", reader)
	var sent problem
	if err := json.Unmarshal(refused.Body.Bytes(), &sent); err != nil {
		t.Fatalf("the error is not JSON: %s", refused.Body.String())
	}
	if refused.Code != http.StatusForbidden || sent.Code != CodeForbidden || sent.Type != "about:blank" || sent.Status != http.StatusForbidden {
		t.Errorf("without the scope = %d %+v, want 403 %s", refused.Code, sent, CodeForbidden)
	}
	if !strings.Contains(sent.Detail, "links:write") {
		t.Errorf("the error does not name the scope: %q", sent.Detail)
	}
	if got := refused.Header().Get("Content-Type"); got != "application/problem+json; charset=utf-8" {
		t.Errorf("the error is sent as %q", got)
	}
	if without := send(t, handler, http.MethodPost, "/v1/things", ""); without.Code != http.StatusUnauthorized {
		t.Errorf("without a key = %d, want 401", without.Code)
	}
}

func TestAKeyMakes60ChangesAMinute(t *testing.T) {
	document, serving := withWrite(t)
	handler, _, moved, _ := guardedWith(t, document, serving)

	for i := range 60 {
		if created := send(t, handler, http.MethodPost, "/v1/things", writer); created.Code != http.StatusCreated {
			t.Fatalf("change %d = %d, want 201", i+1, created.Code)
		}
	}
	refused := send(t, handler, http.MethodPost, "/v1/things", writer)
	if refused.Code != http.StatusTooManyRequests || refused.Header().Get("Retry-After") != "1" {
		t.Errorf("change 61 = %d with Retry-After %q, want 429 and 1", refused.Code, refused.Header().Get("Retry-After"))
	}
	// Reading still has room: the 60 changes are among the 120 requests.
	if read := send(t, handler, http.MethodGet, "/v1/key", writer); read.Code != http.StatusOK {
		t.Errorf("a read after 60 changes = %d, want 200", read.Code)
	}
	moved.advance(time.Second)
	if created := send(t, handler, http.MethodPost, "/v1/things", writer); created.Code != http.StatusCreated {
		t.Errorf("a second later = %d, want 201", created.Code)
	}
}

// With the database away the API says so, and does not count it against
// whoever asked (docs/delivery-semantics.md).
func TestTheAPIAnswers503WhenTheKeysCannotBeRead(t *testing.T) {
	handler, keys, _, written := guarded(t)
	keys.failure = errors.New("sentinel-of-the-database: connection refused")

	var last received
	for range 12 {
		response, body := ask(t, handler, "203.0.113.7:40000", "Bearer "+reader)
		if response.status != http.StatusServiceUnavailable || code(t, body) != CodeUnavailable {
			t.Fatalf("GET /v1/key = %d %s, want 503 %s", response.status, code(t, body), CodeUnavailable)
		}
		if strings.Contains(string(body), "sentinel") {
			t.Errorf("the error tells the client why: %s", body)
		}
		last = response
	}
	failures := written.lines(t, "a key could not be resolved")
	if len(failures) != 12 || failures[11]["request"] != last.header.Get("X-Request-Id") ||
		failures[11]["error"] != "sentinel-of-the-database: connection refused" {
		t.Errorf("the failures are logged as %v", failures)
	}
}

// The log names the key of a request by its identifier, once the key is
// known to be valid, and never carries what was presented.
func TestTheLogNamesAValidKeyAndNeverACredential(t *testing.T) {
	handler, _, _, written := guarded(t)
	ask(t, handler, "203.0.113.7:40000", "Bearer "+reader)
	ask(t, handler, "203.0.113.7:40000", "Bearer sentinel-credential")
	ask(t, handler, "203.0.113.7:40000", "")

	requests := written.lines(t, "request")
	if len(requests) != 3 {
		t.Fatalf("%d lines for 3 requests", len(requests))
	}
	if requests[0]["key"] != "key-of-the-reader" || requests[0]["route"] != "GET /v1/key" {
		t.Errorf("the request with a valid key is logged as %v", requests[0])
	}
	for _, line := range requests[1:] {
		if _, named := line["key"]; named || line["status"] != float64(http.StatusUnauthorized) {
			t.Errorf("a request without a valid key is logged as %v", line)
		}
	}
	if strings.Contains(written.String(), "sentinel") || strings.Contains(written.String(), reader) {
		t.Errorf("the log carries a credential: %s", written.String())
	}
	if strings.Contains(written.String(), "203.0.113.7") {
		t.Errorf("the log carries the address of the client: %s", written.String())
	}
}

func TestTheAddressOfAClient(t *testing.T) {
	proxies, err := ParseProxies("198.51.100.0/24, 2001:db8:ffff::/48")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		remote    string
		forwarded []string
		trusted   []netip.Prefix
		want      string
	}{
		{"the connection, when no proxy is trusted", "203.0.113.7:40000", []string{"192.0.2.1"}, nil, "203.0.113.7"},
		{"the connection, when it is not a proxy", "203.0.113.7:40000", []string{"192.0.2.1"}, proxies, "203.0.113.7"},
		{"what the proxy saw", "198.51.100.5:40000", []string{"192.0.2.1"}, proxies, "192.0.2.1"},
		{"the rightmost address that is not a proxy", "198.51.100.5:40000", []string{"192.0.2.77, 192.0.2.1, 198.51.100.9"}, proxies, "192.0.2.1"},
		{"across several headers", "198.51.100.5:40000", []string{"192.0.2.77", "192.0.2.1"}, proxies, "192.0.2.1"},
		{"the proxy itself, when it forwards nothing", "198.51.100.5:40000", nil, proxies, "198.51.100.5"},
		{"the last proxy, when every hop is one", "198.51.100.5:40000", []string{"198.51.100.9"}, proxies, "198.51.100.9"},
		{"the proxy, when the header is not an address", "198.51.100.5:40000", []string{"unknown"}, proxies, "198.51.100.5"},
		{"what can be read to the right of what cannot", "198.51.100.5:40000", []string{"192.0.2.1, unknown, 198.51.100.9"}, proxies, "198.51.100.9"},
		{"an IPv4 address written as IPv6", "[::ffff:203.0.113.7]:40000", nil, nil, "203.0.113.7"},
		{"an IPv6 client behind an IPv6 proxy", "[2001:db8:ffff::5]:40000", []string{"2001:db8:1:2::9"}, proxies, "2001:db8:1:2::9"},
		{"no address at all", "", nil, nil, "invalid IP"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/key", nil)
			request.RemoteAddr = c.remote
			for _, value := range c.forwarded {
				request.Header.Add("X-Forwarded-For", value)
			}
			if got := clientAddress(request, c.trusted).String(); got != c.want {
				t.Errorf("clientAddress = %s, want %s", got, c.want)
			}
		})
	}
}

func TestTheListOfTrustedProxies(t *testing.T) {
	proxies, err := ParseProxies(" 10.0.0.0/8 ,192.0.2.10,, 2001:db8::/32,::ffff:192.0.2.11 ")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, proxy := range proxies {
		got = append(got, proxy.String())
	}
	if want := []string{"10.0.0.0/8", "192.0.2.10/32", "2001:db8::/32", "192.0.2.11/32"}; !slices.Equal(got, want) {
		t.Errorf("ParseProxies = %v, want %v", got, want)
	}
	if none, err := ParseProxies(""); err != nil || len(none) != 0 {
		t.Errorf("an empty list = %v, %v; want no proxy and no error", none, err)
	}
	for _, list := range []string{"proxy.example", "10.0.0.0/33", "10.0.0.1, nothing"} {
		if _, err := ParseProxies(list); err == nil {
			t.Errorf("ParseProxies(%q) gave no error", list)
		}
	}
}

// What is kept of an address is a hash that only this process can compute.
func TestAnAddressIsKeptOnlyAsAKeyedHash(t *testing.T) {
	first, err := newAddresses()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newAddresses()
	if err != nil {
		t.Fatal(err)
	}
	address := netip.MustParseAddr("203.0.113.7")
	key := first.key(address)
	if key != first.key(address) {
		t.Error("the same address gives two keys")
	}
	if key == first.key(netip.MustParseAddr("203.0.113.8")) {
		t.Error("two addresses give the same key")
	}
	if key == second.key(address) {
		t.Error("two processes give the same key to an address: the hash has no secret")
	}
	if len(key) != 32 || strings.Contains(key, string(address.AsSlice())) || strings.Contains(key, address.String()) {
		t.Error("the key is not a hash, or carries the address")
	}
	if first.key(netip.MustParseAddr("2001:db8:1:2::1")) != first.key(netip.MustParseAddr("2001:db8:1:2:ffff::9")) {
		t.Error("two addresses of one IPv6 network give different keys")
	}
	if first.key(netip.MustParseAddr("2001:db8:1:2::1")) == first.key(netip.MustParseAddr("2001:db8:1:3::1")) {
		t.Error("two IPv6 networks give the same key")
	}
}
