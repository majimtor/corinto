package api

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
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
	CodeUnsupportedMedia Code = "unsupported_media_type"
	CodeAliasTaken       Code = "alias_taken"
	CodeQuotaExceeded    Code = "quota_exceeded"
)

// codes returns every code, in the order of the contract.
func codes() []Code {
	return []Code{
		CodeInvalidRequest, CodeNotFound, CodeMethodNotAllowed, CodePayloadTooLarge, CodeInternalError,
		CodeUnauthenticated, CodeForbidden, CodeRateLimited, CodeUnavailable,
		CodeUnsupportedMedia, CodeAliasTaken, CodeQuotaExceeded,
	}
}

// Reason is why one value of a request was rejected. The values are the
// enumeration of the contract, and a test keeps the two lists equal.
type Reason string

// The reasons a value is rejected for. The first seven are what the
// contract itself can say of a value; the rest are rules of a link.
const (
	ReasonRequired       Reason = "required"
	ReasonUnknown        Reason = "unknown"
	ReasonInvalid        Reason = "invalid"
	ReasonTooShort       Reason = "too_short"
	ReasonTooLong        Reason = "too_long"
	ReasonTooMany        Reason = "too_many"
	ReasonDuplicate      Reason = "duplicate"
	ReasonCharacters     Reason = "characters"
	ReasonScheme         Reason = "scheme"
	ReasonCredentials    Reason = "credentials"
	ReasonHost           Reason = "host"
	ReasonPort           Reason = "port"
	ReasonDotSegment     Reason = "dot_segment"
	ReasonRedirectDomain Reason = "redirect_domain"
	ReasonNotAllowed     Reason = "not_allowed"
	ReasonReserved       Reason = "reserved"
	ReasonNotPermitted   Reason = "not_permitted"
	ReasonConflict       Reason = "conflict"
	ReasonInThePast      Reason = "in_the_past"
)

// reasons returns every reason, in the order of the contract.
func reasons() []Reason {
	return []Reason{
		ReasonRequired, ReasonUnknown, ReasonInvalid, ReasonTooShort, ReasonTooLong, ReasonTooMany, ReasonDuplicate,
		ReasonCharacters, ReasonScheme, ReasonCredentials, ReasonHost, ReasonPort, ReasonDotSegment,
		ReasonRedirectDomain, ReasonNotAllowed, ReasonReserved, ReasonNotPermitted, ReasonConflict, ReasonInThePast,
	}
}

// rejection is one value of a request that was rejected: where it was
// sent, which one it is and why. It never carries the value.
type rejection struct {
	// In is body or query.
	In string `json:"in"`
	// Field is a JSON Pointer into the body, empty for the whole of it, or
	// the name of a query parameter.
	Field string `json:"field"`
	Code  Reason `json:"code"`
}

// inBody and inQuery are the two places a rejected value comes from.
const (
	inBody  = "body"
	inQuery = "query"
)

// problem is the one shape of an error: Problem Details (RFC 9457) with the
// code of the contract.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   Code   `json:"code"`
	// Errors has one entry per value of the request that was rejected.
	Errors []rejection `json:"errors,omitempty"`
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

// writeRejected answers that the content of a request was rejected, with
// one entry per value, in a fixed order and none twice.
func writeRejected(w http.ResponseWriter, rejected ...rejection) {
	slices.SortFunc(rejected, func(a, b rejection) int {
		return cmp.Or(cmp.Compare(a.In, b.In), cmp.Compare(a.Field, b.Field), cmp.Compare(a.Code, b.Code))
	})
	write(w, http.StatusBadRequest, "application/problem+json", problem{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusBadRequest),
		Status: http.StatusBadRequest,
		Detail: "The request does not match the contract of the operation.",
		Code:   CodeInvalidRequest,
		Errors: slices.Compact(rejected),
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
