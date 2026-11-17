package contracttest_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/majimtor/corinto/contract/contracttest"
)

// recording is a test that remembers its failures instead of failing, so
// that the checks of the client can be seen to fail.
type recording struct {
	testing.TB
	failures []string
}

func (r *recording) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// answer is what a server under test sends back.
type answer struct {
	status  int
	headers map[string]string
	body    string
}

// good returns the headers of a response that follows every rule.
func good(contentType string) map[string]string {
	return map[string]string{
		"Content-Type":           contentType,
		"X-Content-Type-Options": "nosniff",
		"X-Request-Id":           "a-request",
	}
}

// without returns headers with one removed, and with returns them with one
// more.
func without(headers map[string]string, name string) map[string]string {
	delete(headers, name)
	return headers
}

func with(headers map[string]string, name, value string) map[string]string {
	headers[name] = value
	return headers
}

const (
	jsonType    = "application/json; charset=utf-8"
	problemType = "application/problem+json; charset=utf-8"
	notFound    = `{"type":"about:blank","title":"Not Found","status":404,"code":"not_found"}`
)

// failures sends one request to a server that gives one answer, through the
// client under test, and returns what the client found wrong.
func failures(t *testing.T, method, path string, sent answer) []string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for name, value := range sent.headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(sent.status)
		_, _ = io.WriteString(w, sent.body)
	}))
	defer server.Close()

	recorder := &recording{TB: t}
	request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := contracttest.Client(recorder).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return recorder.failures
}

func TestAnAnswerWithinTheContractAndTheRulesPasses(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		sent   answer
	}{
		{"an operation", http.MethodGet, "/v1/health", answer{200, good(jsonType), `{"status":"ok"}`}},
		{"a documented error of an operation", http.MethodGet, "/v1/health",
			answer{500, good(problemType), `{"type":"about:blank","title":"Internal Server Error","status":500,"code":"internal_error"}`}},
		{"a field the contract does not know yet", http.MethodGet, "/v1/health", answer{200, good(jsonType), `{"status":"ok","since":"today"}`}},
		{"a path nobody has", http.MethodGet, "/v1/nothing", answer{404, good(problemType), notFound}},
		{"a method the path does not have", http.MethodPost, "/v1/health",
			answer{405, with(good(problemType), "Allow", "GET, HEAD"), `{"type":"about:blank","title":"Method Not Allowed","status":405,"code":"method_not_allowed"}`}},
		{"a body over the limit, anywhere", http.MethodPost, "/v1/nothing",
			answer{413, good(problemType), `{"type":"about:blank","title":"Request Entity Too Large","status":413,"code":"payload_too_large"}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if found := failures(t, c.method, c.path, c.sent); len(found) != 0 {
				t.Errorf("the client found fault with it: %v", found)
			}
		})
	}
}

func TestAnAnswerOutsideTheContractOrTheRulesIsFound(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		sent   answer
		want   string
	}{
		{"a status the operation does not document", http.MethodGet, "/v1/health",
			answer{201, good(jsonType), `{"status":"ok"}`}, "status is not supported"},
		{"a value outside an enumeration", http.MethodGet, "/v1/health",
			answer{200, good(jsonType), `{"status":"down"}`}, "does not match the contract of getHealth"},
		{"a required field that is missing", http.MethodGet, "/v1/health",
			answer{200, good(jsonType), `{}`}, "does not match the contract of getHealth"},
		{"a media type the operation does not document", http.MethodGet, "/v1/health",
			answer{200, good("text/plain; charset=utf-8"), `ok`}, "does not match the contract of getHealth"},
		{"an error of an operation that is a bare string", http.MethodGet, "/v1/health",
			answer{500, good("text/plain; charset=utf-8"), `it broke`}, "does not match the contract of getHealth"},
		{"an error with a code outside the enumeration", http.MethodGet, "/v1/health",
			answer{500, good(problemType), `{"type":"about:blank","title":"x","status":500,"code":"oops"}`}, "does not match the contract of getHealth"},
		{"no nosniff", http.MethodGet, "/v1/health",
			answer{200, without(good(jsonType), "X-Content-Type-Options"), `{"status":"ok"}`}, "X-Content-Type-Options is not nosniff"},
		{"no request identifier", http.MethodGet, "/v1/health",
			answer{200, without(good(jsonType), "X-Request-Id"), `{"status":"ok"}`}, "X-Request-Id is missing"},
		{"a cross-origin header", http.MethodGet, "/v1/health",
			answer{200, with(good(jsonType), "Access-Control-Allow-Origin", "*"), `{"status":"ok"}`}, "is a cross-origin header"},
		{"a body without its character set", http.MethodGet, "/v1/health",
			answer{200, good("application/json"), `{"status":"ok"}`}, "without its character set"},
		{"a path nobody has that answers", http.MethodGet, "/v1/nothing",
			answer{200, good(jsonType), `{"status":"ok"}`}, "so the answer is 404"},
		{"a trailing slash taken for the path", http.MethodGet, "/v1/health/",
			answer{200, good(jsonType), `{"status":"ok"}`}, "so the answer is 404"},
		{"an encoded slash taken for the path", http.MethodGet, "/v1%2Fhealth",
			answer{200, good(jsonType), `{"status":"ok"}`}, "so the answer is 404"},
		{"a 404 that is a bare string", http.MethodGet, "/v1/nothing",
			answer{404, good("text/plain; charset=utf-8"), `404 page not found`}, "want application/problem+json"},
		{"a 404 with another code", http.MethodGet, "/v1/nothing",
			answer{404, good(problemType), `{"type":"about:blank","title":"Not Found","status":404,"code":"internal_error"}`}, "want not_found"},
		{"a 404 with a type of its own", http.MethodGet, "/v1/nothing",
			answer{404, good(problemType), `{"type":"https://example.com/not-found","title":"Not Found","status":404,"code":"not_found"}`}, "does not match the schema Problem"},
		{"an error that says another status", http.MethodGet, "/v1/nothing",
			answer{404, good(problemType), `{"type":"about:blank","title":"Not Found","status":400,"code":"not_found"}`}, "the error says status 400"},
		{"a method the path does not have, answered 404", http.MethodPost, "/v1/health",
			answer{404, good(problemType), notFound}, "so the answer is 405"},
		{"a 405 that does not name the methods", http.MethodPost, "/v1/health",
			answer{405, good(problemType), `{"type":"about:blank","title":"Method Not Allowed","status":405,"code":"method_not_allowed"}`}, "names the methods of the path in Allow"},
		{"a HEAD to a path nobody has", http.MethodHead, "/v1/nothing",
			answer{200, good(jsonType), ``}, "so the answer is 404"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			found := failures(t, c.method, c.path, c.sent)
			if !strings.Contains(strings.Join(found, "\n"), c.want) {
				t.Errorf("the client found %v, want it to say %q", found, c.want)
			}
		})
	}
}
