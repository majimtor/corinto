package api

import (
	"encoding/json"
	"net/http"
)

// Code is the stable identifier of an error: what a client goes by. The
// values are the enumeration of the contract, and a test keeps the two
// lists equal.
type Code string

// The codes of the errors this server answers with.
const (
	CodeInvalidRequest   Code = "invalid_request"
	CodeNotFound         Code = "not_found"
	CodeMethodNotAllowed Code = "method_not_allowed"
	CodePayloadTooLarge  Code = "payload_too_large"
	CodeInternalError    Code = "internal_error"
	CodeUnauthenticated  Code = "unauthenticated"
	CodeForbidden        Code = "forbidden"
	CodeRateLimited      Code = "rate_limited"
	CodeUnavailable      Code = "unavailable"
)

// codes returns every code, in the order of the contract.
func codes() []Code {
	return []Code{
		CodeInvalidRequest, CodeNotFound, CodeMethodNotAllowed, CodePayloadTooLarge, CodeInternalError,
		CodeUnauthenticated, CodeForbidden, CodeRateLimited, CodeUnavailable,
	}
}

// problem is the one shape of an error: Problem Details (RFC 9457) with the
// code of the contract.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   Code   `json:"code"`
}

// writeProblem answers with an error. Every error of the API leaves through
// here, so that none is a bare string or a shape of its own. Detail is text
// for a developer; it never repeats what the request carried.
func writeProblem(w http.ResponseWriter, status int, code Code, detail string) {
	write(w, status, "application/problem+json", problem{
		// The code identifies the error; the type says so.
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Code:   code,
	})
}

// writeJSON answers with a value of the contract.
func writeJSON(w http.ResponseWriter, status int, value any) {
	write(w, status, "application/json", value)
}

func write(w http.ResponseWriter, status int, mediaType string, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		// A value of this package that cannot be written is a mistake in
		// the code: it stops the request, and the caller of the operation
		// answers that it failed.
		panic(err)
	}
	w.Header().Set("Content-Type", mediaType+"; charset=utf-8")
	w.WriteHeader(status)
	// A client that went away cannot be told so.
	_, _ = w.Write(append(body, '\n'))
}
