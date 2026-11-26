//go:build integration

package panel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
)

// shop creates a demo workspace as a test describes it, and returns its
// identifier and the cookie of a session in it.
func (s *stage) shop(t *testing.T, draft link.WorkspaceDraft) (string, *http.Cookie) {
	t.Helper()
	draft.Demo = true
	if draft.Name == "" {
		draft.Name = "Acme"
	}
	if draft.MaxLinks == 0 {
		draft.MaxLinks = 100
	}
	draft.MaxKeys = 10
	created, err := link.CreateWorkspace(t.Context(), s.pool, draft)
	if err != nil {
		t.Fatal(err)
	}
	return created.ID, s.sessionOf(t, created.ID)
}

// send posts a form to the panel with a session.
func (s *stage) send(t *testing.T, cookie *http.Cookie, path string, fields url.Values, changes ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	return s.ask(t, http.MethodPost, path, append([]func(*http.Request){with(cookie), filled(fields)}, changes...)...)
}

// only returns the one link of a workspace.
func (s *stage) only(t *testing.T, workspaceID string) link.Link {
	t.Helper()
	found := s.all(t, workspaceID)
	if len(found) != 1 {
		t.Fatalf("the workspace has %d links, want one", len(found))
	}
	return found[0]
}

// all returns the links of a workspace.
func (s *stage) all(t *testing.T, workspaceID string) []link.Link {
	t.Helper()
	found, _, err := s.store.List(t.Context(), workspaceID, "", link.MaxPage)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// The form that creates a link: what it asks for, and what it leaves.
func TestTheFormCreatesALinkAndLeadsToItsPage(t *testing.T) {
	s := newStage(t)
	plain, inPlain := s.shop(t, link.WorkspaceDraft{})
	chosen, inChosen := s.shop(t, link.WorkspaceDraft{MayChooseAliases: true, Allowed: []string{"https://www.example.com/", "https://www.example.org/shop"}})

	// The list leads to the form, and the form asks for what the workspace
	// can give: an alias only where one can be chosen, and the list of
	// destinations where there is one.
	if list := s.ask(t, http.MethodGet, "/en/links", with(inPlain)); !strings.Contains(list.Body.String(), `<a href="/en/links/new">`+s.text(t, english, "links.new")+`</a>`) {
		t.Error("the list does not lead to the form of a new link")
	}
	for _, lang := range languages {
		answer := s.ask(t, http.MethodGet, "/"+string(lang)+"/links/new", with(inChosen))
		s.shows(t, "the form of a new link", answer, http.StatusOK, lang, "form.new.title")
		page := answer.Body.String()
		for what, want := range map[string]string{
			"where it is sent":         `<form method="post" action="/` + string(lang) + `/links" novalidate>`,
			"the destination":          `<label for="destination">` + s.text(t, lang, "form.destination.label") + `</label>`,
			"what the destination is":  `<p class="hint" id="destination-hint">` + s.text(t, lang, "form.destination.hint") + `</p>`,
			"where it may lead":        `<li class="address">https://www.example.org/shop</li>`,
			"the alias":                `<input type="text" id="alias" name="alias" value=""`,
			"the expiry, in UTC":       `<p class="hint" id="expires_at-hint">` + s.text(t, lang, "form.expires.hint") + `</p>`,
			"the campaign, as a group": `<legend>` + s.text(t, lang, "form.campaign.legend") + `</legend>`,
			"its button":               `<button type="submit" class="primary">` + s.text(t, lang, "form.create") + `</button>`,
			"the way back":             `<a href="/` + string(lang) + `/links">` + s.text(t, lang, "form.cancel") + `</a>`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the form in %q does not show %s: no %q in it", lang, what, want)
			}
		}
		if strings.Contains(page, `class="error"`) || strings.Contains(page, "aria-invalid") || strings.Contains(page, `role="alert"`) {
			t.Errorf("the empty form in %q says that something is wrong", lang)
		}
	}
	if page := s.ask(t, http.MethodGet, "/en/links/new", with(inPlain)).Body.String(); strings.Contains(page, `name="alias"`) || strings.Contains(page, "destination-allowed") {
		t.Error("the form of a workspace that cannot choose aliases and has no list asks for an alias, or shows a list")
	}

	// Everything the form takes, with spaces around it as when it is
	// pasted.
	answer := s.send(t, inChosen, "/es/links", url.Values{
		"destination": {"  https://www.example.com/books/spring  "}, "alias": {" spring-books "}, "title": {" Spring books "},
		"tags": {"spring, books ,, newsletter"}, "utm_source": {"newsletter"}, "utm_medium": {"email"}, "utm_campaign": {"spring"},
		"utm_term": {"novels"}, "utm_content": {"header"}, "expires_at": {"2027-03-01T08:30"},
	})
	created := s.only(t, chosen)
	sentTo(t, "creating a link", answer, "/es/links/"+created.ID)
	expiry := time.Date(2027, time.March, 1, 8, 30, 0, 0, time.UTC)
	if created.Identifier != "spring-books" || created.Destination != "https://www.example.com/books/spring" || created.Title != "Spring books" ||
		!slices.Equal(created.Tags, []string{"spring", "books", "newsletter"}) ||
		created.Campaign != (link.Campaign{Source: "newsletter", Medium: "email", Name: "spring", Term: "novels", Content: "header"}) ||
		created.ExpiresAt == nil || !created.ExpiresAt.Equal(expiry) || created.State != link.StateActive {
		t.Errorf("the link created is %+v", created)
	}
	if !strings.Contains(created.Resolved, "utm_source=newsletter") || !strings.Contains(created.Resolved, "utm_content=header") {
		t.Errorf("the link created redirects to %q, without its campaign", created.Resolved)
	}
	// Its page is there, and shows it.
	if page := s.ask(t, http.MethodGet, "/es/links/"+created.ID, with(inChosen)); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<h1>Spring books</h1>") {
		t.Errorf("the page of the link created = %d", page.Code)
	}

	// The least it takes: a destination. The identifier is generated, and
	// an alias sent to a workspace that cannot choose one is not read.
	answer = s.send(t, inPlain, "/en/links", url.Values{"destination": {"https://example.com/"}, "alias": {"chosen"}})
	least := s.only(t, plain)
	sentTo(t, "creating a link with a destination and nothing else", answer, "/en/links/"+least.ID)
	if least.Identifier == "chosen" || least.Title != "" || len(least.Tags) != 0 || least.ExpiresAt != nil || least.Campaign != (link.Campaign{}) {
		t.Errorf("the link created with a destination alone is %+v", least)
	}
}

// What is wrong with a form is said in words, next to its field and at the
// top of the page, with what was typed still there; and nothing is
// created.
func TestWhatIsWrongWithAFormIsSaidNextToItsField(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{MayChooseAliases: true})
	listed, inListed := s.shop(t, link.WorkspaceDraft{Allowed: []string{"https://www.example.com/"}})
	full, inFull := s.shop(t, link.WorkspaceDraft{MaxLinks: 1})
	s.link(t, shop, link.Draft{Alias: "taken-alias"})
	s.link(t, full, link.Draft{})
	valid := "https://example.com/books"

	for _, c := range []struct {
		what    string
		session *http.Cookie
		fields  url.Values
		field   string
		key     string
	}{
		{"no destination", inside, url.Values{"title": {"Spring"}}, fieldDestination, "error.destination.required"},
		{"a destination of another scheme", inside, url.Values{"destination": {"ftp://example.com/books"}}, fieldDestination, "error.destination.scheme"},
		{"a destination with a space", inside, url.Values{"destination": {"https://example.com/spring books"}}, fieldDestination, "error.destination.characters"},
		{"a destination with a password", inside, url.Values{"destination": {"https://reader:secret-word-of-a-test@example.com/"}}, fieldDestination, "error.destination.credentials"},
		{"a destination at an address", inside, url.Values{"destination": {"https://192.0.2.10/books"}}, fieldDestination, "error.destination.host"},
		{"a destination with a port that is not one", inside, url.Values{"destination": {"https://example.com:99999/"}}, fieldDestination, "error.destination.port"},
		{"a destination with a dot segment", inside, url.Values{"destination": {"https://example.com/a/../b"}}, fieldDestination, "error.destination.dot_segment"},
		{"a destination that is the short domain", inside, url.Values{"destination": {"https://" + redirectDomain + "/spring"}}, fieldDestination, "error.destination.redirect_domain"},
		{"a destination that is too long", inside, url.Values{"destination": {"https://example.com/" + strings.Repeat("a", 2100)}}, fieldDestination, "error.destination.too_long"},
		{"a destination outside the list", inListed, url.Values{"destination": {"https://www.example.net/books"}}, fieldDestination, "error.destination.not_allowed"},
		{"an alias that is too short", inside, url.Values{"destination": {valid}, "alias": {"ab"}}, fieldAlias, "error.alias.length"},
		{"an alias with a capital and an underscore", inside, url.Values{"destination": {valid}, "alias": {"Spring_Books"}}, fieldAlias, "error.alias.characters"},
		{"an alias that is reserved", inside, url.Values{"destination": {valid}, "alias": {"login"}}, fieldAlias, "error.alias.reserved"},
		{"an alias that is taken", inside, url.Values{"destination": {valid}, "alias": {"taken-alias"}}, fieldAlias, "error.alias.taken"},
		{"a campaign parameter that is too long", inside, url.Values{"destination": {valid}, "utm_source": {strings.Repeat("s", 201)}}, fieldCampaign, "error.campaign.characters"},
		{"a campaign parameter the destination has", inside, url.Values{"destination": {valid + "?utm_source=there"}, "utm_source": {"here"}}, fieldCampaign, "error.campaign.conflict"},
		{"a title that is too long", inside, url.Values{"destination": {valid}, "title": {strings.Repeat("t", 201)}}, fieldTitle, "error.title.characters"},
		{"a tag twice", inside, url.Values{"destination": {valid}, "tags": {"spring, books, spring"}}, fieldTags, "error.tags.invalid"},
		{"an expiry that is not a moment", inside, url.Values{"destination": {valid}, "expires_at": {"next spring"}}, fieldExpiresAt, "error.expires_at.invalid"},
		{"an expiry that has passed", inside, url.Values{"destination": {valid}, "expires_at": {"2026-11-19T15:59"}}, fieldExpiresAt, "error.expires_at.past"},
		{"an expiry that is this moment", inside, url.Values{"destination": {valid}, "expires_at": {"2026-11-19T16:00:00"}}, fieldExpiresAt, "error.expires_at.past"},
		{"a workspace that is full", inFull, url.Values{"destination": {valid}}, fieldForm, "error.quota"},
	} {
		for _, lang := range languages {
			answer := s.send(t, c.session, "/"+string(lang)+"/links", c.fields)
			page := answer.Body.String()
			said := s.text(t, lang, c.key)
			if answer.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s, in %q = %d, want %d", c.what, lang, answer.Code, http.StatusUnprocessableEntity)
				continue
			}
			wants := map[string]string{
				"that the page is of an error":    "<title>" + s.text(t, lang, "form.error_mark") + " " + s.text(t, lang, "form.new.title") + " · Corinto</title>",
				"how many problems there are":     `<h2 id="problems-title">` + escaped(mustPlural(t, s, lang, "form.problems", 1)) + `</h2>`,
				"the summary, where it is heard":  `<div class="problems" role="alert" aria-labelledby="problems-title">`,
				"the form, to be sent again":      `<form method="post" action="/` + string(lang) + `/links" novalidate>`,
				"the heading of the form as ever": "<h1>" + s.text(t, lang, "form.new.title") + "</h1>",
			}
			if c.field == fieldForm {
				wants["the problem, in the summary"] = "<li>" + said + "</li>"
			} else {
				wants["the problem, next to its field"] = `<p class="error" id="` + c.field + `-error">` + said + `</p>`
				wants["the problem, in the summary"] = `<li><a href="#` + c.field + `">` + said + `</a></li>`
				wants["the field, named by what is wrong"] = c.field + `-error"`
			}
			for name, typed := range c.fields {
				if name != fieldAlias || c.session == inside {
					wants["what was typed in "+name] = `name="` + name + `" value="` + escaped(typed[0]) + `"`
				}
			}
			for what, want := range wants {
				if !strings.Contains(page, want) {
					t.Errorf("%s, in %q: the page does not show %s: no %q in it", c.what, lang, what, want)
				}
			}
			if marked := strings.Count(page, `aria-invalid="true"`); (c.field == fieldForm || c.field == fieldCampaign) != (marked == 0) || marked > 1 {
				t.Errorf("%s, in %q: %d fields are marked as wrong", c.what, lang, marked)
			}
			if strings.Count(page, `class="error"`) > 1 {
				t.Errorf("%s, in %q: more than one thing is said to be wrong", c.what, lang)
			}
		}
	}
	// Nothing was created by any of it.
	if links := len(s.all(t, shop)) + len(s.all(t, listed)) + len(s.all(t, full)); links != 2 {
		t.Errorf("the forms that were wrong left %d links, want the two there were", links)
	}

	// Two things wrong at once are both said, and counted.
	answer := s.send(t, inside, "/en/links", url.Values{"expires_at": {"2020-01-01T00:00"}})
	page := answer.Body.String()
	for _, want := range []string{
		escaped(mustPlural(t, s, english, "form.problems", 2)),
		`<p class="error" id="destination-error">` + s.text(t, english, "error.destination.required") + `</p>`,
		`<p class="error" id="expires_at-error">` + s.text(t, english, "error.expires_at.past") + `</p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("a form with two things wrong does not show %q", want)
		}
	}
	if strings.Index(page, `href="#destination"`) > strings.Index(page, `href="#expires_at"`) || strings.Count(page, `aria-invalid="true"`) != 2 {
		t.Error("the two problems are not listed in the order of the form, or not both fields are marked")
	}
}

// mustPlural returns a text that counts, as a page writes it.
func mustPlural(t *testing.T, s *stage, lang language, key string, n int64) string {
	t.Helper()
	found, err := s.texts.plural(lang, key, n)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// The form that changes a link shows what the link has and sets it to what
// the form says: an empty field takes away what was there.
func TestTheFormChangesALink(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{MayChooseAliases: true})
	expiry := time.Date(2027, time.March, 1, 8, 30, 45, 0, time.UTC)
	target := s.link(t, shop, link.Draft{
		Alias: "spring-books", Destination: "https://example.com/books?shelf=new", Title: `Spring "books" & more`, Tags: []string{"spring", "books"},
		Campaign: link.Campaign{Source: "newsletter", Medium: "email", Name: "spring"}, ExpiresAt: &expiry,
	})
	page := "/en/links/" + target.ID

	// The page of the link leads to the form, and the form holds the link:
	// its destination without the campaign, its tags in one field, its
	// expiry to the minute. The alias is not there: it never changes.
	if shown := s.ask(t, http.MethodGet, page, with(inside)); !strings.Contains(shown.Body.String(), `<a href="`+page+`/edit">`+s.text(t, english, "link.edit")+`</a>`) {
		t.Error("the page of a link does not lead to the form that changes it")
	}
	form := s.ask(t, http.MethodGet, page+"/edit", with(inside))
	s.shows(t, "the form that changes a link", form, http.StatusOK, english, "form.edit.title", "name", `Spring "books" & more`)
	for what, want := range map[string]string{
		"where it is sent": `<form method="post" action="` + page + `" novalidate>`,
		"the destination":  `name="destination" value="https://example.com/books?shelf=new"`,
		"the title":        `name="title" value="Spring &#34;books&#34; &amp; more"`,
		"the tags":         `name="tags" value="spring, books"`,
		"the source":       `name="utm_source" value="newsletter"`,
		"the campaign":     `name="utm_campaign" value="spring"`,
		"the expiry":       `name="expires_at" value="2027-03-01T08:30"`,
		"its button":       `<button type="submit" class="primary">` + s.text(t, english, "form.save") + `</button>`,
		"the way back":     `<a href="` + page + `">` + s.text(t, english, "form.cancel") + `</a>`,
	} {
		if !strings.Contains(form.Body.String(), want) {
			t.Errorf("the form that changes a link does not show %s: no %q in it", what, want)
		}
	}
	if strings.Contains(form.Body.String(), `name="alias"`) {
		t.Error("the form that changes a link asks for an alias")
	}

	// Sent as it came, it changes nothing: not the expiry either, whose
	// seconds the form does not show.
	sentTo(t, "sending the form as it came", s.send(t, inside, page, url.Values{
		"destination": {"https://example.com/books?shelf=new"}, "title": {`Spring "books" & more`}, "tags": {"spring, books"},
		"utm_source": {"newsletter"}, "utm_medium": {"email"}, "utm_campaign": {"spring"}, "expires_at": {"2027-03-01T08:30"},
	}), page)
	if same := s.only(t, shop); same.Version != target.Version || !same.ExpiresAt.Equal(expiry) {
		t.Errorf("a form sent as it came left the version %d and the expiry %v; want %d and %v", same.Version, same.ExpiresAt, target.Version, expiry)
	}

	// Changed: another destination and other tags, and no title, campaign
	// or expiry.
	sentTo(t, "changing a link", s.send(t, inside, page, url.Values{
		"destination": {"https://example.com/books/summer"}, "title": {""}, "tags": {"summer"},
		"utm_source": {""}, "utm_medium": {""}, "utm_campaign": {""}, "expires_at": {""},
	}), page)
	changed := s.only(t, shop)
	if changed.Destination != "https://example.com/books/summer" || changed.Resolved != "https://example.com/books/summer" || changed.Title != "" ||
		!slices.Equal(changed.Tags, []string{"summer"}) || changed.Campaign != (link.Campaign{}) || changed.ExpiresAt != nil ||
		changed.Identifier != "spring-books" || changed.Version <= target.Version {
		t.Errorf("the link after the change is %+v", changed)
	}

	// What is wrong with it is said in the form that changes it, and
	// nothing changes.
	wrong := s.send(t, inside, "/es"+strings.TrimPrefix(page, "/en"), url.Values{"destination": {"ftp://example.com/"}, "title": {"Otro"}})
	if wrong.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(wrong.Body.String(), `<p class="error" id="destination-error">`+s.text(t, spanish, "error.destination.scheme")+`</p>`) ||
		!strings.Contains(wrong.Body.String(), `<form method="post" action="/es/links/`+target.ID+`" novalidate>`) ||
		!strings.Contains(wrong.Body.String(), "<title>"+s.text(t, spanish, "form.error_mark")+" "+s.text(t, spanish, "form.edit.title", "name", "spring-books")) {
		t.Errorf("a change that is wrong = %d, without the form that says so", wrong.Code)
	}
	if after := s.only(t, shop); after.Version != changed.Version {
		t.Error("a change that was wrong changed the link")
	}

	// A link that has expired keeps its expiry while the rest of it is
	// changed, and cannot be given another moment that has passed.
	past := today.Add(-48 * time.Hour)
	over := s.link(t, shop, link.Draft{Title: "Winter", ExpiresAt: &past})
	overPage := "/en/links/" + over.ID
	sentTo(t, "changing the title of a link that has expired", s.send(t, inside, overPage, url.Values{
		"destination": {over.Destination}, "title": {"Winter clearance"}, "expires_at": {past.Format("2006-01-02T15:04")},
	}), overPage)
	if kept, err := s.store.Get(t.Context(), shop, over.ID); err != nil || kept.Title != "Winter clearance" || kept.ExpiresAt == nil || !kept.ExpiresAt.Equal(past) {
		t.Errorf("the link that has expired is %+v, %v after its title was changed", kept, err)
	}
	if again := s.send(t, inside, overPage, url.Values{"destination": {over.Destination}, "expires_at": {"2026-11-18T00:00"}}); again.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(again.Body.String(), s.text(t, english, "error.expires_at.past")) {
		t.Errorf("giving a link another moment that has passed = %d", again.Code)
	}
}

// A link is disabled, enabled and deleted with forms, and deleting asks
// first: it cannot be undone.
func TestALinkIsSwitchedAndDeletedWithForms(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	target := s.link(t, shop, link.Draft{Title: "Spring <books>"})
	page := "/es/links/" + target.ID
	state := func() link.State {
		t.Helper()
		found, err := s.store.Get(t.Context(), shop, target.ID)
		if err != nil {
			t.Fatal(err)
		}
		return found.State
	}
	button := func(action, key string) string {
		return `<form method="post" action="` + page + "/" + action + `"><button type="submit">` + s.text(t, spanish, key) + `</button></form>`
	}

	shown := s.ask(t, http.MethodGet, page, with(inside)).Body.String()
	if !strings.Contains(shown, button("disable", "link.disable")) || strings.Contains(shown, page+"/enable") ||
		!strings.Contains(shown, `<nav class="actions" aria-label="`+s.text(t, spanish, "link.actions")+`">`) {
		t.Error("the page of an active link does not offer to disable it, and that alone")
	}
	sentTo(t, "disabling a link", s.send(t, inside, page+"/disable", url.Values{}), page)
	if state() != link.StateDisabled {
		t.Fatalf("the link is %q after it was disabled", state())
	}
	shown = s.ask(t, http.MethodGet, page, with(inside)).Body.String()
	if !strings.Contains(shown, button("enable", "link.enable")) || strings.Contains(shown, page+"/disable") ||
		!strings.Contains(shown, "<dd>"+s.text(t, spanish, "link.state.disabled")+"</dd>") {
		t.Error("the page of a disabled link does not say so and offer to enable it, and that alone")
	}
	// Disabling it again is not an error, and enabling it brings it back.
	sentTo(t, "disabling a link again", s.send(t, inside, page+"/disable", url.Values{}), page)
	sentTo(t, "enabling a link", s.send(t, inside, page+"/enable", url.Values{}), page)
	if state() != link.StateActive {
		t.Fatalf("the link is %q after it was enabled", state())
	}

	// Deleting: the page leads to a question, and the question deletes
	// nothing.
	if !strings.Contains(shown, `<a href="`+page+`/delete">`+s.text(t, spanish, "link.delete")+`</a>`) {
		t.Error("the page of a link does not lead to deleting it")
	}
	question := s.ask(t, http.MethodGet, page+"/delete", with(inside))
	s.shows(t, "the question before deleting", question, http.StatusOK, spanish, "delete.title", "name", "Spring <books>")
	for what, want := range map[string]string{
		"that it cannot be undone": s.text(t, spanish, "delete.text"),
		"the button that deletes":  `<form method="post" action="` + page + `/delete">`,
		"what the button says":     `<button type="submit" class="danger">` + s.text(t, spanish, "delete.confirm") + `</button>`,
		"the way back":             `<a href="` + page + `">` + s.text(t, spanish, "delete.keep") + `</a>`,
	} {
		if !strings.Contains(question.Body.String(), want) {
			t.Errorf("the question before deleting does not show %s: no %q in it", what, want)
		}
	}
	if state() != link.StateActive {
		t.Fatal("asking whether to delete a link deleted it")
	}
	sentTo(t, "deleting a link", s.send(t, inside, page+"/delete", url.Values{}), "/es/links")
	if _, err := s.store.Get(t.Context(), shop, target.ID); !errors.Is(err, link.ErrNotFound) {
		t.Fatalf("the link after it was deleted: %v, want %v", err, link.ErrNotFound)
	}
	// It is gone from the list, and every address of it is of nothing.
	if list := s.ask(t, http.MethodGet, "/es/links", with(inside)).Body.String(); strings.Contains(list, target.ID) || !strings.Contains(list, s.text(t, spanish, "links.empty")) {
		t.Error("the list still shows the link that was deleted")
	}
	for _, path := range []string{page, page + "/edit", page + "/delete"} {
		s.shows(t, "GET "+path+" after the link was deleted", s.ask(t, http.MethodGet, path, with(inside)), http.StatusNotFound, spanish, "notfound.title")
	}
	for _, path := range []string{page, page + "/disable", page + "/enable", page + "/delete"} {
		s.shows(t, "POST "+path+" after the link was deleted", s.send(t, inside, path, url.Values{"destination": {"https://example.com/"}}), http.StatusNotFound, spanish, "notfound.title")
	}
}

// A link the operator blocked can be read and deleted by its workspace,
// and nothing else: a block that the workspace could edit away would not
// be the operator's.
func TestALinkTheOperatorBlockedCanOnlyBeDeleted(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	target := s.link(t, shop, link.Draft{Title: "Spring"})
	if _, err := s.store.Block(t.Context(), target.Identifier); err != nil {
		t.Fatal(err)
	}
	page := "/en/links/" + target.ID

	shown := s.ask(t, http.MethodGet, page, with(inside)).Body.String()
	if !strings.Contains(shown, `<p class="notice">`+s.text(t, english, "error.blocked")+`</p>`) || strings.Contains(shown, page+"/edit") ||
		strings.Contains(shown, page+"/disable") || strings.Contains(shown, page+"/enable") || !strings.Contains(shown, `<a href="`+page+`/delete">`) {
		t.Error("the page of a blocked link does not say so and offer to delete it, and that alone")
	}
	sentTo(t, "the form that changes a blocked link", s.ask(t, http.MethodGet, page+"/edit", with(inside)), page)
	refused := s.send(t, inside, page, url.Values{"destination": {"https://example.com/elsewhere"}})
	if refused.Code != http.StatusUnprocessableEntity || !strings.Contains(refused.Body.String(), "<li>"+s.text(t, english, "error.blocked")+"</li>") {
		t.Errorf("changing a blocked link = %d, without the form that says why not", refused.Code)
	}
	sentTo(t, "disabling a blocked link", s.send(t, inside, page+"/disable", url.Values{}), page)
	sentTo(t, "enabling a blocked link", s.send(t, inside, page+"/enable", url.Values{}), page)
	if found := s.only(t, shop); found.State != link.StateBlocked || found.Destination != target.Destination {
		t.Fatalf("the blocked link is %+v after what was tried on it", found)
	}
	sentTo(t, "deleting a blocked link", s.send(t, inside, page+"/delete", url.Values{}), "/en/links")
	if left := s.all(t, shop); len(left) != 0 {
		t.Errorf("the blocked link is still there after it was deleted")
	}
}

// One point of authorization (PLAN.md section 9): no form reaches the link
// of another workspace, which is answered like one that does not exist,
// and none is served or taken without a session.
func TestNoFormReachesALinkOfAnotherWorkspace(t *testing.T) {
	s := newStage(t)
	owner, _ := s.shop(t, link.WorkspaceDraft{})
	_, stranger := s.shop(t, link.WorkspaceDraft{Name: "Globex"})
	target := s.link(t, owner, link.Draft{Title: "Spring"})
	page := "/en/links/" + target.ID
	fields := url.Values{"destination": {"https://example.com/taken-over"}, "title": {"Theirs"}}

	for _, id := range []string{target.ID, uuid.NewV7().String(), "spring"} {
		at := "/en/links/" + id
		for _, path := range []string{at + "/edit", at + "/delete"} {
			s.shows(t, "GET "+path+" from another workspace", s.ask(t, http.MethodGet, path, with(stranger)), http.StatusNotFound, english, "notfound.title")
		}
		for _, path := range []string{at, at + "/disable", at + "/enable", at + "/delete"} {
			s.shows(t, "POST "+path+" from another workspace", s.send(t, stranger, path, fields), http.StatusNotFound, english, "notfound.title")
		}
	}
	// Without a session every one of them leads to the door, and a form
	// that creates a link creates none.
	for _, path := range []string{"/en/links/new", page + "/edit", page + "/delete"} {
		sentTo(t, "GET "+path+" without a session", s.ask(t, http.MethodGet, path), "/en/")
	}
	for _, path := range []string{"/en/links", page, page + "/disable", page + "/enable", page + "/delete"} {
		sentTo(t, "POST "+path+" without a session", s.ask(t, http.MethodPost, path, filled(fields)), "/en/")
	}
	if found := s.only(t, owner); found.Version != target.Version || found.State != link.StateActive || found.Title != "Spring" {
		t.Errorf("the link is %+v after what another workspace and nobody tried on it", found)
	}
}

// Nothing changes state on a GET, and a form sent from another site is
// refused (the engineering rules), for every form of a link.
func TestTheFormsOfALinkTakeNoGetAndNothingFromAnotherSite(t *testing.T) {
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	target := s.link(t, shop, link.Draft{Title: "Spring"})
	page := "/es/links/" + target.ID
	fields := url.Values{"destination": {"https://example.com/elsewhere"}}

	for _, path := range []string{page + "/disable", page + "/enable"} {
		s.shows(t, "GET "+path, s.ask(t, http.MethodGet, path, with(inside)), http.StatusNotFound, spanish, "notfound.title")
	}
	for _, path := range []string{"/es/links", page, page + "/disable", page + "/delete"} {
		for _, site := range []string{"cross-site", "same-site"} {
			answer := s.send(t, inside, path, fields, saying("Sec-Fetch-Site", site))
			s.shows(t, "POST "+path+" from a page that is "+site, answer, http.StatusForbidden, spanish, "refused.title")
		}
	}
	// What a form says is in its body: a destination in the address is not
	// the destination of the form.
	inAddress := s.send(t, inside, "/es/links?destination=https%3A%2F%2Fexample.com%2Ffrom-the-address", url.Values{})
	if inAddress.Code != http.StatusUnprocessableEntity || !strings.Contains(inAddress.Body.String(), s.text(t, spanish, "error.destination.required")) {
		t.Errorf("a form with its destination in the address = %d, want it to lack a destination", inAddress.Code)
	}
	// A form larger than a form of the panel is: not one of its own.
	large := url.Values{"destination": {"https://example.com/"}, "title": {strings.Repeat("t", limits.APIMaxBodyBytes)}}
	s.shows(t, "a form that is too large", s.send(t, inside, "/es/links", large), http.StatusNotFound, spanish, "notfound.title")
	if found := s.only(t, shop); found.Version != target.Version || found.State != link.StateActive {
		t.Errorf("the link is %+v after the requests that were refused", found)
	}
}

// PLAN.md section 9: sixty writes a minute. A session of the panel has the
// allowance of a key of the API, of its own.
func TestASessionMakesSixtyChangesAMinute(t *testing.T) {
	if limits.APIWritesPerMinute != 60 {
		t.Errorf("a key writes %d times a minute, want 60", limits.APIWritesPerMinute)
	}
	s := newStage(t)
	shop, inside := s.shop(t, link.WorkspaceDraft{})
	target := s.link(t, shop, link.Draft{})
	page := "/en/links/" + target.ID
	switches := []string{page + "/disable", page + "/enable"}

	for i := range limits.APIWritesPerMinute {
		sentTo(t, "change "+strconv.Itoa(i+1)+" of a minute", s.send(t, inside, switches[i%2], url.Values{}), page)
	}
	limited := s.send(t, inside, page+"/disable", url.Values{})
	s.shows(t, "the change after the sixtieth", limited, http.StatusTooManyRequests, english, "busy.title")
	if limited.Header().Get("Retry-After") != "1" || !strings.Contains(limited.Body.String(), s.text(t, english, "busy.text")) ||
		!strings.Contains(limited.Body.String(), `action="/en/logout"`) {
		t.Errorf("the change after the sixtieth: Retry-After = %q, or the page does not say why or is not of the session", limited.Header().Get("Retry-After"))
	}
	if found := s.only(t, shop); found.State != link.StateActive {
		t.Errorf("the link is %q: the change after the sixtieth was made", found.State)
	}
	// A form that would have been wrong counts too: the limit is on what
	// is asked. Reading is not limited, and another session has its own.
	s.shows(t, "a form that is wrong, past the limit", s.send(t, inside, "/en/links", url.Values{}), http.StatusTooManyRequests, english, "busy.title")
	if shown := s.ask(t, http.MethodGet, page, with(inside)); shown.Code != http.StatusOK {
		t.Errorf("the page of the link for a session that reached its limit = %d", shown.Code)
	}
	sentTo(t, "a change from another session", s.send(t, s.sessionOf(t, shop), page+"/disable", url.Values{}), page)

	// A second earns one change back, and no more.
	s.clock.advance(time.Second)
	sentTo(t, "a change a second later", s.send(t, inside, page+"/enable", url.Values{}), page)
	s.shows(t, "a second change a second later", s.send(t, inside, page+"/disable", url.Values{}), http.StatusTooManyRequests, english, "busy.title")
}
