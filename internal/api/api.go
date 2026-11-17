// Package api serves the management API. The contract in
// contract/openapi.yaml is the source of truth: it says which operations
// exist, the server routes exactly those, and every request is validated
// against it before the operation runs.
package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"

	"github.com/majimtor/corinto/contract"
	"github.com/majimtor/corinto/internal/limits"
)

// unmatched is the pattern of the handler that answers what no operation
// matches.
const unmatched = "/"

// New returns the handler of the API: the operations of the contract, each
// behind the validation of its requests.
func New(ctx context.Context, logger *slog.Logger) (http.Handler, error) {
	document, err := contract.Load(ctx)
	if err != nil {
		return nil, err
	}
	written, err := contract.Document()
	if err != nil {
		return nil, fmt.Errorf("read the contract: %w", err)
	}
	return newHandler(document, operations(written), logger)
}

// newHandler routes the operations of a document to what serves them. The
// two must name the same operations: an operation of the contract that
// nothing serves and a handler the contract does not know both stop the
// start, so the contract and the server cannot drift apart unnoticed.
func newHandler(document *openapi3.T, serving map[string]http.Handler, logger *slog.Logger) (http.Handler, error) {
	mux := http.NewServeMux()
	routed := make(map[string]bool)
	var mismatches []string
	for route, item := range document.Paths.Map() {
		for method, operation := range item.Operations() {
			id := operation.OperationID
			handler, served := serving[id]
			switch {
			case id == "":
				mismatches = append(mismatches, fmt.Sprintf("%s %s has no operation identifier", method, route))
			case routed[id]:
				mismatches = append(mismatches, fmt.Sprintf("two operations are called %q", id))
			case !served:
				mismatches = append(mismatches, fmt.Sprintf("nothing serves the operation %q of the contract", id))
			default:
				routed[id] = true
				validated := validate(&routers.Route{
					Spec: document, Path: route, PathItem: item, Method: method, Operation: operation,
				}, handler)
				if err := register(mux, method+" "+route, validated); err != nil {
					mismatches = append(mismatches, err.Error())
				}
			}
		}
	}
	for id := range serving {
		if !routed[id] {
			mismatches = append(mismatches, fmt.Sprintf("the contract has no operation %q, which a handler serves", id))
		}
	}
	if len(mismatches) > 0 {
		// The document is a map: sorted, so that the error does not change
		// from one start to the next.
		slices.Sort(mismatches)
		return nil, fmt.Errorf("the server and the contract do not match: %s", strings.Join(mismatches, "; "))
	}
	mux.Handle(unmatched, answerUnmatched(mux))
	return observe(limitBody(refuseUncleanPaths(mux)), logger), nil
}

// register adds a pattern to the mux, which panics on one it cannot route.
// Here that is a path of the contract, and it is reported with the rest.
func register(mux *http.ServeMux, pattern string, handler http.Handler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%q cannot be routed: %v", pattern, recovered)
		}
	}()
	mux.Handle(pattern, handler)
	return nil
}

// validate checks a request against its operation in the contract before
// the operation runs.
func validate(route *routers.Route, next http.Handler) http.Handler {
	options := &openapi3filter.Options{
		// Who the request acts as is resolved by this server, not by the
		// validation of the contract.
		AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		// An operation that takes no body refuses one: a client that sends
		// it expects something this operation will not do.
		RejectWhenRequestBodyNotSpecified: true,
	}
	var names []string
	for _, parameters := range []openapi3.Parameters{route.PathItem.Parameters, route.Operation.Parameters} {
		for _, parameter := range parameters {
			if parameter.Value != nil && parameter.Value.In == openapi3.ParameterInPath {
				names = append(names, parameter.Value.Name)
			}
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The mux has already matched the path: the values of its
		// parameters are read from it, so that no second router has to
		// agree with the first.
		values := make(map[string]string, len(names))
		for _, name := range names {
			values[name] = r.PathValue(name)
		}
		err := openapi3filter.ValidateRequest(r.Context(), &openapi3filter.RequestValidationInput{
			Request: r, PathParams: values, Route: route, Options: options,
		})
		if err != nil {
			writeProblem(w, http.StatusBadRequest, CodeInvalidRequest,
				"The request does not match the contract of the operation.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// answerUnmatched answers a request that matches no operation: 405 with the
// methods of the path when the path exists under another method, and 404
// otherwise. The mux is asked, method by method, so that the answer comes
// from the same routes that serve.
func answerUnmatched(mux *http.ServeMux) http.Handler {
	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var allowed []string
		for _, method := range methods {
			_, pattern := mux.Handler(&http.Request{Method: method, URL: r.URL, Host: r.Host})
			if pattern == unmatched {
				continue
			}
			allowed = append(allowed, method)
			if method == http.MethodGet {
				// The mux serves HEAD wherever it serves GET.
				allowed = append(allowed, http.MethodHead)
			}
		}
		if len(allowed) == 0 {
			writeProblem(w, http.StatusNotFound, CodeNotFound, "No operation has this path.")
			return
		}
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeProblem(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "The path has no operation with this method.")
	})
}

// refuseUncleanPaths answers 404 to a path with an empty segment, a dot
// segment or a trailing slash. The mux would redirect it to its clean form
// with a page of its own; the API has no redirects, and every answer has
// the shape of the contract.
func refuseUncleanPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requested := r.URL.Path; !strings.HasPrefix(requested, "/") || path.Clean(requested) != requested {
			writeProblem(w, http.StatusNotFound, CodeNotFound, "No operation has this path.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody refuses a request that declares a body over the limit, and cuts
// off one that sends more than it declared or declared nothing.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limits.APIMaxBodyBytes {
			writeProblem(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
				"The body of a request has at most 16 kB.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limits.APIMaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// recorder remembers the status of a response for the log.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

// Unwrap gives http.ResponseController the writer underneath.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observe is the outermost layer of every request. It gives the request an
// identifier, sets the headers every response carries, turns a panic into
// the error of the contract and logs one line.
func observe(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		start := time.Now()
		// Generated here and never taken from the client: a value a client
		// chose could not be trusted to name one request in the log.
		id := rand.Text()
		header := w.Header()
		header.Set("X-Request-Id", id)
		header.Set("X-Content-Type-Options", "nosniff")
		// What the API answers belongs to a workspace at a moment: nothing
		// between the server and the client keeps it.
		header.Set("Cache-Control", "no-store")

		response := &recorder{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(ctx, "an operation panicked",
					"request", id, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
				if response.status == 0 {
					writeProblem(response, http.StatusInternalServerError, CodeInternalError, "")
				}
			}
			// The pattern of the route, never the path: a path carries
			// identifiers, and a query string is not logged at all.
			route := r.Pattern
			if route == unmatched {
				route = ""
			}
			logger.InfoContext(ctx, "request",
				"request", id, "method", r.Method, "route", route,
				"status", response.status, "duration", time.Since(start).String())
		}()
		next.ServeHTTP(response, r)
	})
}
