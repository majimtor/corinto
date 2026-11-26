package panel

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/majimtor/corinto/internal/link"
)

// linksPerPage is how many links a page of the list shows.
const linksPerPage = 50

// linksView is what the list of the links of a workspace is rendered with.
type linksView struct {
	view
	Links []listedLink
	// Next is where the links after these are listed, or empty on the last
	// page.
	Next string
}

// listedLink is a row of the list: a link, what the page calls it, the key
// of the text that says its state and where its own page is.
type listedLink struct {
	Link     link.Link
	Name     string
	StateKey string
	Page     string
}

// linksPage serves the links of the workspace of a session in one language,
// in the order in which they were created, a page of them at a time.
func (p *panel) linksPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		found, more, err := p.settings.Links.List(r.Context(), in.principal.WorkspaceID, r.URL.Query().Get("after"), linksPerPage)
		if errors.Is(err, link.ErrPage) {
			// A page that starts after something that is not a link.
			p.missing(w, r, lang)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		now := p.settings.Now().UTC()
		here := "/" + string(lang) + "/links"
		shown := linksView{view: p.viewIn(r, lang, in), Links: make([]listedLink, len(found))}
		for i, one := range found {
			name := one.Title
			if name == "" {
				name = one.Identifier
			}
			shown.Links[i] = listedLink{Link: one, Name: name, StateKey: stateKey(one, now), Page: here + "/" + one.ID}
		}
		if more {
			shown.Next = here + "?" + url.Values{"after": {found[len(found)-1].ID}}.Encode()
		}
		p.render(w, r, http.StatusOK, lang, pageLinks, shown)
	}
}
