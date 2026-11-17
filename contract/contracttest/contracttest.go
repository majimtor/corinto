// Package contracttest gives the tests an HTTP client that checks every
// response it receives: against the contract of the operation that was
// called, and against the rules every response of the project follows. A
// test cannot forget the check, because it has no other way to call the
// server.
package contracttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"

	"github.com/majimtor/corinto/contract"
)

// Client returns a client whose every response is checked. A response that
// breaks the contract or a rule fails the test and is still returned, so
// that the test can go on and say what else is wrong.
//
// The operation of a request is found here, from the paths of the contract,
// and not with the routes of the server: a path the server routes and the
// contract does not have is one of the things this is meant to find.
func Client(t testing.TB) *http.Client {
	t.Helper()
	document, err := contract.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{
		Transport: &checked{t: t, document: document, next: transport},
		// The API has no redirects. One would be a response to check, not
		// to follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type checked struct {
	t        testing.TB
	document *openapi3.T
	next     http.RoundTripper
}

// errNoPath and errNoMethod say why a request has no operation.
var (
	errNoPath   = errors.New("the contract has no such path")
	errNoMethod = errors.New("the contract has the path under another method")
)

// operation finds the operation of the contract that a request calls, with
// the values of its path parameters. A path of the contract is matched
// segment by segment and exactly: a trailing slash, an empty segment or an
// encoded slash make another path. The router that comes with the library
// that reads the contract takes all three for the same path, which is why
// it is not used here.
func (c *checked) operation(method string, requested *url.URL) (*routers.Route, map[string]string, error) {
	segments := strings.Split(requested.EscapedPath(), "/")
	for i, segment := range segments {
		unescaped, err := url.PathUnescape(segment)
		if err != nil {
			return nil, nil, errNoPath
		}
		segments[i] = unescaped
	}
	for path, item := range c.document.Paths.Map() {
		template := strings.Split(path, "/")
		if len(template) != len(segments) {
			continue
		}
		values := make(map[string]string)
		matches := true
		for i, part := range template {
			name, isParameter := strings.CutPrefix(part, "{")
			switch {
			case isParameter && segments[i] != "":
				values[strings.TrimSuffix(name, "}")] = segments[i]
			case part != segments[i] || isParameter:
				matches = false
			}
		}
		if !matches {
			continue
		}
		found := item.GetOperation(method)
		if found == nil {
			return nil, nil, errNoMethod
		}
		return &routers.Route{Spec: c.document, Path: path, PathItem: item, Method: method, Operation: found}, values, nil
	}
	return nil, nil, errNoPath
}

func (c *checked) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := c.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))

	for _, broken := range c.check(request, response, body) {
		c.t.Errorf("%s %s answered %d: %s", request.Method, request.URL.Path, response.StatusCode, broken)
	}
	return response, nil
}

// check returns what is wrong with a response.
func (c *checked) check(request *http.Request, response *http.Response, body []byte) []string {
	broken := rules(response, body)

	// The server answers HEAD wherever the contract has GET, without a body.
	method := request.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	route, parameters, err := c.operation(method, request.URL)
	general := response.StatusCode == http.StatusRequestEntityTooLarge
	switch {
	case general:
		// Answered before any operation is looked for.
		broken = append(broken, c.problem(response, body, "payload_too_large")...)
	case err == nil && request.Method == http.MethodHead:
		if len(body) > 0 {
			broken = append(broken, "a response to HEAD has a body")
		}
	case err == nil:
		err := openapi3filter.ValidateResponse(request.Context(), &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: request, PathParams: parameters, Route: route,
			},
			Status: response.StatusCode,
			Header: response.Header,
			Body:   io.NopCloser(bytes.NewReader(body)),
			// A status the operation does not document is a failure.
			Options: &openapi3filter.Options{IncludeResponseStatus: true},
		})
		if err != nil {
			broken = append(broken, fmt.Sprintf("the response does not match the contract of %s: %v", route.Operation.OperationID, err))
		}
	case errors.Is(err, errNoMethod):
		if response.StatusCode != http.StatusMethodNotAllowed {
			broken = append(broken, "the contract has this path under another method only, so the answer is 405")
		}
		if response.Header.Get("Allow") == "" {
			broken = append(broken, "a 405 names the methods of the path in Allow")
		}
		broken = append(broken, c.problem(response, body, "method_not_allowed")...)
	default:
		if response.StatusCode != http.StatusNotFound {
			broken = append(broken, "the contract has no operation with this path, so the answer is 404")
		}
		broken = append(broken, c.problem(response, body, "not_found")...)
	}
	return broken
}

// rules checks what every response carries, whatever the operation
// (docs/engineering-rules.md, "Security headers and CI gates").
func rules(response *http.Response, body []byte) []string {
	var broken []string
	if response.Header.Get("X-Content-Type-Options") != "nosniff" {
		broken = append(broken, "X-Content-Type-Options is not nosniff")
	}
	if response.Header.Get("X-Request-Id") == "" {
		broken = append(broken, "X-Request-Id is missing")
	}
	for name := range response.Header {
		// No browser calls the API from another origin.
		if strings.HasPrefix(name, "Access-Control-") {
			broken = append(broken, name+" is a cross-origin header")
		}
	}
	if len(body) > 0 {
		_, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil || !strings.EqualFold(parameters["charset"], "utf-8") {
			broken = append(broken, fmt.Sprintf("a body is sent as %q, without its character set", response.Header.Get("Content-Type")))
		}
	}
	return broken
}

// problem checks an error that is not the error of one operation: it has
// the shape of the contract and the code of its cause.
func (c *checked) problem(response *http.Response, body []byte, code string) []string {
	var broken []string
	if mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type")); mediaType != "application/problem+json" {
		broken = append(broken, fmt.Sprintf("an error is sent as %q, want application/problem+json", mediaType))
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return append(broken, fmt.Sprintf("the error is not a JSON object: %v", err))
	}
	schema := c.document.Components.Schemas["Problem"]
	if schema == nil || schema.Value == nil {
		return append(broken, "the contract has no schema called Problem")
	}
	if err := schema.Value.VisitJSON(value, openapi3.EnableJSONSchema2020(), openapi3.MultiErrors()); err != nil {
		broken = append(broken, fmt.Sprintf("the error does not match the schema Problem: %v", err))
	}
	if value["code"] != code {
		broken = append(broken, fmt.Sprintf("the code of the error is %v, want %s", value["code"], code))
	}
	if status, _ := value["status"].(float64); int(status) != response.StatusCode {
		broken = append(broken, fmt.Sprintf("the error says status %v", value["status"]))
	}
	return broken
}
