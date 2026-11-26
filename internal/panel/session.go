package panel

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
)

// CookieForm is the form of the session cookie. An installation chooses
// it: it is not read from the connection, because TLS usually ends at a
// proxy and the process sees plain HTTP either way (ADR 0012).
type CookieForm string

const (
	// CookieSecure is the cookie of every installation a browser reaches
	// over HTTPS: `__Host-session`, with Secure.
	CookieSecure CookieForm = "secure"
	// CookieLoopback is the cookie of the local run alone, which serves
	// plain HTTP on a loopback address: without the prefix and without
	// Secure, because one browser engine discards a Secure cookie there.
	// It is issued only to a request whose host is a loopback name.
	CookieLoopback CookieForm = "loopback"
)

// The name of the cookie in each form. The prefix makes a browser refuse
// the cookie unless it is Secure, has no domain and covers every path.
const (
	cookieSecureName   = "__Host-session"
	cookieLoopbackName = "session"
)

// ParseCookieForm reads the form of the session cookie as the environment
// writes it. Nothing written is the form with Secure: the weaker one has
// to be asked for.
func ParseCookieForm(written string) (CookieForm, error) {
	switch form := CookieForm(written); form {
	case "":
		return CookieSecure, nil
	case CookieSecure, CookieLoopback:
		return form, nil
	}
	return "", fmt.Errorf("%q is not a form of the session cookie: it is %q or %q", written, CookieSecure, CookieLoopback)
}

func (p *panel) cookieName() string {
	if p.settings.Cookie == CookieLoopback {
		return cookieLoopbackName
	}
	return cookieSecureName
}

// cookie returns the session cookie with a token, or the one that takes it
// away when the token is empty. It names no domain, so it goes back to the
// host that set it and to no other; a script cannot read it; and it is not
// sent with a request another site makes, but for a link that is followed
// to the panel. It has no lifetime of its own: it goes when the browser is
// closed, and the session ends on the server whatever the cookie does.
func (p *panel) cookie(token string) *http.Cookie {
	cookie := &http.Cookie{ //nolint:gosec // Without Secure in the local run alone, by ADR 0012: enter issues that form to loopback hosts only.
		Name: p.cookieName(), Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: p.settings.Cookie == CookieSecure,
	}
	if token == "" {
		cookie.MaxAge = -1
	}
	return cookie
}

// presented returns the token a request brought, or nothing.
func (p *panel) presented(r *http.Request) string {
	cookie, err := r.Cookie(p.cookieName())
	if err != nil {
		return ""
	}
	return cookie.Value
}

// identify returns who a request acts as, and whether it acts as anybody.
// The cookie is read here and nowhere else in the process: the API
// authenticates by key and never looks at it.
func (p *panel) identify(r *http.Request) (auth.Principal, bool, error) {
	token := p.presented(r)
	if token == "" {
		return auth.Principal{}, false, nil
	}
	principal, err := p.settings.Sessions.Authenticate(r.Context(), token)
	if errors.Is(err, auth.ErrInvalidSession) {
		return auth.Principal{}, false, nil
	}
	if err != nil {
		return auth.Principal{}, false, err
	}
	noteOf(r.Context()).session = principal.SessionID
	return principal, true, nil
}

// member is who a page behind a session is rendered for.
type member struct {
	principal auth.Principal
	workspace link.Workspace
}

// inside puts a page behind a session. A request without one is sent to
// the door of the panel, in the language it asked in.
func (p *panel) inside(lang language, page func(http.ResponseWriter, *http.Request, member)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, in, err := p.identify(r)
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		if !in {
			redirect(w, "/"+string(lang)+"/", http.StatusSeeOther)
			return
		}
		// A session of a workspace that is gone or blocked does not exist:
		// whatever ends the workspace ends its sessions with it.
		workspace, err := p.settings.Links.Workspace(r.Context(), principal.WorkspaceID)
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		page(w, r, member{principal: principal, workspace: workspace})
	}
}

// viewIn returns what a page behind a session is rendered with.
func (p *panel) viewIn(r *http.Request, lang language, in member) view {
	shown := p.viewOf(r, lang)
	shown.Workspace = in.workspace.Name
	return shown
}

// enter is the demo entry: it opens a session in a demo workspace for
// whoever asks, without a secret, and sends them to its links.
func (p *panel) enter(lang language) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// The cookie without Secure exists for the local run alone: to any
		// other host it is not issued, and so nobody enters.
		if p.settings.Cookie == CookieLoopback && !loopback(r.Host) {
			p.render(w, r, http.StatusForbidden, lang, pageInsecure, p.viewOf(r, lang))
			return
		}
		// Every attempt counts, the ones that open nothing too: what the
		// limit protects is the database behind the entry.
		client := p.addresses.Key(limits.ClientAddress(r, p.settings.TrustedProxies))
		if wait := p.entries.Take(client); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			p.render(w, r, http.StatusTooManyRequests, lang, pageLimited, p.viewOf(r, lang))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limits.APIMaxBodyBytes)
		if err := r.ParseForm(); err != nil {
			// A form no page of the panel sends names no workspace.
			p.missing(w, r, lang)
			return
		}

		_, token, err := p.settings.Sessions.Open(ctx, r.PostForm.Get("workspace"))
		switch {
		case errors.Is(err, link.ErrWorkspaceNotFound), errors.Is(err, link.ErrWorkspaceBlocked), errors.Is(err, auth.ErrNotDemo):
			// Not a workspace that is entered this way: for whoever asks,
			// it does not exist.
			p.missing(w, r, lang)
			return
		case err != nil:
			p.fail(w, r, lang, err)
			return
		}
		// A new session on each entry: the one the browser came with ends
		// here, and its token opens nothing from now on.
		if before := p.presented(r); before != "" {
			if err := p.settings.Sessions.Close(ctx, before); err != nil {
				p.fail(w, r, lang, err)
				return
			}
		}
		http.SetCookie(w, p.cookie(token))
		redirect(w, "/"+string(lang)+"/links", http.StatusSeeOther)
	}
}

// leave closes the session of a request, on the server and in the browser,
// and sends whoever asked back to the door. Leaving without a session is
// not an error.
func (p *panel) leave(lang language) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token := p.presented(r); token != "" {
			if err := p.settings.Sessions.Close(r.Context(), token); err != nil {
				p.fail(w, r, lang, err)
				return
			}
		}
		http.SetCookie(w, p.cookie(""))
		// What the browser may still hold of the panel: its cache, where a
		// page could wait for the button that goes back, and its storage,
		// which the panel does not use. The cookie is taken away by name
		// above and not with the directive for cookies, which would also
		// take away the ones of every other host under the same domain.
		w.Header().Set("Clear-Site-Data", `"cache", "storage"`)
		redirect(w, "/"+string(lang)+"/", http.StatusSeeOther)
	}
}

// loopback reports whether a host, as a request names it, is a loopback
// name: localhost, or an address of the loopback network.
func loopback(host string) bool {
	name := host
	if split, port, err := net.SplitHostPort(host); err == nil {
		// What follows the last colon is a port or the host is not one a
		// browser sends.
		if port == "" || strings.Trim(port, "0123456789") != "" {
			return false
		}
		name = split
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(name)
	return err == nil && address.Unmap().IsLoopback()
}
