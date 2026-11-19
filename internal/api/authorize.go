package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/limits"
)

// scheme is the name the contract gives to the API key among its security
// schemes.
const scheme = "apiKey"

// Authenticator resolves the credential of a request to who it acts as. In
// the process it is *auth.Keys; it is an interface so that the tests of
// this package can decide what a credential resolves to.
type Authenticator interface {
	Authenticate(ctx context.Context, presented string) (auth.Principal, error)
}

// requirement is what an operation asks of a request before it runs, as
// the contract states it.
type requirement struct {
	// key says that the operation needs a valid key.
	key bool
	// scope is what the key must hold, or empty when any valid key will do.
	scope auth.Scope
}

// requirementOf reads what an operation asks for from the contract, which
// is the only place where it is written. An operation has to say it: none
// is open, or closed, by omission.
func requirementOf(operation *openapi3.Operation) (requirement, error) {
	if operation.Security == nil {
		return requirement{}, errors.New("does not say whether it needs a key")
	}
	alternatives := *operation.Security
	if len(alternatives) == 0 {
		return requirement{}, nil
	}
	scopes, named := alternatives[0][scheme]
	if len(alternatives) != 1 || len(alternatives[0]) != 1 || !named || len(scopes) > 1 {
		return requirement{}, fmt.Errorf("asks for something other than the scheme %q with at most one scope", scheme)
	}
	if len(scopes) == 0 {
		return requirement{key: true}, nil
	}
	scope := auth.Scope(scopes[0])
	if !slices.Contains(auth.Scopes(), scope) {
		return requirement{}, fmt.Errorf("asks for the scope %q, which does not exist", scope)
	}
	return requirement{key: true, scope: scope}, nil
}

// gate is the one point where a request is told who it acts as and whether
// it may go on. Every operation that needs a key passes through it.
type gate struct {
	keys      Authenticator
	trusted   []netip.Prefix
	addresses *limits.Addresses
	// failures counts the requests without a valid key of each client
	// address; requests and writes count what each key does.
	failures, requests, writes *limits.Limiter
}

func newGate(ctx context.Context, keys Authenticator, trusted []netip.Prefix, now func() time.Time) (*gate, error) {
	hashed, err := limits.NewAddresses()
	if err != nil {
		return nil, err
	}
	g := &gate{
		keys: keys, trusted: trusted, addresses: hashed,
		failures: perMinute(limits.APIFailedAuthenticationsPerMinute, now),
		requests: perMinute(limits.APIRequestsPerMinute, now),
		writes:   perMinute(limits.APIWritesPerMinute, now),
	}
	for _, counter := range []*limits.Limiter{g.failures, g.requests, g.writes} {
		go counter.Sweep(ctx)
	}
	return g, nil
}

// perMinute returns a limiter for one of the limits of the API, which are
// all a number of events in a minute with the same number at once.
func perMinute(events int, now func() time.Time) *limits.Limiter {
	return limits.NewLimiter(events, time.Minute, events, now)
}

// principalKey is the key under which the principal of a request travels in
// its context.
type principalKey struct{}

// principalOf returns who a request acts as. Only an operation behind the
// gate has one.
func principalOf(ctx context.Context) auth.Principal {
	principal, _ := ctx.Value(principalKey{}).(auth.Principal)
	return principal
}

// guard puts an operation behind the gate.
func (g *gate) guard(asked requirement, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// An address that has just failed too often is refused before its
		// key is looked at, valid or not: otherwise the limit would only
		// count the guesses and still answer each one.
		client := g.addresses.Key(limits.ClientAddress(r, g.trusted))
		if wait := g.failures.Wait(client); wait > 0 {
			tooMany(w, wait)
			return
		}

		principal, err := auth.Principal{}, auth.ErrInvalidKey
		if credential, sent := bearer(r.Header.Get("Authorization")); sent {
			principal, err = g.keys.Authenticate(ctx, credential)
		}
		switch {
		case errors.Is(err, auth.ErrInvalidKey):
			g.failures.Take(client)
			// No key, a malformed one, one that does not exist, a wrong
			// secret, a revoked key, an expired one.
			unauthenticated(w)
			return
		case err != nil:
			// The keys could not be read. It is not counted against the
			// address: nothing says that its key is not valid.
			fail(w, r, err)
			return
		}
		noteOf(ctx).key = principal.KeyID

		if wait := g.requests.Take(principal.KeyID); wait > 0 {
			tooMany(w, wait)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if wait := g.writes.Take(principal.KeyID); wait > 0 {
				tooMany(w, wait)
				return
			}
		}
		if asked.scope != "" && !principal.Allows(asked.scope) {
			writeProblem(w, http.StatusForbidden, CodeForbidden, "The key does not hold the scope "+string(asked.scope)+".")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, principalKey{}, principal)))
	})
}

// bearer returns the credential of an Authorization header: what follows
// the scheme Bearer, whose name is not case-sensitive, and one space.
func bearer(header string) (string, bool) {
	name, credential, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(name, "Bearer") || credential == "" {
		return "", false
	}
	return credential, true
}

// tooMany answers that a limit was reached, with the whole seconds after
// which the next request has room.
func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	writeProblem(w, http.StatusTooManyRequests, CodeRateLimited, "Too many requests in a minute.")
}
