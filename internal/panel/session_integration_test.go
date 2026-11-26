//go:build integration

package panel

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
)

// named creates a workspace with a name and returns its identifier.
func (s *stage) named(t *testing.T, name string, demo bool) string {
	t.Helper()
	created, err := link.CreateWorkspace(t.Context(), s.pool, link.WorkspaceDraft{Name: name, Demo: demo, MaxLinks: 100, MaxKeys: 10})
	if err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// block blocks a workspace, as the operator does.
func (s *stage) block(t *testing.T, workspaceID string) {
	t.Helper()
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, _, err := link.BlockWorkspace(t.Context(), tx, workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CloseSessions(t.Context(), tx, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// issued returns the session cookie an answer sets, or nil when it sets
// none.
func issued(t *testing.T, answer *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cookies := answer.Result().Cookies()
	if len(cookies) > 1 {
		t.Fatalf("an answer sets %d cookies: the panel has one", len(cookies))
	}
	if len(cookies) == 0 {
		return nil
	}
	return cookies[0]
}

// sentTo fails unless an answer sends its request to a path of the panel,
// with no body.
func sentTo(t *testing.T, what string, answer *httptest.ResponseRecorder, path string) {
	t.Helper()
	if answer.Code != http.StatusSeeOther || answer.Header().Get("Location") != path || answer.Body.Len() != 0 {
		t.Errorf("%s = %d to %q with %d bytes, want %d to %q with none",
			what, answer.Code, answer.Header().Get("Location"), answer.Body.Len(), http.StatusSeeOther, path)
	}
}

// shows fails unless an answer is a page with a status and a heading, which
// is a text of the catalogue with its values.
func (s *stage) shows(t *testing.T, what string, answer *httptest.ResponseRecorder, status int, lang language, heading string, pairs ...any) {
	t.Helper()
	if want := "<h1>" + s.text(t, lang, heading, pairs...) + "</h1>"; answer.Code != status || !strings.Contains(answer.Body.String(), want) {
		t.Errorf("%s = %d, want %d with the page that says %q", what, answer.Code, status, want)
	}
}

// The door of the panel: the demo workspaces that can be entered, each with
// a form that changes something and so is a post.
func TestTheDoorOffersTheDemoWorkspacesInBothLanguages(t *testing.T) {
	s := newStage(t)
	for _, lang := range languages {
		answer := s.get(t, "", "/"+string(lang)+"/")
		s.shows(t, "the door with no demo workspace", answer, http.StatusOK, lang, "entry.title")
		if page := answer.Body.String(); !strings.Contains(page, s.text(t, lang, "entry.none")) || strings.Contains(page, "<form") {
			t.Errorf("the door in %q with no demo workspace does not say so, or offers a form", lang)
		}
	}

	first, second := s.named(t, "Acme <Books>", true), s.named(t, "Globex", true)
	ordinary, closed := s.named(t, "Harbor Bakery", false), s.named(t, "Summit Outdoors", true)
	s.block(t, closed)
	for _, lang := range languages {
		answer := s.get(t, "", "/"+string(lang)+"/")
		page := answer.Body.String()
		s.shows(t, "the door", answer, http.StatusOK, lang, "entry.title")
		for what, want := range map[string]string{
			"its language":                `<html lang="` + string(lang) + `">`,
			"where its forms are sent":    `<form method="post" action="/` + string(lang) + `/session">`,
			"the first demo workspace":    `<input type="hidden" name="workspace" value="` + first + `">`,
			"its name, as text":           s.text(t, lang, "entry.enter", "name", "Acme <Books>"),
			"the second demo workspace":   `<input type="hidden" name="workspace" value="` + second + `">`,
			"what entering leaves behind": s.text(t, lang, "entry.cookie"),
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the door in %q does not show %s: no %q in it", lang, what, want)
			}
		}
		if strings.Contains(page, ordinary) || strings.Contains(page, "Harbor Bakery") || strings.Contains(page, closed) || strings.Contains(page, "Summit Outdoors") {
			t.Errorf("the door in %q offers a workspace that is not a demo, or a blocked one", lang)
		}
		// The oldest first, and nobody is inside: no bar of a session.
		if strings.Index(page, first) > strings.Index(page, second) || strings.Count(page, "<form") != 2 || strings.Contains(page, "/logout") {
			t.Errorf("the door in %q does not offer the two workspaces in the order they were created, and nothing else", lang)
		}
	}
}

// PLAN.md section 9: the session travels in a host-only cookie that a
// script cannot read, `__Host-session` with Secure, with SameSite=Lax.
func TestEnteringOpensASessionAndLeadsToTheLinks(t *testing.T) {
	s := newStage(t)
	entered := s.named(t, "Acme", true)
	target := s.link(t, entered, link.Draft{Title: "Spring reading list"})

	answer := s.ask(t, http.MethodPost, "/en/session", naming(entered), saying("Sec-Fetch-Site", "same-origin"))
	sentTo(t, "entering", answer, "/en/links")
	cookie := issued(t, answer)
	if cookie == nil {
		t.Fatal("entering set no cookie")
	}
	if cookie.Name != "__Host-session" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode ||
		cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
		t.Errorf("the cookie is %+v; want __Host-session with Secure, HttpOnly, SameSite=Lax and Path=/, no domain and no lifetime", cookie)
	}
	if !regexp.MustCompile(`^[a-z2-7]{52}$`).MatchString(cookie.Value) || s.held(t) != 1 {
		t.Errorf("the cookie does not hold a token (%d characters), or the database holds %d sessions", len(cookie.Value), s.held(t))
	}
	principal, err := s.sessions.Authenticate(t.Context(), cookie.Value)
	if err != nil || principal.WorkspaceID != entered {
		t.Fatalf("the token of the cookie resolves to the workspace %q, %v; want %q", principal.WorkspaceID, err, entered)
	}

	// Inside: the links of the workspace, its name in the bar and the way
	// out, which is a form.
	list := s.ask(t, http.MethodGet, "/en/links", with(cookie))
	s.shows(t, "the links after entering", list, http.StatusOK, english, "links.title")
	for what, want := range map[string]string{
		"the workspace":         `<p class="workspace">Acme</p>`,
		"the way out":           `<form method="post" action="/en/logout"><button type="submit">` + s.text(t, english, "layout.leave") + `</button></form>`,
		"the link, by its name": `<a href="/en/links/` + target.ID + `">Spring reading list</a>`,
		"its short link":        `<td class="short">` + redirectDomain + "/" + target.Identifier + `</td>`,
		"its state":             "<td>" + s.text(t, english, "link.state.active") + "</td>",
	} {
		if !strings.Contains(list.Body.String(), want) {
			t.Errorf("the list does not show %s: no %q in it", what, want)
		}
	}
	// The page of the link carries the same bar, and the door has nothing
	// to offer whoever is inside.
	if page := s.ask(t, http.MethodGet, "/es/links/"+target.ID, with(cookie)); page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), `<p class="workspace">Acme</p>`) || !strings.Contains(page.Body.String(), `action="/es/logout"`) {
		t.Errorf("the page of a link after entering = %d, or without the bar of the session", page.Code)
	}
	sentTo(t, "the door for whoever is inside", s.ask(t, http.MethodGet, "/es/", with(cookie)), "/es/links")
}

// The form of the cookie without Secure exists for the local run alone
// (the engineering rules): asked for under a host name that is not
// loopback, it is refused.
func TestTheCookieOfTheLocalRunIsIssuedToLoopbackHostsAlone(t *testing.T) {
	s := newStageWith(t, CookieLoopback)
	entered := s.named(t, "Acme", true)

	for _, host := range []string{"example.com", "panel.example.com:8080", "localhost.example.com", "192.0.2.10:8080", "[2001:db8::1]:8080"} {
		answer := s.ask(t, http.MethodPost, "/es/session", naming(entered), to(host))
		s.shows(t, "an entry under the host "+host, answer, http.StatusForbidden, spanish, "insecure.title")
		if issued(t, answer) != nil || s.held(t) != 0 {
			t.Errorf("an entry under the host %s set a cookie, or opened a session", host)
		}
	}
	for _, host := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080", "localhost"} {
		answer := s.ask(t, http.MethodPost, "/es/session", naming(entered), to(host))
		sentTo(t, "an entry under the host "+host, answer, "/es/links")
		cookie := issued(t, answer)
		if cookie == nil {
			t.Fatalf("an entry under the host %s set no cookie", host)
		}
		if cookie.Name != "session" || cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode ||
			cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge != 0 {
			t.Errorf("the cookie under the host %s is %+v; want session without Secure, with HttpOnly, SameSite=Lax and Path=/", host, cookie)
		}
		if list := s.ask(t, http.MethodGet, "/es/links", with(cookie), to(host)); list.Code != http.StatusOK {
			t.Errorf("the links with the cookie of the local run = %d", list.Code)
		}
		// Each form reads its own cookie and not the one of the other.
		other := carried("__Host-session", cookie.Value)
		sentTo(t, "the links with the token under the name of the other form", s.ask(t, http.MethodGet, "/es/links", with(other), to(host)), "/es/")
	}

	// And the form with Secure does not read the cookie of the local run.
	secure := newStage(t)
	inside := secure.sessionOf(t, secure.named(t, "Acme", true))
	if answer := secure.ask(t, http.MethodGet, "/es/links", with(inside)); answer.Code != http.StatusOK {
		t.Fatalf("the links with the cookie of the installation = %d", answer.Code)
	}
	sentTo(t, "the links with the token under the name of the local run",
		secure.ask(t, http.MethodGet, "/es/links", with(carried("session", inside.Value))), "/es/")
}

// A new token on each entry (PLAN.md section 17): whoever enters with a
// session gets another, and the first one ends.
func TestEachEntryHasASessionOfItsOwnAndEndsTheOneBefore(t *testing.T) {
	s := newStage(t)
	first, second := s.named(t, "Acme", true), s.named(t, "Globex", true)

	before := issued(t, s.ask(t, http.MethodPost, "/es/session", naming(first)))
	after := issued(t, s.ask(t, http.MethodPost, "/es/session", naming(second), with(before)))
	if before == nil || after == nil || before.Value == after.Value {
		t.Fatal("two entries did not give two tokens")
	}
	sentTo(t, "the links with the session before", s.ask(t, http.MethodGet, "/es/links", with(before)), "/es/")
	if list := s.ask(t, http.MethodGet, "/es/links", with(after)); list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `<p class="workspace">Globex</p>`) {
		t.Errorf("the links with the new session = %d, or not the ones of the workspace entered last", list.Code)
	}
	if rows := s.held(t); rows != 1 {
		t.Errorf("the database holds %d sessions, want the new one alone", rows)
	}

	// An entry that opens nothing leaves the session there was.
	refused := s.ask(t, http.MethodPost, "/es/session", naming(uuid.NewV7().String()), with(after))
	s.shows(t, "an entry to a workspace that does not exist", refused, http.StatusNotFound, spanish, "notfound.title")
	if list := s.ask(t, http.MethodGet, "/es/links", with(after)); list.Code != http.StatusOK || issued(t, refused) != nil {
		t.Errorf("an entry that was refused ended the session there was, or set a cookie")
	}
}

// Only a demo workspace is entered without a secret. Any other is, for
// whoever asks, a workspace that does not exist.
func TestNothingButADemoWorkspaceIsEntered(t *testing.T) {
	s := newStage(t)
	ordinary, closed := s.named(t, "Harbor Bakery", false), s.named(t, "Summit Outdoors", true)
	s.block(t, closed)
	entered := s.named(t, "Acme", true)

	for what, form := range map[string]func(*http.Request){
		"a workspace that is not a demo":      naming(ordinary),
		"a demo workspace that is blocked":    naming(closed),
		"a workspace that does not exist":     naming(uuid.NewV7().String()),
		"something that is not an identifier": naming("acme"),
		"no workspace":                        filled(url.Values{}),
		"a form with no body":                 func(*http.Request) {},
		"a body that is not a form":           func(r *http.Request) { naming(entered)(r); r.Header.Set("Content-Type", "application/json") },
		"a workspace in the address":          func(r *http.Request) { r.URL.RawQuery = "workspace=" + entered },
		"a form that cannot be read": func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader("workspace=%zz"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		},
		"a form larger than a form is": filled(url.Values{"workspace": {entered}, "filler": {strings.Repeat("x", limits.APIMaxBodyBytes)}}),
	} {
		answer := s.ask(t, http.MethodPost, "/en/session", form, from("192.0.2."+strconv.Itoa(len(what))+":4000"))
		s.shows(t, "an entry with "+what, answer, http.StatusNotFound, english, "notfound.title")
		if issued(t, answer) != nil || s.held(t) != 0 {
			t.Errorf("an entry with %s set a cookie, or opened a session", what)
		}
	}
	// Nothing changes state on a GET: the address of the entry has no page.
	answer := s.ask(t, http.MethodGet, "/en/session?workspace="+entered)
	s.shows(t, "the entry asked for with GET", answer, http.StatusNotFound, english, "notfound.title")
	if issued(t, answer) != nil || s.held(t) != 0 {
		t.Error("the entry asked for with GET set a cookie, or opened a session")
	}
}

// PLAN.md section 17: logging out invalidates the session on the server,
// and tells the browser to forget what it holds of the panel.
func TestLeavingClosesTheSessionOnTheServerAndInTheBrowser(t *testing.T) {
	s := newStage(t)
	entered := s.named(t, "Acme", true)
	leaving, staying := s.sessionOf(t, entered), s.sessionOf(t, entered)

	// Nothing changes state on a GET.
	s.shows(t, "the way out asked for with GET", s.ask(t, http.MethodGet, "/es/logout", with(leaving)), http.StatusNotFound, spanish, "notfound.title")
	if list := s.ask(t, http.MethodGet, "/es/links", with(leaving)); list.Code != http.StatusOK {
		t.Fatalf("after asking for the way out with GET the session gives %d", list.Code)
	}

	answer := s.ask(t, http.MethodPost, "/es/logout", with(leaving), saying("Sec-Fetch-Site", "same-origin"))
	sentTo(t, "leaving", answer, "/es/")
	gone := issued(t, answer)
	if gone == nil || gone.Name != "__Host-session" || gone.Value != "" || gone.MaxAge >= 0 || !gone.Secure || !gone.HttpOnly || gone.Path != "/" {
		t.Errorf("leaving sets the cookie %+v; want __Host-session emptied and expired, with the attributes it was set with", gone)
	}
	if got := answer.Header().Get("Clear-Site-Data"); got != `"cache", "storage"` {
		t.Errorf("Clear-Site-Data = %q", got)
	}
	// The token opens nothing any more, whatever the browser does with the
	// cookie, and the other session of the workspace is as it was.
	if _, err := s.sessions.Authenticate(t.Context(), leaving.Value); err == nil {
		t.Error("the token of the session that left still resolves")
	}
	sentTo(t, "the links with the session that left", s.ask(t, http.MethodGet, "/es/links", with(leaving)), "/es/")
	if list := s.ask(t, http.MethodGet, "/es/links", with(staying)); list.Code != http.StatusOK || s.held(t) != 1 {
		t.Errorf("the other session of the workspace gives %d, and the database holds %d sessions", list.Code, s.held(t))
	}

	// Leaving twice, or without ever entering, is not an error.
	for what, changes := range map[string][]func(*http.Request){
		"leaving again":             {with(leaving)},
		"leaving without a session": nil,
		"leaving with no token":     {with(carried("__Host-session", "acme"))},
	} {
		again := s.ask(t, http.MethodPost, "/en/logout", changes...)
		sentTo(t, what, again, "/en/")
		if cookie := issued(t, again); cookie == nil || cookie.MaxAge >= 0 || again.Header().Get("Clear-Site-Data") == "" {
			t.Errorf("%s does not tell the browser to forget the session", what)
		}
	}
	if rows := s.held(t); rows != 1 {
		t.Errorf("the database holds %d sessions, want the one that stays", rows)
	}
}

// Every non-safe request to the panel passes the cross-origin check of the
// standard library (the engineering rules): a form posted from another
// site or from a sibling subdomain is refused, and changes nothing.
func TestAFormFromAnotherOriginIsRefused(t *testing.T) {
	s := newStage(t)
	entered := s.named(t, "Acme", true)
	inside := s.sessionOf(t, entered)
	const host = "panel.example.com"

	for what, changes := range map[string][]func(*http.Request){
		"another site, as the browser says it":         {saying("Sec-Fetch-Site", "cross-site")},
		"a sibling subdomain, as the browser says it":  {saying("Sec-Fetch-Site", "same-site")},
		"another site, by its origin":                  {saying("Origin", "https://attacker.example")},
		"a sibling subdomain, by its origin":           {saying("Origin", "https://blog.example.com")},
		"the same host on another port, by its origin": {saying("Origin", "https://panel.example.com:8443")},
		"an origin that is not one":                    {saying("Origin", "null")},
	} {
		for _, path := range []string{"/es/session", "/es/logout", "/es/links", "/es/nothing"} {
			answer := s.ask(t, http.MethodPost, path, append([]func(*http.Request){to(host), with(inside), naming(entered)}, changes...)...)
			s.shows(t, "a post to "+path+" from "+what, answer, http.StatusForbidden, spanish, "refused.title")
			if issued(t, answer) != nil {
				t.Errorf("a post to %s from %s set a cookie", path, what)
			}
		}
		if list := s.ask(t, http.MethodGet, "/es/links", with(inside)); list.Code != http.StatusOK || s.held(t) != 1 {
			t.Errorf("after the posts from %s the session gives %d and the database holds %d sessions", what, list.Code, s.held(t))
		}
	}

	// From a page of the panel itself, from the address bar and from a
	// client that is not a browser, the form is taken.
	for what, changes := range map[string][]func(*http.Request){
		"the panel, as the browser says it": {saying("Sec-Fetch-Site", "same-origin")},
		"the panel, by its origin":          {saying("Origin", "https://panel.example.com")},
		"nowhere, as the browser says it":   {saying("Sec-Fetch-Site", "none")},
		"a client that is not a browser":    nil,
	} {
		answer := s.ask(t, http.MethodPost, "/es/session", append([]func(*http.Request){to(host), naming(entered), from("198.51.100." + strconv.Itoa(len(what)) + ":4000")}, changes...)...)
		sentTo(t, "an entry from "+what, answer, "/es/links")
	}
	// A page is read from anywhere: a link to the panel on another site is
	// followed, and with SameSite=Lax it brings the session.
	if list := s.ask(t, http.MethodGet, "/es/links", with(inside), saying("Sec-Fetch-Site", "cross-site")); list.Code != http.StatusOK {
		t.Errorf("the links asked for from another site = %d", list.Code)
	}
}

// PLAN.md section 9: ten sessions a minute for each client address.
func TestTheDemoEntryIsLimitedByAddress(t *testing.T) {
	if limits.DemoEntriesPerMinute != 10 {
		t.Errorf("the demo entry allows %d sessions a minute, want 10", limits.DemoEntriesPerMinute)
	}
	s := newStage(t)
	entered := s.named(t, "Acme", true)
	enter := func(address string, changes ...func(*http.Request)) *httptest.ResponseRecorder {
		return s.ask(t, http.MethodPost, "/en/session", append([]func(*http.Request){naming(entered), from(address)}, changes...)...)
	}

	for i := range limits.DemoEntriesPerMinute {
		// A port is not an address: each of these comes from another one.
		sentTo(t, "entry "+strconv.Itoa(i+1)+" of a minute", enter("192.0.2.7:"+strconv.Itoa(4000+i)), "/en/links")
	}
	limited := enter("192.0.2.7:5000")
	s.shows(t, "the entry after the tenth", limited, http.StatusTooManyRequests, english, "limited.title")
	if wait, err := strconv.Atoi(limited.Header().Get("Retry-After")); err != nil || wait < 1 || wait > 6 ||
		issued(t, limited) != nil || s.held(t) != limits.DemoEntriesPerMinute {
		t.Errorf("the entry after the tenth: Retry-After = %q, and the database holds %d sessions; want up to 6 seconds and %d",
			limited.Header().Get("Retry-After"), s.held(t), limits.DemoEntriesPerMinute)
	}
	// The address is the one of the connection: a header that names
	// another, from a client that is no proxy of the installation, is not
	// believed. Another address has its own allowance.
	s.shows(t, "the same address under a forwarding header", enter("192.0.2.7:5001", saying("X-Forwarded-For", "203.0.113.9")),
		http.StatusTooManyRequests, english, "limited.title")
	sentTo(t, "an entry from another address", enter("192.0.2.8:4000"), "/en/links")

	// An entry that opens nothing counts as one: the limit is on what
	// asks, not on what it gets.
	for range limits.DemoEntriesPerMinute {
		enter("192.0.2.9:4000", naming(uuid.NewV7().String()))
	}
	s.shows(t, "an entry after ten that opened nothing", enter("192.0.2.9:4000"), http.StatusTooManyRequests, english, "limited.title")

	// Six seconds earn one entry back, and no more.
	s.clock.advance(6 * time.Second)
	sentTo(t, "an entry six seconds later", enter("192.0.2.7:5002"), "/en/links")
	s.shows(t, "a second entry six seconds later", enter("192.0.2.7:5003"), http.StatusTooManyRequests, english, "limited.title")
	// The pages are not what is limited.
	if door := s.ask(t, http.MethodGet, "/en/", from("192.0.2.7:5004")); door.Code != http.StatusOK {
		t.Errorf("the door for an address that reached its limit = %d", door.Code)
	}
}

// A page behind a session sends whoever has none to the door, in the
// language the page was asked in.
func TestAPageBehindASessionSendsWhoeverHasNoneToTheDoor(t *testing.T) {
	s := newStage(t)
	entered := s.named(t, "Acme", true)
	target := s.link(t, entered, link.Draft{})
	aged := func(opened, seen time.Duration) *http.Cookie {
		t.Helper()
		cookie := s.sessionOf(t, entered)
		hash := sha256.Sum256([]byte(cookie.Value))
		if _, err := s.pool.Exec(t.Context(),
			`UPDATE sessions SET created_at = now() - make_interval(secs => $2),
			                     expires_at = now() - make_interval(secs => $2) + make_interval(secs => $4),
			                     last_seen_at = now() - make_interval(secs => $3)
			  WHERE token_hash = $1`, hash[:], opened.Seconds(), seen.Seconds(), auth.SessionLifetime.Seconds()); err != nil {
			t.Fatal(err)
		}
		return cookie
	}
	closed := s.sessionOf(t, entered)
	if err := s.sessions.Close(t.Context(), closed.Value); err != nil {
		t.Fatal(err)
	}
	blocked := s.named(t, "Summit Outdoors", true)
	locked := s.sessionOf(t, blocked)
	s.block(t, blocked)

	for what, changes := range map[string][]func(*http.Request){
		"no cookie":                        nil,
		"a cookie that holds no token":     {with(carried("__Host-session", "acme"))},
		"an empty cookie":                  {with(carried("__Host-session", ""))},
		"a session past its lifetime":      {with(aged(auth.SessionLifetime+time.Second, 0))},
		"a session left idle":              {with(aged(time.Hour, auth.SessionIdleFor+time.Second))},
		"a session that was closed":        {with(closed)},
		"a session of a blocked workspace": {with(locked)},
		"a key of the API and no cookie":   {saying("Authorization", "Bearer "+closed.Value)},
		"the token in the address":         {func(r *http.Request) { r.URL.RawQuery = "session=" + s.sessionOf(t, entered).Value }},
		"the token in a header of its own": {saying("X-Session", s.sessionOf(t, entered).Value)},
		"the token under another name":     {with(carried("corinto", s.sessionOf(t, entered).Value))},
		"the token after one that is not":  {with(carried("__Host-session", "acme")), with(s.sessionOf(t, entered))},
	} {
		sentTo(t, "the links in Spanish with "+what, s.ask(t, http.MethodGet, "/es/links", changes...), "/es/")
		sentTo(t, "the page of a link in English with "+what, s.ask(t, http.MethodGet, "/en/links/"+target.ID, changes...), "/en/")
	}
	// A session that is a minute short of both its times is one.
	if list := s.ask(t, http.MethodGet, "/es/links", with(aged(auth.SessionLifetime-time.Minute, auth.SessionIdleFor-time.Minute))); list.Code != http.StatusOK {
		t.Errorf("the links with a session a minute short of its times = %d", list.Code)
	}
}

// The list shows the links of the workspace of the session and of no
// other, a page of them at a time.
func TestTheListShowsTheLinksOfTheWorkspaceAndOfNoOther(t *testing.T) {
	s := newStage(t)
	owner, other := s.named(t, "Acme", true), s.named(t, "Globex", true)
	inside := s.sessionOf(t, owner)

	for _, lang := range languages {
		answer := s.ask(t, http.MethodGet, "/"+string(lang)+"/links", with(inside))
		s.shows(t, "the list of a workspace without links", answer, http.StatusOK, lang, "links.title")
		if page := answer.Body.String(); !strings.Contains(page, s.text(t, lang, "links.empty")) || strings.Contains(page, "<table") {
			t.Errorf("the list in %q of a workspace without links does not say so, or shows a table", lang)
		}
	}

	past := today.Add(-time.Hour)
	first := s.link(t, owner, link.Draft{Title: `Spring <reading> list`, Destination: "https://example.com/books?season=spring&year=2026"})
	expired := s.link(t, owner, link.Draft{ExpiresAt: &past})
	theirs := s.link(t, other, link.Draft{Title: "Autumn clearance"})
	for _, lang := range languages {
		page := s.ask(t, http.MethodGet, "/"+string(lang)+"/links", with(inside)).Body.String()
		for what, want := range map[string]string{
			"the caption of the table":            "<caption>" + s.text(t, lang, "links.caption") + "</caption>",
			"a link by its title, as text":        `<a href="/` + string(lang) + "/links/" + first.ID + `">Spring &lt;reading&gt; list</a>`,
			"its destination, as text":            `<td class="address leads">https://example.com/books?season=spring&amp;year=2026</td>`,
			"a link without a title, by its name": `<a href="/` + string(lang) + "/links/" + expired.ID + `">` + expired.Identifier + `</a>`,
			"that it has expired":                 "<td>" + s.text(t, lang, "link.state.expired") + "</td>",
			"the workspace":                       `<p class="workspace">Acme</p>`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the list in %q does not show %s: no %q in it", lang, what, want)
			}
		}
		if strings.Contains(page, theirs.ID) || strings.Contains(page, "Autumn") || strings.Contains(page, "Globex") ||
			strings.Index(page, first.ID) > strings.Index(page, expired.ID) || strings.Contains(page, "?after=") {
			t.Errorf("the list in %q shows a link of another workspace, or not its own in the order they were created, or a page after the last", lang)
		}
	}

	// A workspace with more links than a page shows: the page says where
	// the rest are, and the last one does not.
	created := []link.Link{first, expired}
	for len(created) <= linksPerPage {
		created = append(created, s.link(t, owner, link.Draft{}))
	}
	page := s.ask(t, http.MethodGet, "/en/links", with(inside)).Body.String()
	next := `<a href="/en/links?after=` + created[linksPerPage-1].ID + `">` + s.text(t, english, "links.next") + "</a>"
	if strings.Count(page, `<th scope="row">`) != linksPerPage || !strings.Contains(page, next) || strings.Contains(page, created[linksPerPage].ID) {
		t.Errorf("the first page of %d links does not show %d of them and where the rest are", len(created), linksPerPage)
	}
	last := s.ask(t, http.MethodGet, "/en/links?after="+created[linksPerPage-1].ID, with(inside)).Body.String()
	if strings.Count(last, `<th scope="row">`) != 1 || !strings.Contains(last, created[linksPerPage].ID) || strings.Contains(last, s.text(t, english, "links.next")) {
		t.Errorf("the last page does not show the one link that was left, and no page after it")
	}
	// The page after a link of another workspace is the page after nothing
	// that this one has, and one after what is not a link does not exist.
	if answer := s.ask(t, http.MethodGet, "/en/links?after="+theirs.ID, with(inside)); answer.Code != http.StatusOK ||
		strings.Contains(answer.Body.String(), "/links/"+theirs.ID) || strings.Contains(answer.Body.String(), "Autumn") {
		t.Errorf("the page after a link of another workspace = %d, or it shows that link", answer.Code)
	}
	s.shows(t, "the page after something that is not a link", s.ask(t, http.MethodGet, "/en/links?after=spring", with(inside)),
		http.StatusNotFound, english, "notfound.title")
}

// The address with no language leads to the door in the language the
// browser asks for, and a door without its slash to the door.
func TestTheAddressWithNoLanguageLeadsToADoor(t *testing.T) {
	s := newStage(t)
	for header, door := range map[string]string{"": "/es/", "en-GB,en;q=0.9": "/en/", "es-ES,es;q=0.9,en;q=0.8": "/es/", "fr-FR": "/es/"} {
		answer := s.ask(t, http.MethodGet, "/", saying("Accept-Language", header))
		sentTo(t, "the address with no language for a browser that asks for "+header, answer, door)
		if answer.Header().Get("Vary") != "Accept-Language" {
			t.Errorf("the address with no language: Vary = %q", answer.Header().Get("Vary"))
		}
	}
	for _, lang := range languages {
		answer := s.ask(t, http.MethodGet, "/"+string(lang))
		if answer.Code != http.StatusPermanentRedirect || answer.Header().Get("Location") != "/"+string(lang)+"/" || answer.Body.Len() != 0 {
			t.Errorf("GET /%s = %d to %q, want %d to its door", lang, answer.Code, answer.Header().Get("Location"), http.StatusPermanentRedirect)
		}
	}
	// A path has one form: the mux would redirect these with a page of
	// its own, and they have none.
	for _, path := range []string{"//es/links", "/es//links", "/es/./links", "/es/../en/", "/es/links/..", "/en//"} {
		answer := s.ask(t, http.MethodGet, path)
		if answer.Code != http.StatusNotFound || answer.Header().Get("Location") != "" || !strings.Contains(answer.Body.String(), "<h1>") {
			t.Errorf("GET %s = %d to %q, want the page of what is not found", path, answer.Code, answer.Header().Get("Location"))
		}
	}
}

// The log names a request by an identifier, a route and a session, and
// never by its path, its query or its token (docs/privacy.md).
func TestTheLogNamesTheSessionAndNeverTheTokenOrThePath(t *testing.T) {
	s := newStage(t)
	entered := s.named(t, "Acme", true)
	target := s.link(t, entered, link.Draft{})
	inside := s.sessionOf(t, entered)
	principal, err := s.sessions.Authenticate(t.Context(), inside.Value)
	if err != nil {
		t.Fatal(err)
	}

	page := s.ask(t, http.MethodGet, "/en/links/"+target.ID+"?from=newsletter", with(inside), from("192.0.2.77:4000"),
		saying("User-Agent", "sentinel-agent"), saying("Referer", "https://sentinel.example/page"))
	entry := s.ask(t, http.MethodPost, "/en/session", naming(entered), from("192.0.2.77:4001"))
	s.ask(t, http.MethodGet, "/en/nothing/"+target.Identifier)
	s.ask(t, http.MethodPost, "/en/logout", with(issued(t, entry)))

	var lines []map[string]any
	for line := range strings.Lines(s.log.String()) {
		var read map[string]any
		if err := json.Unmarshal([]byte(line), &read); err != nil {
			t.Fatalf("a line of the log is not JSON: %s", line)
		}
		if read["msg"] == "request" {
			lines = append(lines, read)
		}
	}
	if len(lines) != 4 {
		t.Fatalf("the log has %d lines of requests, want one for each of the four", len(lines))
	}
	if first := lines[0]; first["request"] != page.Header().Get("X-Request-Id") || first["method"] != "GET" ||
		first["route"] != "GET /en/links/{id}" || first["status"] != float64(http.StatusOK) || first["session"] != principal.SessionID || first["duration"] == nil {
		t.Errorf("the line of the page of a link is %v", first)
	}
	if second := lines[1]; second["route"] != "POST /en/session" || second["status"] != float64(http.StatusSeeOther) || second["session"] != nil {
		t.Errorf("the line of an entry is %v", second)
	}
	if third := lines[2]; third["route"] != "" || third["status"] != float64(http.StatusNotFound) {
		t.Errorf("the line of a path that is not a page is %v", third)
	}
	if fourth := lines[3]; fourth["route"] != "POST /en/logout" || fourth["status"] != float64(http.StatusSeeOther) {
		t.Errorf("the line of leaving is %v", fourth)
	}
	for _, sentinel := range []string{inside.Value, issued(t, entry).Value, target.ID, target.Identifier, "newsletter", "192.0.2.77", "sentinel", "Acme"} {
		if strings.Contains(s.log.String(), sentinel) {
			t.Errorf("the log holds %q", sentinel)
		}
	}
}
