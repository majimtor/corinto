//go:build integration

package panel

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/majimtor/corinto/internal/auth"
	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
)

// The form a credential has: the prefix, the identifier of its key and its
// secret.
var credentialForm = regexp.MustCompile(`cor_[0-9a-f]{32}_[a-z2-7]{52}`)

// key issues a key of a workspace, with a name and scopes.
func (s *stage) key(t *testing.T, workspaceID, name string, scopes ...auth.Scope) (auth.Key, string) {
	t.Helper()
	issued, credential, err := s.keys.Issue(t.Context(), workspaceID, auth.Draft{Name: name, Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return issued, credential
}

// inForce returns the keys of a workspace that are in force.
func (s *stage) inForce(t *testing.T, workspaceID string) []auth.Key {
	t.Helper()
	found, err := s.keys.List(t.Context(), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// inWords returns a day or a moment as the language of a page writes it.
func inWords(t *testing.T, write func(language, time.Time) (string, error), lang language, at time.Time) string {
	t.Helper()
	text, err := write(lang, at)
	if err != nil {
		t.Fatal(err)
	}
	return escaped(text)
}

// The list of the keys of a workspace: the ones in force, with what each
// may do, its state and when it was last used.
func TestTheListOfKeysShowsTheKeysInForceAndOfNoOtherWorkspace(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	other, _ := s.shop(t, link.WorkspaceDraft{Name: "Globex"})

	for _, lang := range languages {
		answer := s.ask(t, http.MethodGet, "/"+string(lang)+"/keys", with(inside))
		s.shows(t, "the list of a workspace without keys", answer, http.StatusOK, lang, "keys.title")
		page := answer.Body.String()
		for what, want := range map[string]string{
			"that it has none":    "<p>" + s.text(t, lang, "keys.empty") + "</p>",
			"where one is issued": `<a href="/` + string(lang) + `/keys/new">` + s.text(t, lang, "keys.new") + "</a>",
			"what a key is":       "<p>" + s.text(t, lang, "keys.intro") + "</p>",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the list in %q of a workspace without keys does not show %s: no %q in it", lang, what, want)
			}
		}
		if strings.Contains(page, `<ul class="keys"`) {
			t.Errorf("the list in %q of a workspace without keys shows a list", lang)
		}
	}

	// Three keys: one that was used, one that expires, one that is revoked;
	// and a key of another workspace.
	used, credential := s.key(t, shop, `Deploy <pipeline>`, auth.ScopeLinksWrite, auth.ScopeLinksRead)
	if _, err := s.keys.Authenticate(t.Context(), credential); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Minute)
	expiring, _, err := s.keys.Issue(t.Context(), shop, auth.Draft{Name: "Launch week", Scopes: []auth.Scope{auth.ScopeMetricsRead}, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	revoked, _ := s.key(t, shop, "Forgotten", auth.ScopeLinksRead)
	if err := s.keys.Revoke(t.Context(), shop, revoked.ID); err != nil {
		t.Fatal(err)
	}
	theirs, _ := s.key(t, other, "Of another workspace", auth.ScopeLinksRead)
	var lastUsed time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT last_used_on FROM api_keys WHERE id = $1::uuid`, used.ID).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}

	// row is a key of the list as the page writes it: its name, what it
	// has, each thing under its own name, and where it is revoked.
	row := func(lang language, key auth.Key, state, expires, lastUse string) string {
		scopes := ""
		for _, scope := range key.Scopes {
			scopes += "<li><code>" + string(scope) + "</code></li>"
		}
		fact := func(name, class, value string) string {
			return "<div><dt>" + s.text(t, lang, name) + "</dt><dd" + class + ">" + value + "</dd></div>\n"
		}
		here := "/" + string(lang) + "/keys/" + key.ID + "/revoke"
		return "<li>\n<h2>" + escaped(key.Name) + "</h2>\n<dl class=\"facts\">\n" +
			fact("keys.identifier", ` class="address code"`, key.ID) + fact("keys.scopes", "", `<ul class="scopes">`+scopes+"</ul>") +
			fact("keys.state", "", s.text(t, lang, state)) + fact("keys.created", "", inWords(t, s.texts.date, lang, key.CreatedAt)) +
			fact("keys.expires", "", expires) + fact("keys.last_used", "", lastUse) +
			"</dl>\n<p class=\"actions\"><a href=\"" + here + `" aria-label="` + s.text(t, lang, "keys.revoke_named", "name", key.Name) + `">` +
			s.text(t, lang, "keys.revoke") + "</a></p>\n</li>"
	}
	for _, lang := range languages {
		page := s.ask(t, http.MethodGet, "/"+string(lang)+"/keys", with(inside)).Body.String()
		rows := []string{
			row(lang, used, "key.state.in_force", s.text(t, lang, "keys.no_expiry"), inWords(t, s.texts.date, lang, lastUsed)),
			row(lang, expiring, "key.state.in_force", inWords(t, s.texts.moment, lang, expiry), s.text(t, lang, "keys.never_used")),
		}
		if want := `<ul class="keys" aria-labelledby="keys-list">` + "\n" + strings.Join(rows, "\n") + "\n</ul>"; !strings.Contains(page, want) {
			t.Errorf("the list in %q is not its two keys in force, the oldest first: no %q in %q", lang, want, page)
		}
		if !strings.Contains(page, `<p id="keys-list">`+s.text(t, lang, "keys.caption")+"</p>") {
			t.Errorf("the list in %q does not say what it holds", lang)
		}
		// The scopes of a key are stored in one order, whatever order they
		// were asked in.
		if !slices.Equal(used.Scopes, []auth.Scope{auth.ScopeLinksRead, auth.ScopeLinksWrite}) {
			t.Fatalf("the scopes of the first key are %v", used.Scopes)
		}
		if strings.Contains(page, revoked.ID) || strings.Contains(page, "Forgotten") || strings.Contains(page, theirs.ID) ||
			strings.Contains(page, "another workspace") || credentialForm.MatchString(page) {
			t.Errorf("the list in %q shows a revoked key, a key of another workspace or a credential", lang)
		}
	}

	// A key whose time is up stays in the list, and the list says so.
	s.clock.advance(expiry.Sub(s.clock.now()))
	for _, lang := range languages {
		page := s.ask(t, http.MethodGet, "/"+string(lang)+"/keys", with(inside)).Body.String()
		if want := row(lang, expiring, "key.state.expired", inWords(t, s.texts.moment, lang, expiry), s.text(t, lang, "keys.never_used")); !strings.Contains(page, want) {
			t.Errorf("the list in %q, at the moment a key expires, does not say that it has: no %q in it", lang, want)
		}
		if strings.Count(page, "<dd>"+s.text(t, lang, "key.state.expired")+"</dd>") != 1 {
			t.Errorf("the list in %q does not say of one key, and one alone, that it has expired", lang)
		}
	}
}

// The bar of every page behind a session leads to the links of the
// workspace, to its keys and to what became of its events, whatever the
// page shows under those names.
func TestTheBarLeadsToTheLinksTheKeysAndTheStatusFromEveryPage(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	target := s.link(t, shop, link.Draft{})
	held, _ := s.key(t, shop, "Deploy pipeline", auth.ScopeLinksRead)
	for _, lang := range languages {
		door := "/" + string(lang)
		for _, path := range []string{
			"/links", "/links/new", "/links/" + target.ID, "/links/" + target.ID + "/edit", "/links/" + target.ID + "/delete",
			"/keys", "/keys/new", "/keys/" + held.ID + "/revoke", "/status",
		} {
			page := s.ask(t, http.MethodGet, door+path, with(inside)).Body.String()
			want := `<a href="` + door + `/links">` + s.text(t, lang, "layout.links") + "</a>\n" +
				`<a href="` + door + `/keys">` + s.text(t, lang, "layout.keys") + "</a>\n" +
				`<a href="` + door + `/status">` + s.text(t, lang, "layout.status") + "</a>\n" +
				`<form method="post" action="` + door + `/logout">`
			if !strings.Contains(page, want) {
				t.Errorf("the bar of %s does not lead to the links, to the keys, to the status and out: no %q in it", door+path, want)
			}
		}
		// The door is behind no session, and has no bar.
		if page := s.ask(t, http.MethodGet, door+"/").Body.String(); strings.Contains(page, door+"/keys") {
			t.Errorf("the door in %q leads to the keys of a workspace", lang)
		}
	}
}

// The list of links keeps its page when the language changes.
func TestTheListOfLinksKeepsItsPageInTheOtherLanguage(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	first := s.link(t, shop, link.Draft{})
	s.link(t, shop, link.Draft{})
	page := s.ask(t, http.MethodGet, "/en/links?after="+first.ID, with(inside)).Body.String()
	if want := `<a href="/es/links?after=` + first.ID + `" hreflang="es"`; !strings.Contains(page, want) || strings.Count(page, `<th scope="row">`) != 1 {
		t.Errorf("the second page of the list does not lead to itself in the other language: no %q in it", want)
	}
	if page := s.ask(t, http.MethodGet, "/en/links", with(inside)).Body.String(); !strings.Contains(page, `<a href="/es/links" hreflang="es"`) {
		t.Error("the first page of the list does not lead to the first page in the other language")
	}
}

// The form that issues a key, and the one answer that shows it.
func TestTheFormIssuesAKeyAndShowsItOnce(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})

	for _, lang := range languages {
		answer := s.ask(t, http.MethodGet, "/"+string(lang)+"/keys/new", with(inside))
		s.shows(t, "the form of a new key", answer, http.StatusOK, lang, "keys.new")
		page := answer.Body.String()
		for what, want := range map[string]string{
			"where it is sent":        `<form method="post" action="/` + string(lang) + `/keys" novalidate>`,
			"the name":                `<label for="name">` + s.text(t, lang, "keys.name") + "</label>",
			"the scopes, as a group":  "<legend>" + s.text(t, lang, "keys.scopes") + "</legend>",
			"a scope and what it is":  `<input type="checkbox" id="scope-links-write" name="scopes" value="links:write"><label for="scope-links-write"><code>links:write</code> ` + s.text(t, lang, "scope.links.write") + "</label>",
			"the scope of the figure": `<code>metrics:read</code> ` + s.text(t, lang, "scope.metrics.read") + "</label>",
			"the expiry, in UTC":      `<p class="hint" id="expires_at-hint">` + s.text(t, lang, "keyform.expires.hint") + "</p>",
			"its button":              `<button type="submit" class="primary">` + s.text(t, lang, "keyform.create") + "</button>",
			"the way back":            `<a href="/` + string(lang) + `/keys">` + s.text(t, lang, "form.cancel") + "</a>",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the form in %q does not show %s: no %q in it", lang, what, want)
			}
		}
		// Three scopes, none chosen for whoever fills the form, and none of
		// the ones that no operation asks for yet.
		if strings.Count(page, `type="checkbox"`) != 3 || strings.Contains(page, " checked") || strings.Contains(page, "webhooks") {
			t.Errorf("the form in %q does not offer its three scopes with none chosen", lang)
		}
		if strings.Contains(page, `class="error"`) || strings.Contains(page, "aria-invalid") || strings.Contains(page, `role="alert"`) {
			t.Errorf("the empty form in %q says that something is wrong", lang)
		}
	}

	for _, lang := range languages {
		before := len(s.inForce(t, shop))
		answer := s.send(t, inside, "/"+string(lang)+"/keys", url.Values{
			"name": {"  Deploy <pipeline>  "}, "scopes": {"metrics:read", "links:read"}, "expires_at": {"2031-03-01T08:30"},
		})
		s.shows(t, "the answer to the form of a key", answer, http.StatusCreated, lang, "issued.title")
		page := answer.Body.String()
		keys := s.inForce(t, shop)
		if len(keys) != before+1 {
			t.Fatalf("the workspace has %d keys after the form, want %d", len(keys), before+1)
		}
		issued := keys[len(keys)-1]
		expiry := time.Date(2031, time.March, 1, 8, 30, 0, 0, time.UTC)
		if issued.Name != "Deploy <pipeline>" || !slices.Equal(issued.Scopes, []auth.Scope{auth.ScopeLinksRead, auth.ScopeMetricsRead}) ||
			issued.ExpiresAt == nil || !issued.ExpiresAt.Equal(expiry) {
			t.Errorf("the key issued is %+v", issued)
		}

		// The credential is in the answer once, in a field that takes no
		// typing, and it is the one of the key: it authenticates as it.
		shown := credentialForm.FindAllString(page, -1)
		if len(shown) != 1 {
			t.Fatalf("the answer in %q shows %d credentials, want one", lang, len(shown))
		}
		credential := shown[0]
		if want := `<input type="text" id="credential" class="code" value="` + credential + `" readonly`; !strings.Contains(page, want) {
			t.Errorf("the answer in %q does not show the credential in its field: no %q in it", lang, want)
		}
		principal, err := s.keys.Authenticate(t.Context(), credential)
		if err != nil || principal.KeyID != issued.ID || principal.WorkspaceID != shop || !slices.Equal(principal.Scopes, issued.Scopes) {
			t.Errorf("the credential shown resolves to %+v, %v; want the key %s", principal, err, issued.ID)
		}
		for what, want := range map[string]string{
			"that it is shown once": `<p class="notice">` + s.text(t, lang, "issued.once") + "</p>",
			"how it is sent":        `<p class="hint" id="credential-hint">` + s.text(t, lang, "issued.hint") + "</p>",
			"the name of the key":   "<dd>Deploy &lt;pipeline&gt;</dd>",
			"its identifier":        `<dd class="address code">` + issued.ID + "</dd>",
			"its scopes":            `<ul class="scopes"><li><code>links:read</code></li><li><code>metrics:read</code></li></ul>`,
			"its expiry":            "<dd>" + inWords(t, s.texts.moment, lang, expiry) + "</dd>",
			"the way to the list":   `<a href="/` + string(lang) + `/keys">` + s.text(t, lang, "issued.back") + "</a>",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the answer in %q does not show %s: no %q in it", lang, what, want)
			}
		}
		// No cache keeps the answer, and it leads nowhere that would.
		if answer.Header().Get("Cache-Control") != "no-store" || answer.Header().Get("Location") != "" {
			t.Errorf("the answer in %q: Cache-Control %q, Location %q", lang, answer.Header().Get("Cache-Control"), answer.Header().Get("Location"))
		}

		// Nothing shows it again: not the list, not the page that revokes
		// the key, and not the log.
		_, secret, _ := strings.Cut(strings.TrimPrefix(credential, "cor_"), "_")
		for _, path := range []string{"/keys", "/keys/" + issued.ID + "/revoke", "/keys/new"} {
			if again := s.ask(t, http.MethodGet, "/"+string(lang)+path, with(inside)).Body.String(); strings.Contains(again, secret) {
				t.Errorf("GET %s shows the secret of a key again", path)
			}
		}
		if log := s.log.String(); strings.Contains(log, secret) || strings.Contains(log, credential) || strings.Contains(log, "Deploy") {
			t.Errorf("the log holds the secret of a key, or its name")
		}
	}

	// A key with a name and a scope, and nothing else: it does not expire.
	answer := s.send(t, inside, "/en/keys", url.Values{"name": {"Reader"}, "scopes": {"links:read"}})
	s.shows(t, "the answer to a form with no expiry", answer, http.StatusCreated, english, "issued.title")
	keys := s.inForce(t, shop)
	if last := keys[len(keys)-1]; last.Name != "Reader" || last.ExpiresAt != nil || !strings.Contains(answer.Body.String(), "<dd>"+s.text(t, english, "keys.no_expiry")+"</dd>") {
		t.Errorf("the key issued with no expiry is %+v, or its page does not say that it does not expire", last)
	}
	// The same form sent again issues another key, with a secret of its
	// own: the answer to a form is the only place a credential is shown.
	again := s.send(t, inside, "/en/keys", url.Values{"name": {"Reader"}, "scopes": {"links:read"}})
	if first, second := credentialForm.FindString(answer.Body.String()), credentialForm.FindString(again.Body.String()); again.Code != http.StatusCreated ||
		first == second || len(s.inForce(t, shop)) != len(keys)+1 {
		t.Errorf("the form sent again = %d, and it did not issue another key with another credential", again.Code)
	}
}

// What is wrong with the form of a key is said in words, next to its field
// and at the top of the page, and nothing is issued.
func TestWhatIsWrongWithTheFormOfAKeyIsSaidNextToItsField(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	scope := []string{"links:read"}

	for _, c := range []struct {
		what   string
		fields url.Values
		field  string
		key    string
	}{
		{"no name", url.Values{"scopes": scope}, fieldKeyName, "error.key.name.required"},
		{"a name of spaces", url.Values{"name": {"   "}, "scopes": scope}, fieldKeyName, "error.key.name.required"},
		{"a name of 101 characters", url.Values{"name": {strings.Repeat("n", 101)}, "scopes": scope}, fieldKeyName, "error.key.name.characters"},
		{"a name with a control character", url.Values{"name": {"Deploy\x07pipeline"}, "scopes": scope}, fieldKeyName, "error.key.name.characters"},
		{"no scope", url.Values{"name": {"Deploy"}}, fieldKeyScopes, "error.key.scopes"},
		{"a scope the form does not offer", url.Values{"name": {"Deploy"}, "scopes": {"links:read", "webhooks:write"}}, fieldKeyScopes, "error.key.scopes"},
		{"a scope that does not exist", url.Values{"name": {"Deploy"}, "scopes": {"everything"}}, fieldKeyScopes, "error.key.scopes"},
		{"an empty scope", url.Values{"name": {"Deploy"}, "scopes": {""}}, fieldKeyScopes, "error.key.scopes"},
		{"an expiry that is not a moment", url.Values{"name": {"Deploy"}, "scopes": scope, "expires_at": {"tomorrow"}}, fieldExpiresAt, "error.expires_at.invalid"},
		{"an expiry that has passed", url.Values{"name": {"Deploy"}, "scopes": scope, "expires_at": {"2020-01-01T00:00"}}, fieldExpiresAt, "error.expires_at.past"},
		// Later than the clock of the page, which stands a week back, and
		// earlier than the one of the database, which decides.
		{"an expiry the database has passed", url.Values{"name": {"Deploy"}, "scopes": scope, "expires_at": {today.Add(20 * time.Hour).Format("2006-01-02T15:04")}}, fieldExpiresAt, "error.expires_at.past"},
	} {
		for _, lang := range languages {
			answer := s.send(t, inside, "/"+string(lang)+"/keys", c.fields)
			page := answer.Body.String()
			said := s.text(t, lang, c.key)
			if answer.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s in %q = %d, want %d", c.what, lang, answer.Code, http.StatusUnprocessableEntity)
			}
			for what, want := range map[string]string{
				"the title that says so":   "<title>" + s.text(t, lang, "form.error_mark") + " " + s.text(t, lang, "keys.new") + " · Corinto</title>",
				"the count of problems":    `<h2 id="problems-title">` + escaped(counted(t, s, lang, 1)) + "</h2>",
				"the way to the field":     `<li><a href="#` + c.field + `">` + said + "</a></li>",
				"the problem by its field": `<p class="error" id="` + c.field + `-error">` + said + "</p>",
				"the field described":      ` ` + c.field + `-error"`,
			} {
				if !strings.Contains(page, want) {
					t.Errorf("%s in %q: the form does not show %s: no %q in it", c.what, lang, what, want)
				}
			}
			// One problem, one field marked: an input says so itself, and
			// the group of the scopes by what describes it.
			marked := 1
			if c.field == fieldKeyScopes {
				marked = 0
			}
			if strings.Count(page, `aria-invalid="true"`) != marked || strings.Count(page, `class="error"`) != 1 || credentialForm.MatchString(page) {
				t.Errorf("%s in %q: the form marks %d fields and says %d problems, or shows a credential", c.what, lang,
					strings.Count(page, `aria-invalid="true"`), strings.Count(page, `class="error"`))
			}
			// What was chosen stays chosen.
			if chosen := strings.Count(page, " checked"); chosen != len(slices.DeleteFunc(slices.Clone(c.fields["scopes"]), func(scope string) bool { return scope != "links:read" })) {
				t.Errorf("%s in %q: the form comes back with %d scopes chosen", c.what, lang, chosen)
			}
		}
	}
	if keys := s.inForce(t, shop); len(keys) != 0 {
		t.Fatalf("the forms that were wrong issued %d keys", len(keys))
	}

	// What was typed stays in the form, as text; and several problems are
	// counted and said in the order of the form.
	answer := s.send(t, inside, "/en/keys", url.Values{"name": {` <b>Deploy</b> `}, "expires_at": {"2020-01-01T00:00"}})
	page := answer.Body.String()
	order := []int{
		strings.Index(page, `<a href="#name">`), strings.Index(page, `<a href="#scopes">`), strings.Index(page, `<a href="#expires_at">`),
	}
	if !strings.Contains(page, `name="name" value="&lt;b&gt;Deploy&lt;/b&gt;"`) || !strings.Contains(page, `name="expires_at" value="2020-01-01T00:00"`) ||
		!strings.Contains(page, escaped(counted(t, s, english, 2))) || order[0] != -1 || order[1] == -1 || order[1] > order[2] {
		t.Errorf("a form with two problems does not keep what was typed, or does not say its two problems in the order of the form: %v", order)
	}
	three := s.send(t, inside, "/en/keys", url.Values{"expires_at": {"never"}}).Body.String()
	if !strings.Contains(three, escaped(counted(t, s, english, 3))) ||
		strings.Index(three, `<a href="#name">`) > strings.Index(three, `<a href="#scopes">`) || !strings.Contains(three, `<a href="#name">`) {
		t.Error("a form with nothing right in it does not say its three problems, the name first")
	}

	// A workspace with all the keys it can have: the form says so, of no
	// field, and issues nothing.
	for i := range 10 {
		s.key(t, shop, "Key "+strconv.Itoa(i), auth.ScopeLinksRead)
	}
	for _, lang := range languages {
		full := s.send(t, inside, "/"+string(lang)+"/keys", url.Values{"name": {"One more"}, "scopes": scope})
		if page := full.Body.String(); full.Code != http.StatusUnprocessableEntity || !strings.Contains(page, "<li>"+s.text(t, lang, "error.key.quota")+"</li>") ||
			strings.Contains(page, `class="error"`) || !strings.Contains(page, `value="One more"`) || credentialForm.MatchString(page) {
			t.Errorf("the form of a full workspace in %q = %d, and it does not say that the workspace is full, at the top and of no field", lang, full.Code)
		}
	}
	if keys := s.inForce(t, shop); len(keys) != 10 {
		t.Errorf("the full workspace has %d keys after the form", len(keys))
	}
}

// Revoking a key asks first, and then ends it.
func TestRevokingAKeyAsksFirstAndEndsIt(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	other, _ := s.shop(t, link.WorkspaceDraft{Name: "Globex"})
	kept, _ := s.key(t, shop, "Reader", auth.ScopeLinksRead)
	theirs, theirCredential := s.key(t, other, "Of another workspace", auth.ScopeLinksRead)
	works := func(credential string) bool {
		_, err := s.keys.Authenticate(t.Context(), credential)
		return err == nil
	}

	for _, lang := range languages {
		held, credential := s.key(t, shop, `Deploy <pipeline>`, auth.ScopeLinksWrite)
		path := "/" + string(lang) + "/keys/" + held.ID + "/revoke"

		// The question changes nothing.
		question := s.ask(t, http.MethodGet, path, with(inside))
		s.shows(t, "the question before revoking", question, http.StatusOK, lang, "revoke.title", "name", `Deploy <pipeline>`)
		page := question.Body.String()
		for what, want := range map[string]string{
			"what revoking does":  "<p>" + s.text(t, lang, "revoke.text") + "</p>",
			"the form that does":  `<form method="post" action="` + path + `">`,
			"its button":          `<button type="submit" class="danger">` + s.text(t, lang, "revoke.confirm") + "</button>",
			"the way to leave it": `<a href="/` + string(lang) + `/keys">` + s.text(t, lang, "revoke.keep") + "</a>",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the question in %q does not show %s: no %q in it", lang, what, want)
			}
		}
		if !works(credential) {
			t.Fatalf("the question in %q revoked the key", lang)
		}

		sentTo(t, "revoking a key", s.send(t, inside, path, url.Values{}), "/"+string(lang)+"/keys")
		if works(credential) {
			t.Errorf("the key revoked in %q still authenticates", lang)
		}
		list := s.ask(t, http.MethodGet, "/"+string(lang)+"/keys", with(inside)).Body.String()
		if strings.Contains(list, held.ID) || !strings.Contains(list, kept.ID) {
			t.Errorf("the list in %q shows the key that was revoked, or not the one that was kept", lang)
		}
		// A key that is revoked is no longer there, to ask about or to
		// revoke again.
		s.shows(t, "the question about a revoked key", s.ask(t, http.MethodGet, path, with(inside)), http.StatusNotFound, lang, "notfound.title")
		s.shows(t, "revoking a key again", s.send(t, inside, path, url.Values{}), http.StatusNotFound, lang, "notfound.title")
	}

	// A key of another workspace is answered like one that does not exist,
	// and stays as it was.
	for _, id := range []string{theirs.ID, uuid.NewV7().String(), "deploy-pipeline"} {
		path := "/en/keys/" + id + "/revoke"
		for what, answer := range map[string]*httptest.ResponseRecorder{
			"the question about": s.ask(t, http.MethodGet, path, with(inside)),
			"revoking":           s.send(t, inside, path, url.Values{}),
		} {
			if answer.Code != http.StatusNotFound || !strings.Contains(answer.Body.String(), "<h1>"+s.text(t, english, "notfound.title")+"</h1>") ||
				strings.Contains(answer.Body.String(), "another workspace") {
				t.Errorf("%s the key %s = %d, want the page of what is not found and nothing of the key", what, id, answer.Code)
			}
		}
	}
	if !works(theirCredential) || len(s.inForce(t, shop)) != 1 {
		t.Error("the key of another workspace was revoked, or the one that was kept is gone")
	}
}

// The pages of the keys are behind a session, change nothing on a GET, take
// nothing from another site, and their changes count in the minute of the
// session.
func TestTheKeysAreBehindASessionAndChangeOnlyByAFormOfThePanel(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	held, credential := s.key(t, shop, "Deploy pipeline", auth.ScopeLinksRead)
	revoke := "/en/keys/" + held.ID + "/revoke"
	form := url.Values{"name": {"Reader"}, "scopes": {"links:read"}}

	// Without a session every one of them leads to the door.
	for _, path := range []string{"/en/keys", "/en/keys/new", revoke} {
		sentTo(t, "GET "+path+" without a session", s.ask(t, http.MethodGet, path), "/en/")
	}
	for _, path := range []string{"/en/keys", revoke} {
		sentTo(t, "POST "+path+" without a session", s.ask(t, http.MethodPost, path, filled(form)), "/en/")
	}
	// From a page that is not of the panel, nothing.
	for _, path := range []string{"/es/keys", "/es/keys/" + held.ID + "/revoke"} {
		for _, site := range []string{"cross-site", "same-site"} {
			answer := s.send(t, inside, path, form, saying("Sec-Fetch-Site", site))
			s.shows(t, "POST "+path+" from a page that is "+site, answer, http.StatusForbidden, spanish, "refused.title")
		}
	}
	// What a form says is in its body, and a form is never larger than one
	// of the panel.
	inAddress := s.send(t, inside, "/en/keys?name=Reader&scopes=links%3Aread", url.Values{})
	if inAddress.Code != http.StatusUnprocessableEntity || !strings.Contains(inAddress.Body.String(), s.text(t, english, "error.key.name.required")) {
		t.Errorf("a form with its fields in the address = %d, want it to lack a name", inAddress.Code)
	}
	scopeInAddress := s.send(t, inside, "/en/keys?scopes=links%3Aread", url.Values{"name": {"Reader"}})
	if page := scopeInAddress.Body.String(); scopeInAddress.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(page, s.text(t, english, "error.key.scopes")) || strings.Contains(page, " checked") {
		t.Errorf("a form with its scope in the address = %d, want it to lack a scope and to show none chosen", scopeInAddress.Code)
	}
	large := url.Values{"name": {strings.Repeat("n", limits.APIMaxBodyBytes)}, "scopes": {"links:read"}}
	s.shows(t, "a form that is too large", s.send(t, inside, "/en/keys", large), http.StatusNotFound, english, "notfound.title")
	// No other method reaches them.
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		s.shows(t, method+" "+revoke, s.ask(t, method, revoke, with(inside)), http.StatusNotFound, english, "notfound.title")
	}
	if keys := s.inForce(t, shop); len(keys) != 1 || keys[0].ID != held.ID {
		t.Fatalf("the workspace has the keys %+v after the requests that were refused", keys)
	}
	if _, err := s.keys.Authenticate(t.Context(), credential); err != nil {
		t.Fatalf("the key no longer authenticates after the requests that were refused: %v", err)
	}

	// The forms of the keys count among the sixty changes of a minute, the
	// ones that are wrong too. A session of its own makes them: the one
	// above has made some.
	fresh := s.sessionOf(t, shop)
	for i := range limits.APIWritesPerMinute {
		if answer := s.send(t, fresh, "/en/keys", url.Values{}); answer.Code != http.StatusUnprocessableEntity {
			t.Fatalf("form %d of a minute = %d, want %d", i+1, answer.Code, http.StatusUnprocessableEntity)
		}
	}
	limited := s.send(t, fresh, "/en/keys", form)
	s.shows(t, "the form of a key after the sixtieth change", limited, http.StatusTooManyRequests, english, "busy.title")
	if limited.Header().Get("Retry-After") != "1" || credentialForm.MatchString(limited.Body.String()) || len(s.inForce(t, shop)) != 1 {
		t.Errorf("the form after the sixtieth change: Retry-After = %q, or it issued a key", limited.Header().Get("Retry-After"))
	}
	s.shows(t, "revoking after the sixtieth change", s.send(t, fresh, revoke, url.Values{}), http.StatusTooManyRequests, english, "busy.title")
	if _, err := s.keys.Authenticate(t.Context(), credential); err != nil {
		t.Errorf("the key was revoked past the limit: %v", err)
	}
}
