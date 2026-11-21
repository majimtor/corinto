package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/majimtor/corinto/contract"
	"github.com/majimtor/corinto/contract/contracttest"
	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/rollup"
)

// log is where a server under test writes while the test reads.
type log struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

func (l *log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.String()
}

// lines returns the log lines with a given message.
func (l *log) lines(t *testing.T, message string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for line := range strings.Lines(l.String()) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("a log line is not JSON: %q", line)
		}
		if entry["msg"] == message {
			found = append(found, entry)
		}
	}
	return found
}

// serve starts the API as the binary builds it and returns its address, its
// log and the function that stops it. Stopping waits for the requests in
// flight, so that their log lines are written before the test reads them.
func serve(t *testing.T) (string, *log, func()) {
	t.Helper()
	written := &log{}
	handler, err := New(t.Context(), Settings{Logger: slog.New(slog.NewJSONHandler(written, nil)), Keys: &keyring{}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL, written, server.Close
}

// received is a response, read to its end.
type received struct {
	status int
	header http.Header
}

// call sends a request through the client that checks every response
// against the contract, and returns the response with its body.
func call(t *testing.T, method, url string, body io.Reader, headers ...string) (received, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := contracttest.Client(t).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	read, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return received{response.StatusCode, response.Header}, read
}

// code returns the code of an error response.
func code(t *testing.T, body []byte) Code {
	t.Helper()
	var sent problem
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("the error is not JSON: %q", body)
	}
	return sent.Code
}

func TestTheFirstOperationsAnswerAsTheContractSays(t *testing.T) {
	url, _, _ := serve(t)

	health, body := call(t, http.MethodGet, url+"/v1/health", nil)
	if health.status != http.StatusOK || strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Errorf("GET /v1/health = %d %q", health.status, body)
	}

	document, body := call(t, http.MethodGet, url+"/v1/openapi.yaml", nil)
	embedded, err := contract.Document()
	if err != nil {
		t.Fatal(err)
	}
	if document.status != http.StatusOK || !bytes.Equal(body, embedded) {
		t.Errorf("GET /v1/openapi.yaml = %d with %d bytes, want the %d of the embedded document", document.status, len(body), len(embedded))
	}
	if got := document.header.Get("Content-Type"); got != "application/yaml; charset=utf-8" {
		t.Errorf("the document is sent as %q", got)
	}

	// Nothing between the server and the client keeps an answer, and each
	// request has an identifier of its own.
	for _, response := range []received{health, document} {
		if got := response.header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
	}
	first, second := health.header.Get("X-Request-Id"), document.header.Get("X-Request-Id")
	if len(first) < 20 || first == second {
		t.Errorf("the identifiers of two requests are %q and %q", first, second)
	}

	head, body := call(t, http.MethodHead, url+"/v1/health", nil)
	if head.status != http.StatusOK || len(body) != 0 {
		t.Errorf("HEAD /v1/health = %d with %d bytes of body", head.status, len(body))
	}
}

func TestWhatMatchesNoOperationIsAnErrorOfTheContract(t *testing.T) {
	url, _, _ := serve(t)
	cases := []struct {
		name   string
		method string
		path   string
		status int
		code   Code
		allow  string
	}{
		{"a path nobody has", http.MethodGet, "/v1/nothing", http.StatusNotFound, CodeNotFound, ""},
		{"the root", http.MethodGet, "/", http.StatusNotFound, CodeNotFound, ""},
		{"no version", http.MethodGet, "/health", http.StatusNotFound, CodeNotFound, ""},
		{"another case", http.MethodGet, "/V1/health", http.StatusNotFound, CodeNotFound, ""},
		{"a trailing slash", http.MethodGet, "/v1/health/", http.StatusNotFound, CodeNotFound, ""},
		{"an empty segment", http.MethodGet, "/v1//health", http.StatusNotFound, CodeNotFound, ""},
		{"a dot segment", http.MethodGet, "/v1/./health", http.StatusNotFound, CodeNotFound, ""},
		{"a parent segment", http.MethodGet, "/v1/links/../health", http.StatusNotFound, CodeNotFound, ""},
		{"an encoded slash", http.MethodGet, "/v1%2Fhealth", http.StatusNotFound, CodeNotFound, ""},
		{"another method", http.MethodPost, "/v1/health", http.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET, HEAD"},
		{"a deletion of the contract", http.MethodDelete, "/v1/openapi.yaml", http.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET, HEAD"},
		{"a question about the methods", http.MethodOptions, "/v1/health", http.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET, HEAD"},
		{"a method of no operation", "PROPFIND", "/v1/nothing", http.StatusNotFound, CodeNotFound, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response, body := call(t, c.method, url+c.path, nil)
			if response.status != c.status || code(t, body) != c.code {
				t.Errorf("%s %s = %d %s, want %d %s", c.method, c.path, response.status, code(t, body), c.status, c.code)
			}
			if got := response.header.Get("Allow"); got != c.allow {
				t.Errorf("Allow = %q, want %q", got, c.allow)
			}
			if got := response.header.Get("Content-Type"); got != "application/problem+json; charset=utf-8" {
				t.Errorf("the error is sent as %q", got)
			}
		})
	}
}

// An operation that takes no body refuses one, and a body over the limit is
// refused whatever it was sent to.
func TestTheBodyOfARequestIsBounded(t *testing.T) {
	url, _, _ := serve(t)
	cases := []struct {
		name   string
		method string
		path   string
		bytes  int
		status int
		code   Code
	}{
		{"a body the operation does not take", http.MethodGet, "/v1/health", 1, http.StatusBadRequest, CodeInvalidRequest},
		{"the largest body, to an operation that takes none", http.MethodGet, "/v1/health", 16_000, http.StatusBadRequest, CodeInvalidRequest},
		{"one byte over the limit", http.MethodGet, "/v1/health", 16_001, http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
		{"over the limit, to a path nobody has", http.MethodPost, "/v1/nothing", 64_000, http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
		{"over the limit, with a method the path does not have", http.MethodPost, "/v1/health", 64_000, http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
		{"within the limit, to a path nobody has", http.MethodPost, "/v1/nothing", 16_000, http.StatusNotFound, CodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response, body := call(t, c.method, url+c.path, bytes.NewReader(make([]byte, c.bytes)))
			if response.status != c.status || code(t, body) != c.code {
				t.Errorf("%d bytes to %s %s = %d %s, want %d %s",
					c.bytes, c.method, c.path, response.status, code(t, body), c.status, c.code)
			}
		})
	}
}

// failing returns the handler of the API with one operation replaced.
func failing(t *testing.T, written io.Writer, id string, replacement http.HandlerFunc) http.Handler {
	t.Helper()
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	serving := operations(nil, Settings{}, time.Now)
	serving[id] = replacement
	settings := Settings{Logger: slog.New(slog.NewJSONHandler(written, nil)), Keys: &keyring{}}
	handler, err := newHandler(t.Context(), document, serving, settings, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestAPanicInAnOperationIsTheErrorOfTheContractAndIsLogged(t *testing.T) {
	written := &log{}
	server := httptest.NewServer(failing(t, written, "getHealth", func(http.ResponseWriter, *http.Request) {
		panic("sentinel-of-the-panic")
	}))
	t.Cleanup(server.Close)

	response, body := call(t, http.MethodGet, server.URL+"/v1/health", nil)
	if response.status != http.StatusInternalServerError || code(t, body) != CodeInternalError {
		t.Errorf("GET /v1/health = %d %s, want 500 %s", response.status, code(t, body), CodeInternalError)
	}
	// The reason goes to the log, not to whoever asks.
	if strings.Contains(string(body), "sentinel-of-the-panic") {
		t.Errorf("the error tells the client why: %s", body)
	}
	// The other operations still answer.
	if after, _ := call(t, http.MethodGet, server.URL+"/v1/openapi.yaml", nil); after.status != http.StatusOK {
		t.Errorf("after the panic, GET /v1/openapi.yaml = %d", after.status)
	}

	server.Close()
	id := response.header.Get("X-Request-Id")
	panics := written.lines(t, "an operation panicked")
	if len(panics) != 1 || panics[0]["request"] != id || panics[0]["panic"] != "sentinel-of-the-panic" || panics[0]["stack"] == "" {
		t.Errorf("the panic is logged as %v, want one line with the request %s", panics, id)
	}
	requests := written.lines(t, "request")
	if len(requests) != 2 || requests[0]["request"] != id || requests[0]["status"] != float64(http.StatusInternalServerError) {
		t.Errorf("the requests are logged as %v", requests)
	}
}

// What an operation already sent cannot be taken back: the status stays,
// and the panic is logged.
func TestAPanicAfterTheResponseStartedKeepsItsStatus(t *testing.T) {
	written := &log{}
	handler := failing(t, written, "getHealth", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		panic("late")
	})
	recorded := httptest.NewRecorder()
	handler.ServeHTTP(recorded, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/health", nil))

	if recorded.Code != http.StatusOK || strings.Contains(recorded.Body.String(), "internal_error") {
		t.Errorf("the response is %d %s, want the 200 that was already sent and nothing after it", recorded.Code, recorded.Body.String())
	}
	if len(written.lines(t, "an operation panicked")) != 1 {
		t.Errorf("the panic is not logged: %s", written.String())
	}
}

// The log names a request by its route and never by what the client wrote:
// a path carries identifiers, a query string can carry anything, and
// neither a key nor a user agent belongs in a log.
func TestARequestLeavesOneLineWithoutWhatTheClientWrote(t *testing.T) {
	url, written, stop := serve(t)
	call(t, http.MethodGet, url+"/v1/health?token=sentinel-query", nil,
		"User-Agent", "sentinel-agent", "Authorization", "Bearer sentinel-key", "Referer", "https://sentinel-referrer.example/")
	call(t, http.MethodGet, url+"/v1/sentinel-path", nil)
	call(t, http.MethodPost, url+"/v1/health", nil)
	stop()

	requests := written.lines(t, "request")
	if len(requests) != 3 {
		t.Fatalf("%d lines for 3 requests: %s", len(requests), written.String())
	}
	want := []struct {
		method string
		route  string
		status float64
	}{
		{http.MethodGet, "GET /v1/health", http.StatusOK},
		{http.MethodGet, "", http.StatusNotFound},
		{http.MethodPost, "", http.StatusMethodNotAllowed},
	}
	for i, line := range requests {
		if line["method"] != want[i].method || line["route"] != want[i].route || line["status"] != want[i].status ||
			line["request"] == "" || line["duration"] == "" {
			t.Errorf("line %d is %v, want %+v", i, line, want[i])
		}
		fields := slices.Sorted(func(yield func(string) bool) {
			for field := range line {
				if !yield(field) {
					return
				}
			}
		})
		if !slices.Equal(fields, []string{"duration", "level", "method", "msg", "request", "route", "status", "time"}) {
			t.Errorf("line %d has the fields %v", i, fields)
		}
	}
	if strings.Contains(written.String(), "sentinel") {
		t.Errorf("the log carries something the client wrote: %s", written.String())
	}
}

// The contract and the server must name the same operations, or the server
// does not start.
func TestTheServerDoesNotStartUnlessItServesExactlyTheContract(t *testing.T) {
	settings := Settings{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Keys: &keyring{}}
	load := func(t *testing.T) *openapi3.T {
		t.Helper()
		document, err := contract.Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return document
	}
	nothing := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cases := []struct {
		name   string
		change func(document *openapi3.T, serving map[string]http.Handler)
		want   []string
	}{
		{"an operation of the contract that nothing serves",
			func(_ *openapi3.T, serving map[string]http.Handler) { delete(serving, "getContract") },
			[]string{`nothing serves the operation "getContract" of the contract`}},
		{"a handler the contract does not know",
			func(_ *openapi3.T, serving map[string]http.Handler) { serving["deleteEverything"] = nothing },
			[]string{`the contract has no operation "deleteEverything", which a handler serves`}},
		{"both at once, in a fixed order",
			func(_ *openapi3.T, serving map[string]http.Handler) {
				delete(serving, "getHealth")
				serving["deleteEverything"] = nothing
			},
			[]string{`nothing serves the operation "getHealth" of the contract; the contract has no operation "deleteEverything", which a handler serves`}},
		{"an operation without an identifier",
			func(document *openapi3.T, _ map[string]http.Handler) {
				document.Paths.Find("/v1/health").Get.OperationID = ""
			},
			[]string{"GET /v1/health has no operation identifier", `the contract has no operation "getHealth"`}},
		{"two operations with one identifier",
			func(document *openapi3.T, _ map[string]http.Handler) {
				document.Paths.Find("/v1/openapi.yaml").Get.OperationID = "getHealth"
			},
			[]string{`two operations are called "getHealth"`}},
		{"an operation that does not say whether it needs a key",
			func(document *openapi3.T, _ map[string]http.Handler) {
				document.Paths.Find("/v1/key").Get.Security = nil
			},
			[]string{`the operation "getKey" does not say whether it needs a key`}},
		{"a scope that does not exist",
			func(document *openapi3.T, _ map[string]http.Handler) {
				document.Paths.Find("/v1/key").Get.Security = &openapi3.SecurityRequirements{{"apiKey": {"links:delete"}}}
			},
			[]string{`the operation "getKey" asks for the scope "links:delete", which does not exist`}},
		{"two scopes for one operation",
			func(document *openapi3.T, _ map[string]http.Handler) {
				document.Paths.Find("/v1/key").Get.Security = &openapi3.SecurityRequirements{{"apiKey": {"links:read", "links:write"}}}
			},
			[]string{`the operation "getKey" asks for something other than the scheme "apiKey" with at most one scope`}},
		{"another scheme",
			func(document *openapi3.T, _ map[string]http.Handler) {
				document.Paths.Find("/v1/key").Get.Security = &openapi3.SecurityRequirements{{"session": {}}}
			},
			[]string{`the operation "getKey" asks for something other than the scheme "apiKey"`}},
		{"a key or nothing",
			func(document *openapi3.T, _ map[string]http.Handler) {
				document.Paths.Find("/v1/key").Get.Security = &openapi3.SecurityRequirements{{"apiKey": {}}, {}}
			},
			[]string{`the operation "getKey" asks for something other than the scheme "apiKey"`}},
		{"a path that cannot be routed",
			func(document *openapi3.T, serving map[string]http.Handler) {
				document.Paths.Set("/v1/{a}{b}", &openapi3.PathItem{Get: &openapi3.Operation{
					OperationID: "getBoth", Security: &openapi3.SecurityRequirements{},
				}})
				serving["getBoth"] = nothing
			},
			[]string{`"GET /v1/{a}{b}" cannot be routed`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			document, serving := load(t), operations(nil, Settings{}, time.Now)
			c.change(document, serving)
			handler, err := newHandler(t.Context(), document, serving, settings, time.Now)
			if err == nil || handler != nil {
				t.Fatalf("the server was built: err = %v", err)
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to say %q", err, want)
				}
			}
		})
	}

	if _, err := newHandler(t.Context(), load(t), operations(nil, Settings{}, time.Now), settings, time.Now); err != nil {
		t.Errorf("with the contract and the operations as they are: err = %v", err)
	}
}

// The codes are written twice, in the contract and here. This keeps the two
// lists equal.
func TestTheCodesAreTheOnesOfTheContract(t *testing.T) {
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var inContract []Code
	for _, value := range document.Components.Schemas["Problem"].Value.Properties["code"].Value.Enum {
		text, _ := value.(string)
		inContract = append(inContract, Code(text))
	}
	if !slices.Equal(inContract, codes()) {
		t.Errorf("the contract has the codes %v and the server %v", inContract, codes())
	}
}

// The reasons are written twice, in the contract and here. This keeps the
// two lists equal.
// A breakdown by device or by browser names the values the classifier
// gives, and the contract lists them: a value that one has and the other
// does not is a value a client was not told about.
func TestTheValuesOfABreakdownAreTheOnesOfTheContract(t *testing.T) {
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	listed := func(schema string) []string {
		t.Helper()
		found := document.Components.Schemas[schema]
		if found == nil {
			t.Fatalf("the contract has no schema %s", schema)
		}
		var values []string
		for _, value := range found.Value.Enum {
			text, _ := value.(string)
			values = append(values, text)
		}
		return values
	}
	var devices, browsers []string
	for _, device := range classify.Devices() {
		devices = append(devices, string(device))
	}
	for _, browser := range classify.Browsers() {
		browsers = append(browsers, string(browser))
	}
	if got := listed("Device"); !slices.Equal(got, devices) {
		t.Errorf("the contract lists the devices %v and the classifier gives %v", got, devices)
	}
	if got := listed("Browser"); !slices.Equal(got, browsers) {
		t.Errorf("the contract lists the browsers %v and the classifier gives %v", got, browsers)
	}
}

func TestTheReasonsAreTheOnesOfTheContract(t *testing.T) {
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entry := document.Components.Schemas["FieldError"].Value
	var inContract []Reason
	for _, value := range entry.Properties["code"].Value.Enum {
		text, _ := value.(string)
		inContract = append(inContract, Reason(text))
	}
	if !slices.Equal(inContract, reasons()) {
		t.Errorf("the contract has the reasons %v and the server %v", inContract, reasons())
	}
	if places := entry.Properties["in"].Value.Enum; len(places) != 2 || places[0] != inBody || places[1] != inQuery {
		t.Errorf("the contract has the places %v, want %s and %s", places, inBody, inQuery)
	}
}

// The contract states the bounds of what a link takes, and the rules of a
// link enforce them. A value at a bound of the contract is one the rules
// accept, and one past it is one they refuse: neither list can move alone.
func TestTheBoundsOfTheContractAreTheBoundsOfALink(t *testing.T) {
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	draft := document.Components.Schemas["LinkDraft"].Value.Properties
	bound := func(limit *uint64) int {
		t.Helper()
		if limit == nil {
			t.Fatal("the contract leaves a value of a link without a bound")
		}
		return int(*limit) //nolint:gosec // a bound of the contract, a few thousand at most
	}

	alias := draft["alias"].Value
	shortest, longest := int(alias.MinLength), bound(alias.MaxLength) //nolint:gosec // as above
	for length, want := range map[int]bool{shortest - 1: false, shortest: true, longest: true, longest + 1: false} {
		if _, err := link.ParseAlias(strings.Repeat("a", length)); (err == nil) != want {
			t.Errorf("an alias of %d characters: err = %v, and the contract takes from %d to %d", length, err, shortest, longest)
		}
	}

	longest = bound(draft["destination"].Value.MaxLength)
	for length, want := range map[int]bool{longest: true, longest + 1: false} {
		destination := "https://example.com/" + strings.Repeat("a", length-len("https://example.com/"))
		if _, err := link.ParseDestination(destination, link.Policy{}); (err == nil) != want {
			t.Errorf("a destination of %d characters: err = %v, and the contract takes up to %d", length, err, longest)
		}
	}

	longest = bound(document.Components.Schemas["CampaignValue"].Value.MaxLength)
	for length, want := range map[int]bool{longest: true, longest + 1: false} {
		campaign := link.Campaign{Source: strings.Repeat("a", length)}
		if _, err := link.Resolve("https://example.com/", campaign); (err == nil) != want {
			t.Errorf("a campaign value of %d characters: err = %v, and the contract takes up to %d", length, err, longest)
		}
	}

	tags := draft["tags"].Value
	for name, pair := range map[string][2]int{
		"the length of a title": {bound(draft["title"].Value.MaxLength), link.MaxTitleLength},
		"the number of tags":    {bound(tags.MaxItems), link.MaxTags},
		"the length of a tag":   {bound(tags.Items.Value.MaxLength), link.MaxTagLength},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s is %d in the contract and %d in the rules of a link", name, pair[0], pair[1])
		}
	}
}

// The contract states the bounds of what the metrics take and give, and
// the reads enforce their own: the two are the same numbers and the same
// lists.
func TestTheBoundsOfTheContractAreTheBoundsOfTheMetrics(t *testing.T) {
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var limit *openapi3.Schema
	for _, parameter := range document.Paths.Find("/v1/links/{id}/metrics/breakdown").Get.Parameters {
		if parameter.Value.Name == "limit" {
			limit = parameter.Value.Schema.Value
		}
	}
	if limit == nil || limit.Min == nil || limit.Max == nil {
		t.Fatal("the contract leaves the values of a breakdown without a bound")
	}
	if *limit.Min != 1 || *limit.Max != rollup.MaxShares || fmt.Sprint(limit.Default) != fmt.Sprint(rollup.DefaultShares) {
		t.Errorf("a breakdown names from %v to %v values in the contract, %v unless asked; the reads take up to %d, and %d",
			*limit.Min, *limit.Max, limit.Default, rollup.MaxShares, rollup.DefaultShares)
	}
	schemas := document.Components.Schemas
	most := func(schema, property string) uint64 {
		t.Helper()
		for _, part := range schemas[schema].Value.AllOf {
			if list := part.Value.Properties[property]; list != nil && list.Value.MaxItems != nil {
				return *list.Value.MaxItems
			}
		}
		t.Fatalf("the contract leaves %s of %s without a bound", property, schema)
		return 0
	}
	if points := most("MetricsSeries", "points"); points != rollup.MaxPoints {
		t.Errorf("a series has up to %d points in the contract and %d in the reads", points, rollup.MaxPoints)
	}
	if items := most("MetricsBreakdown", "items"); items != rollup.MaxShares {
		t.Errorf("a breakdown has up to %d items in the contract and %d in the reads", items, rollup.MaxShares)
	}
	for name, want := range map[string][]any{
		"Granularity": {string(rollup.ByHour), string(rollup.ByDay)},
		"Dimension":   {string(rollup.DimensionReferrerHost), string(rollup.DimensionDevice), string(rollup.DimensionBrowser)},
	} {
		if got := schemas[name].Value.Enum; !slices.Equal(got, want) {
			t.Errorf("%s is %v in the contract and %v in the reads", name, got, want)
		}
	}
}

// The same for the status of the ingestion: the days of the contract are
// the days of the reconciliation.
func TestTheBoundsOfTheContractAreTheBoundsOfTheReconciliation(t *testing.T) {
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var days *openapi3.Schema
	for _, parameter := range document.Paths.Find("/v1/ingestion").Get.Parameters {
		if parameter.Value.Name == "days" {
			days = parameter.Value.Schema.Value
		}
	}
	if days == nil || days.Min == nil || days.Max == nil {
		t.Fatal("the contract leaves the days of the status without a bound")
	}
	if *days.Min != 1 || *days.Max != ingest.MaxDays || fmt.Sprint(days.Default) != fmt.Sprint(ingest.DefaultDays) {
		t.Errorf("the status covers from %v to %v days in the contract, %v unless asked; the reconciliation up to %d, and %d",
			*days.Min, *days.Max, days.Default, ingest.MaxDays, ingest.DefaultDays)
	}
	listed := document.Components.Schemas["IngestionStatus"].Value.Properties["days"].Value
	if listed.MaxItems == nil || *listed.MaxItems != ingest.MaxDays || listed.MinItems != 1 {
		t.Errorf("the status lists from %d to %v days in the contract, and the reconciliation from 1 to %d", listed.MinItems, listed.MaxItems, ingest.MaxDays)
	}
}
