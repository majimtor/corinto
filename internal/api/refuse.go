package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/majimtor/corinto/internal/link"
)

// rejectedBy says, for each rule of a link, which value of the request
// breaks it and why. Together with refuse it is the one place where what
// the domain refuses becomes an error of the contract.
var rejectedBy = []struct {
	rule  error
	value rejection
}{
	{link.ErrDestinationTooLong, rejection{inBody, "/destination", ReasonTooLong}},
	{link.ErrDestinationCharacters, rejection{inBody, "/destination", ReasonCharacters}},
	{link.ErrDestinationScheme, rejection{inBody, "/destination", ReasonScheme}},
	{link.ErrDestinationCredentials, rejection{inBody, "/destination", ReasonCredentials}},
	{link.ErrDestinationHost, rejection{inBody, "/destination", ReasonHost}},
	{link.ErrDestinationPort, rejection{inBody, "/destination", ReasonPort}},
	{link.ErrDestinationDotSegment, rejection{inBody, "/destination", ReasonDotSegment}},
	{link.ErrDestinationRedirects, rejection{inBody, "/destination", ReasonRedirectDomain}},
	{link.ErrDestinationNotAllowed, rejection{inBody, "/destination", ReasonNotAllowed}},
	{link.ErrAliasLength, rejection{inBody, "/alias", ReasonInvalid}},
	{link.ErrAliasCharacters, rejection{inBody, "/alias", ReasonInvalid}},
	{link.ErrAliasReserved, rejection{inBody, "/alias", ReasonReserved}},
	{link.ErrAliasNotPermitted, rejection{inBody, "/alias", ReasonNotPermitted}},
	{link.ErrCampaignValue, rejection{inBody, "/campaign", ReasonCharacters}},
	{link.ErrCampaignConflict, rejection{inBody, "/campaign", ReasonConflict}},
	{link.ErrTitle, rejection{inBody, "/title", ReasonCharacters}},
	{link.ErrTags, rejection{inBody, "/tags", ReasonCharacters}},
}

// refuse answers a request that the domain refused.
func refuse(w http.ResponseWriter, r *http.Request, err error) {
	for _, broken := range rejectedBy {
		if errors.Is(err, broken.rule) {
			writeRejected(w, broken.value)
			return
		}
	}
	switch {
	case errors.Is(err, link.ErrNotFound):
		// Of another workspace, deleted or never there: one answer.
		writeProblem(w, http.StatusNotFound, CodeNotFound, "The workspace has no such link.")
	case errors.Is(err, link.ErrAliasTaken):
		writeProblem(w, http.StatusConflict, CodeAliasTaken, "The alias is already in use.")
	case errors.Is(err, link.ErrQuotaExceeded):
		writeProblem(w, http.StatusConflict, CodeQuotaExceeded, "The workspace holds as many links as its quota allows.")
	case errors.Is(err, link.ErrWorkspaceBlocked), errors.Is(err, link.ErrWorkspaceNotFound):
		// The workspace was blocked after the key was resolved, and its
		// keys were revoked with it: the next request would say this.
		unauthenticated(w)
	default:
		fail(w, r, err)
	}
}

// unauthenticated answers a request without a valid key. One answer for
// every cause.
func unauthenticated(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeProblem(w, http.StatusUnauthorized, CodeUnauthenticated,
		"The request needs a valid API key in the Authorization header.")
}

// fail answers that the server could not do what was asked for a reason of
// its own: 503 when the database did not answer, as
// docs/delivery-semantics.md states, and 500 for anything else. The reason
// is left for the log line of the request and is not told to the client.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	noteOf(r.Context()).failure = err
	if away(err) {
		writeProblem(w, http.StatusServiceUnavailable, CodeUnavailable, "The API cannot reach its database.")
		return
	}
	writeProblem(w, http.StatusInternalServerError, CodeInternalError, "")
}

// away reports whether an error means that the database did not answer:
// it could not be reached, the connection broke, the wait ran out, or it
// answered that it is not taking work.
func away(err error) bool {
	var (
		connecting *pgconn.ConnectError
		network    net.Error
		answered   *pgconn.PgError
	)
	switch {
	case errors.As(err, &connecting), errors.As(err, &network):
		return true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return true
	case errors.As(err, &answered):
		// Classes of PostgreSQL error codes: 08 is a connection failure, 53
		// is out of resources, such as connections, and 57P is the server
		// stopping or not yet started.
		return strings.HasPrefix(answered.Code, "08") || strings.HasPrefix(answered.Code, "53") ||
			strings.HasPrefix(answered.Code, "57P")
	}
	return false
}
