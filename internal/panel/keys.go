package panel

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/majimtor/corinto/internal/auth"
)

// The fields of the form of a key, by the name each has in the form. The
// expiry has the name it has in the form of a link.
const (
	fieldKeyName   = "name"
	fieldKeyScopes = "scopes"
)

// offeredScopes are the scopes the form of a key offers: the ones an
// operation of the API asks for today. A scope that nothing asks for yet
// is not offered, and joins them with the first operation that does.
var offeredScopes = []auth.Scope{auth.ScopeLinksRead, auth.ScopeLinksWrite, auth.ScopeMetricsRead}

// The texts for what is wrong with the form of a key: what the form finds
// by itself, and what the domain refuses.
const (
	keyNameRequired = "error.key.name.required"
	keyScopes       = "error.key.scopes"
)

// keyRefusals are the texts for what the domain refuses of a key, by the
// rule that was broken.
var keyRefusals = []struct {
	rule error
	problem
}{
	{auth.ErrName, problem{fieldKeyName, "error.key.name.characters"}},
	{auth.ErrScopes, problem{fieldKeyScopes, keyScopes}},
	{auth.ErrExpiry, problem{fieldExpiresAt, keyExpiryPast}},
	{auth.ErrQuotaExceeded, problem{fieldForm, "error.key.quota"}},
}

// keyWrong is what is wrong with each field of the form of a key: the key
// of the text that says it, or nothing.
type keyWrong struct {
	Name, Scopes, ExpiresAt string
}

// scopeKey returns the key of the text that says what a scope lets a key
// do.
func scopeKey(scope auth.Scope) string {
	return "scope." + strings.ReplaceAll(string(scope), ":", ".")
}

// keyState returns the key of the text that says the state of a key in
// force: one whose time is up no longer authenticates, and stays in the
// list until it is revoked.
func keyState(key auth.Key, now time.Time) string {
	if key.ExpiresAt != nil && !key.ExpiresAt.After(now) {
		return "key.state.expired"
	}
	return "key.state.in_force"
}

// listedKey is a row of the list of keys: a key, the key of the text that
// says its state and where it is revoked.
type listedKey struct {
	Key      auth.Key
	StateKey string
	Revoke   string
}

// keysView is what the list of the keys of a workspace is rendered with.
type keysView struct {
	view
	Keys []listedKey
	// New is where a key is issued.
	New string
}

// keysPage serves the keys of the workspace of a session that are in
// force, the oldest first. A workspace has a handful of them at the most,
// its quota, so the list has one page, and each key a box of its own: what
// a key has does not fit in the row of a table.
func (p *panel) keysPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		found, err := p.settings.Keys.List(r.Context(), in.principal.WorkspaceID)
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		now := p.settings.Now()
		here := "/" + string(lang) + "/keys"
		shown := keysView{view: p.viewIn(r, lang, in), Keys: make([]listedKey, len(found)), New: here + "/new"}
		for i, one := range found {
			shown.Keys[i] = listedKey{Key: one, StateKey: keyState(one, now), Revoke: here + "/" + one.ID + "/revoke"}
		}
		p.render(w, r, http.StatusOK, lang, pageKeys, shown)
	}
}

// scopeChoice is one of the scopes the form of a key offers: the scope,
// what its box is called in the page, the key of the text that says what
// it lets a key do, and whether it is chosen.
type scopeChoice struct {
	Scope   auth.Scope
	ID      string
	NameKey string
	Chosen  bool
}

// keyFormView is what the form of a key is rendered with.
type keyFormView struct {
	view
	// Action is where the form is sent, and Cancel where whoever gives it
	// up goes back to.
	Action, Cancel string
	// Name and ExpiresAt are what was typed, and Scopes the ones the form
	// offers, with the ones that were chosen.
	Name, ExpiresAt string
	Scopes          []scopeChoice
	// Problems is what is wrong with what was sent, in the order of the
	// form, and Wrong the same by field.
	Problems []problem
	Wrong    keyWrong
}

// Found is how many problems the form has, as the catalogue counts.
func (v keyFormView) Found() int64 { return int64(len(v.Problems)) }

// keyFormOf returns the form of a key with what was sent in it, or empty
// when nothing was.
func (p *panel) keyFormOf(r *http.Request, lang language, in member) keyFormView {
	here := "/" + string(lang) + "/keys"
	shown := keyFormView{
		view: p.viewIn(r, lang, in), Action: here, Cancel: here,
		Name: strings.TrimSpace(r.PostForm.Get(fieldKeyName)), ExpiresAt: strings.TrimSpace(r.PostForm.Get(fieldExpiresAt)),
	}
	for _, scope := range offeredScopes {
		shown.Scopes = append(shown.Scopes, scopeChoice{
			Scope: scope, ID: "scope-" + strings.ReplaceAll(string(scope), ":", "-"), NameKey: scopeKey(scope),
			Chosen: slices.Contains(r.PostForm[fieldKeyScopes], string(scope)),
		})
	}
	return shown
}

// keyForm renders the form of a key, with what is wrong with what was sent
// when something is: then the answer is 422.
func (p *panel) keyForm(w http.ResponseWriter, r *http.Request, lang language, shown keyFormView, problems ...problem) {
	status := http.StatusOK
	if len(problems) > 0 {
		status = http.StatusUnprocessableEntity
		shown.Problems = problems
		for _, found := range problems {
			switch found.Field {
			case fieldKeyName:
				shown.Wrong.Name = found.Key
			case fieldKeyScopes:
				shown.Wrong.Scopes = found.Key
			case fieldExpiresAt:
				shown.Wrong.ExpiresAt = found.Key
			}
		}
	}
	p.render(w, r, status, lang, pageKeyForm, shown)
}

// newKeyPage serves the form that issues a key.
func (p *panel) newKeyPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		p.keyForm(w, r, lang, p.keyFormOf(r, lang, in))
	}
}

// issuedView is what the page that shows a key that was just issued is
// rendered with.
type issuedView struct {
	view
	Key auth.Key
	// Credential is the text the holder of the key sends. This page is the
	// only place it is ever shown.
	Credential string
	// Back is where the keys are listed.
	Back string
}

// issueKey issues the key a form describes and answers with it. The answer
// to the form is the only place the credential is ever shown: sending
// whoever asked to another page, as the form of a link does, would need the
// credential kept somewhere until that page is asked for, and it is kept
// nowhere (PLAN.md section 9). So a form sent again issues another key,
// within the quota of the workspace.
func (p *panel) issueKey(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		shown := p.keyFormOf(r, lang, in)
		var problems []problem
		if shown.Name == "" {
			problems = append(problems, problem{fieldKeyName, keyNameRequired})
		}
		// Only what the form offers is taken, and at least one of them: a
		// scope sent by hand is not a choice of the form.
		asked := r.PostForm[fieldKeyScopes]
		scopes := make([]auth.Scope, 0, len(asked))
		for _, scope := range asked {
			scopes = append(scopes, auth.Scope(scope))
		}
		if len(scopes) == 0 || slices.ContainsFunc(scopes, func(scope auth.Scope) bool { return !slices.Contains(offeredScopes, scope) }) {
			problems = append(problems, problem{fieldKeyScopes, keyScopes})
		}
		expiry, wrong := expiryOf(shown.ExpiresAt, p.settings.Now())
		if wrong != "" {
			problems = append(problems, problem{fieldExpiresAt, wrong})
		}
		if len(problems) > 0 {
			p.keyForm(w, r, lang, shown, problems...)
			return
		}

		issued, credential, err := p.settings.Keys.Issue(r.Context(), in.principal.WorkspaceID, auth.Draft{Name: shown.Name, Scopes: scopes, ExpiresAt: expiry})
		for _, known := range keyRefusals {
			if errors.Is(err, known.rule) {
				p.keyForm(w, r, lang, shown, known.problem)
				return
			}
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		p.render(w, r, http.StatusCreated, lang, pageKeyIssued, issuedView{
			view: p.viewIn(r, lang, in), Key: issued, Credential: credential, Back: "/" + string(lang) + "/keys",
		})
	}
}

// revokeView is what the page that asks before revoking a key is rendered
// with.
type revokeView struct {
	view
	Name string
	// Action is where the form that revokes is sent, and Keep where
	// whoever thinks better of it goes back to.
	Action, Keep string
}

// revokeKeyPage asks before a key is revoked: it cannot be undone, and
// whatever holds the key stops working with the next request it makes.
func (p *panel) revokeKeyPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		found, err := p.settings.Keys.Get(r.Context(), in.principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, auth.ErrNotFound) {
			p.missing(w, r, lang)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		list := "/" + string(lang) + "/keys"
		p.render(w, r, http.StatusOK, lang, pageKeyRevoke, revokeView{
			view: p.viewIn(r, lang, in), Name: found.Name, Action: list + "/" + found.ID + "/revoke", Keep: list,
		})
	}
}

// revokeKey revokes a key, and sends whoever asked to the list it is no
// longer in.
func (p *panel) revokeKey(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		err := p.settings.Keys.Revoke(r.Context(), in.principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, auth.ErrNotFound) {
			p.missing(w, r, lang)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		redirect(w, "/"+string(lang)+"/keys", http.StatusSeeOther)
	}
}
