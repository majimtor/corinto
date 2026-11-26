package panel

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/majimtor/corinto/internal/link"
)

// entryView is what the door of the panel is rendered with.
type entryView struct {
	view
	// Demos are the workspaces that can be entered, and Enter where the
	// form of each is sent.
	Demos []link.Workspace
	Enter string
}

// entryPage serves the door of the panel in one language: the demo
// workspaces, each with the button that enters it. Whoever is inside
// already is sent to the links of their workspace.
func (p *panel) entryPage(lang language) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, in, err := p.identify(r)
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		if in {
			redirect(w, "/"+string(lang)+"/links", http.StatusSeeOther)
			return
		}
		demos, err := p.settings.Links.Demos(r.Context())
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		p.render(w, r, http.StatusOK, lang, pageEntry,
			entryView{view: p.viewOf(r, lang), Demos: demos, Enter: "/" + string(lang) + "/session"})
	}
}

// root sends the address with no language to the door in the language the
// browser asks for, or in Spanish, the source, when it asks for neither.
func (p *panel) root(w http.ResponseWriter, r *http.Request) {
	// The answer depends on the header: a cache between the two must not
	// give one reader the door of another.
	w.Header().Set("Vary", "Accept-Language")
	redirect(w, "/"+string(preferred(r.Header.Get("Accept-Language")))+"/", http.StatusSeeOther)
}

// preferred returns the language of the panel that an Accept-Language
// header asks for the most: the one with the greatest weight, and among
// equals the one named first. A language is known by its first subtag, so
// es-MX asks for Spanish. Spanish answers a header that names neither.
func preferred(header string) language {
	chosen, most := spanish, 0.0
entries:
	for _, entry := range strings.Split(header, ",") {
		parts := strings.Split(entry, ";")
		weight := 1.0
		for _, parameter := range parts[1:] {
			written, found := strings.CutPrefix(strings.TrimSpace(parameter), "q=")
			if !found {
				continue
			}
			parsed, err := strconv.ParseFloat(written, 64)
			if err != nil || parsed < 0 || parsed > 1 {
				// A weight that is not one says nothing about its language.
				continue entries
			}
			weight = parsed
		}
		primary, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(parts[0])), "-")
		for _, known := range languages {
			if primary == string(known) && weight > most {
				chosen, most = known, weight
			}
		}
	}
	return chosen
}
