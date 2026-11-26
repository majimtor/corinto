package panel

import (
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/majimtor/corinto/internal/limits"
	"github.com/majimtor/corinto/internal/link"
)

// The fields of the form of a link, by the name each has in the form.
const (
	fieldDestination = "destination"
	fieldAlias       = "alias"
	fieldTitle       = "title"
	fieldTags        = "tags"
	fieldCampaign    = "campaign"
	fieldExpiresAt   = "expires_at"
	// fieldForm is where what is wrong with the whole form is said.
	fieldForm = "form"
)

// expiryLayouts are how a browser writes the moment of a field of date
// and time, with its seconds or without them. The panel reads it in UTC,
// and says so next to the field.
var expiryLayouts = []string{"2006-01-02T15:04", "2006-01-02T15:04:05"}

// problem is what is wrong with a field of a form: the field, and the key
// of the text that says it.
type problem struct {
	Field, Key string
}

// Linked reports whether the problem is of a field, which the page can
// lead to, and not of the form as a whole.
func (p problem) Linked() bool { return p.Field != fieldForm }

// wrong is what is wrong with each field of the form of a link: the key of
// the text that says it, or nothing.
type wrong struct {
	Destination, Alias, Title, Tags, Campaign, ExpiresAt string
}

// of returns what is wrong, field by field, from a list of problems.
func (w wrong) of(problems []problem) wrong {
	for _, found := range problems {
		switch found.Field {
		case fieldDestination:
			w.Destination = found.Key
		case fieldAlias:
			w.Alias = found.Key
		case fieldTitle:
			w.Title = found.Key
		case fieldTags:
			w.Tags = found.Key
		case fieldCampaign:
			w.Campaign = found.Key
		case fieldExpiresAt:
			w.ExpiresAt = found.Key
		}
	}
	return w
}

// The texts for what the domain refuses of a link, by the rule that was
// broken. The API has the same table with its own codes (internal/api,
// rejectedBy): the two name the same rules and share no code, because one
// answers a program and the other a person.
var refusals = []struct {
	rule error
	problem
}{
	{link.ErrDestinationTooLong, problem{fieldDestination, "error.destination.too_long"}},
	{link.ErrDestinationCharacters, problem{fieldDestination, "error.destination.characters"}},
	{link.ErrDestinationScheme, problem{fieldDestination, "error.destination.scheme"}},
	{link.ErrDestinationCredentials, problem{fieldDestination, "error.destination.credentials"}},
	{link.ErrDestinationHost, problem{fieldDestination, "error.destination.host"}},
	{link.ErrDestinationPort, problem{fieldDestination, "error.destination.port"}},
	{link.ErrDestinationDotSegment, problem{fieldDestination, "error.destination.dot_segment"}},
	{link.ErrDestinationRedirects, problem{fieldDestination, "error.destination.redirect_domain"}},
	{link.ErrDestinationNotAllowed, problem{fieldDestination, "error.destination.not_allowed"}},
	{link.ErrAliasLength, problem{fieldAlias, "error.alias.length"}},
	{link.ErrAliasCharacters, problem{fieldAlias, "error.alias.characters"}},
	{link.ErrAliasReserved, problem{fieldAlias, "error.alias.reserved"}},
	{link.ErrAliasTaken, problem{fieldAlias, "error.alias.taken"}},
	{link.ErrCampaignValue, problem{fieldCampaign, "error.campaign.characters"}},
	{link.ErrCampaignConflict, problem{fieldCampaign, "error.campaign.conflict"}},
	{link.ErrTitle, problem{fieldTitle, "error.title.characters"}},
	{link.ErrTags, problem{fieldTags, "error.tags.invalid"}},
	{link.ErrQuotaExceeded, problem{fieldForm, "error.quota"}},
	{link.ErrLinkBlocked, problem{fieldForm, "error.blocked"}},
}

// The texts for what the form itself finds wrong, before the domain is
// asked.
const (
	keyDestinationRequired = "error.destination.required"
	keyExpiryInvalid       = "error.expires_at.invalid"
	keyExpiryPast          = "error.expires_at.past"
)

// linkValues is what the fields of the form hold, as text: what was typed,
// or what the link has.
type linkValues struct {
	Destination, Alias, Title, Tags string
	Source, Medium, Campaign        string
	Term, Content                   string
	ExpiresAt                       string
}

// formView is what the form of a link is rendered with.
type formView struct {
	view
	// New says that the form creates a link. Otherwise it changes the one
	// Name names.
	New  bool
	Name string
	// Action is where the form is sent, and Cancel where whoever gives it
	// up goes back to.
	Action, Cancel string
	// Aliases says that the workspace may choose the alias of a link, and
	// Allowed where its destinations may lead, when it has a list.
	Aliases bool
	Allowed []string
	Values  linkValues
	// Problems is what is wrong with what was sent, in the order of the
	// form, and Wrong the same by field, for each field to say its own.
	Problems []problem
	Wrong    wrong
}

// Found is how many problems the form has, as the catalogue counts.
func (v formView) Found() int64 { return int64(len(v.Problems)) }

// valuesOf returns what the form shows of a link that exists.
func valuesOf(found link.Link) linkValues {
	values := linkValues{
		Destination: found.Destination, Title: found.Title, Tags: strings.Join(found.Tags, ", "),
		Source: found.Campaign.Source, Medium: found.Campaign.Medium, Campaign: found.Campaign.Name,
		Term: found.Campaign.Term, Content: found.Campaign.Content,
	}
	if found.ExpiresAt != nil {
		values.ExpiresAt = found.ExpiresAt.UTC().Format(expiryLayouts[0])
	}
	return values
}

// sent reads the form of a link as it was sent. Every field is trimmed: a
// space around what was pasted is not part of it.
func sent(r *http.Request) linkValues {
	field := func(name string) string { return strings.TrimSpace(r.PostForm.Get(name)) }
	return linkValues{
		Destination: field(fieldDestination), Alias: field(fieldAlias), Title: field(fieldTitle), Tags: field(fieldTags),
		Source: field("utm_source"), Medium: field("utm_medium"), Campaign: field("utm_campaign"),
		Term: field("utm_term"), Content: field("utm_content"), ExpiresAt: field(fieldExpiresAt),
	}
}

// tags returns the tags of a field: what the commas separate, without the
// spaces around each and without the empty ones.
func (v linkValues) tags() []string {
	tags := []string{}
	for _, tag := range strings.Split(v.Tags, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

// campaign returns the campaign parameters of the fields.
func (v linkValues) campaign() link.Campaign {
	return link.Campaign{Source: v.Source, Medium: v.Medium, Name: v.Campaign, Term: v.Term, Content: v.Content}
}

// expiry reads the field of the expiry.
func (v linkValues) expiry(now time.Time) (*time.Time, string) {
	return expiryOf(v.ExpiresAt, now)
}

// expiryOf reads what a field of an expiry holds: no moment when it is
// empty, and otherwise one that is later than now, as the API asks of it.
// It returns the key of what is wrong with it, if anything is. The form of
// a link and the form of a key read theirs here.
func expiryOf(typed string, now time.Time) (*time.Time, string) {
	if typed == "" {
		return nil, ""
	}
	for _, layout := range expiryLayouts {
		if at, err := time.ParseInLocation(layout, typed, time.UTC); err == nil {
			if !at.After(now) {
				return nil, keyExpiryPast
			}
			return &at, ""
		}
	}
	return nil, keyExpiryInvalid
}

// checked reads what the form can check by itself: that there is a
// destination, and the expiry.
func (v linkValues) checked(now time.Time) (*time.Time, []problem) {
	var found []problem
	if v.Destination == "" {
		found = append(found, problem{fieldDestination, keyDestinationRequired})
	}
	expiry, wrong := v.expiry(now)
	if wrong != "" {
		found = append(found, problem{fieldExpiresAt, wrong})
	}
	return expiry, found
}

// refused returns the problem that an error of the domain is, if it is one
// a person can do something about.
func refused(err error) (problem, bool) {
	for _, known := range refusals {
		if errors.Is(err, known.rule) {
			return known.problem, true
		}
	}
	return problem{}, false
}

// form renders the form of a link, with what is wrong with what was sent
// when something is: then the answer is 422, and the page says so before
// anything else.
func (p *panel) form(w http.ResponseWriter, r *http.Request, lang language, shown formView, problems ...problem) {
	status := http.StatusOK
	if len(problems) > 0 {
		status = http.StatusUnprocessableEntity
		shown.Problems, shown.Wrong = problems, wrong{}.of(problems)
	}
	p.render(w, r, status, lang, pageLinkForm, shown)
}

// formOf returns the form of a workspace, empty: the one that creates a
// link.
func (p *panel) formOf(r *http.Request, lang language, in member) formView {
	door := "/" + string(lang) + "/links"
	shown := formView{view: p.viewIn(r, lang, in), New: true, Action: door, Cancel: door, Aliases: in.workspace.MayChooseAliases}
	for _, prefix := range in.workspace.Allowed {
		shown.Allowed = append(shown.Allowed, prefix.Scheme+"://"+prefix.Host+prefix.Path)
	}
	return shown
}

// newLinkPage serves the form that creates a link.
func (p *panel) newLinkPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		p.form(w, r, lang, p.formOf(r, lang, in))
	}
}

// createLink creates the link a form describes, and sends whoever sent it
// to the page of the link. What is wrong with the form is said in it, with
// what was typed still there.
func (p *panel) createLink(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		shown := p.formOf(r, lang, in)
		shown.Values = sent(r)
		if !shown.Aliases {
			// A workspace that cannot choose aliases has no field for one.
			shown.Values.Alias = ""
		}
		expiry, problems := shown.Values.checked(p.settings.Now())
		if len(problems) > 0 {
			p.form(w, r, lang, shown, problems...)
			return
		}
		created, err := p.settings.Links.Create(r.Context(), in.principal.WorkspaceID, link.Draft{
			Alias: shown.Values.Alias, Destination: shown.Values.Destination, Campaign: shown.Values.campaign(),
			Title: shown.Values.Title, Tags: shown.Values.tags(), ExpiresAt: expiry,
		})
		if found, known := refused(err); known {
			p.form(w, r, lang, shown, found)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		redirect(w, "/"+string(lang)+"/links/"+created.ID, http.StatusSeeOther)
	}
}

// editFormOf returns the form that changes a link of a workspace.
func (p *panel) editFormOf(r *http.Request, lang language, in member, found link.Link) formView {
	shown := p.formOf(r, lang, in)
	page := "/" + string(lang) + "/links/" + found.ID
	shown.New, shown.Aliases, shown.Action, shown.Cancel = false, false, page, page
	shown.Name = found.Title
	if shown.Name == "" {
		shown.Name = found.Identifier
	}
	return shown
}

// editLinkPage serves the form that changes a link, with what the link
// has.
func (p *panel) editLinkPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		found, err := p.settings.Links.Get(r.Context(), in.principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, link.ErrNotFound) {
			p.missing(w, r, lang)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		if found.State == link.StateBlocked {
			// Nothing of it can be changed: its page says so.
			redirect(w, "/"+string(lang)+"/links/"+found.ID, http.StatusSeeOther)
			return
		}
		shown := p.editFormOf(r, lang, in, found)
		shown.Values = valuesOf(found)
		p.form(w, r, lang, shown)
	}
}

// updateLink changes a link as a form describes it. The form holds every
// attribute, so each is set to what it says: an empty field takes away
// what the link had there.
func (p *panel) updateLink(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		ctx := r.Context()
		found, err := p.settings.Links.Get(ctx, in.principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, link.ErrNotFound) {
			p.missing(w, r, lang)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		shown := p.editFormOf(r, lang, in, found)
		shown.Values = sent(r)
		expiry, problems := shown.Values.checked(p.settings.Now())
		// The expiry a link already has is not one it is given now: a link
		// that has expired can have the rest of it changed.
		if found.ExpiresAt != nil && shown.Values.ExpiresAt == valuesOf(found).ExpiresAt {
			expiry = found.ExpiresAt
			problems = besides(problems, fieldExpiresAt)
		}
		if len(problems) > 0 {
			p.form(w, r, lang, shown, problems...)
			return
		}
		tags, campaign := shown.Values.tags(), shown.Values.campaign()
		change := link.Change{
			Destination: &shown.Values.Destination, Title: &shown.Values.Title, Tags: &tags,
			Campaign: link.CampaignChange{
				Source: &campaign.Source, Medium: &campaign.Medium, Name: &campaign.Name, Term: &campaign.Term, Content: &campaign.Content,
			},
			ExpiresAt: expiry, NoExpiry: expiry == nil,
		}
		_, err = p.settings.Links.Update(ctx, in.principal.WorkspaceID, found.ID, change)
		if errors.Is(err, link.ErrNotFound) {
			p.missing(w, r, lang)
			return
		}
		if wrong, known := refused(err); known {
			p.form(w, r, lang, shown, wrong)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		redirect(w, "/"+string(lang)+"/links/"+found.ID, http.StatusSeeOther)
	}
}

// besides returns problems without the ones of a field.
func besides(problems []problem, field string) []problem {
	return slices.DeleteFunc(problems, func(found problem) bool { return found.Field == field })
}

// switchLink disables or enables a link, and sends whoever asked back to
// its page, which shows its state. A link the operator blocked stays as it
// is, and its page says that too.
func (p *panel) switchLink(
	lang language, to func(*link.Store, context.Context, string, string) (link.Link, error),
) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		id := r.PathValue("id")
		_, err := to(p.settings.Links, r.Context(), in.principal.WorkspaceID, id)
		switch {
		case errors.Is(err, link.ErrNotFound):
			p.missing(w, r, lang)
		case err != nil && !errors.Is(err, link.ErrLinkBlocked):
			p.fail(w, r, lang, err)
		default:
			redirect(w, "/"+string(lang)+"/links/"+id, http.StatusSeeOther)
		}
	}
}

// deleteView is what the page that asks before deleting a link is
// rendered with.
type deleteView struct {
	view
	Name string
	// Action is where the form that deletes is sent, and Keep where
	// whoever thinks better of it goes back to.
	Action, Keep string
}

// deleteLinkPage asks before a link is deleted: it cannot be undone, and
// the panel has no script to ask with.
func (p *panel) deleteLinkPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		found, err := p.settings.Links.Get(r.Context(), in.principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, link.ErrNotFound) {
			p.missing(w, r, lang)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		page := "/" + string(lang) + "/links/" + found.ID
		shown := deleteView{view: p.viewIn(r, lang, in), Name: found.Title, Action: page + "/delete", Keep: page}
		if shown.Name == "" {
			shown.Name = found.Identifier
		}
		p.render(w, r, http.StatusOK, lang, pageLinkDelete, shown)
	}
}

// deleteLink deletes a link, and sends whoever asked to the list it is no
// longer in.
func (p *panel) deleteLink(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		err := p.settings.Links.Delete(r.Context(), in.principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, link.ErrNotFound) {
			p.missing(w, r, lang)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		redirect(w, "/"+string(lang)+"/links", http.StatusSeeOther)
	}
}

// changing puts behind a session something that changes what a workspace
// holds. A session makes as many changes in a minute as a key of the API
// (PLAN.md section 9): a demo workspace is open to whoever comes, and
// every change reaches the copy of the links that the redirect keeps. The
// form is read here, within the size a form of the panel has.
func (p *panel) changing(lang language, change func(http.ResponseWriter, *http.Request, member)) http.HandlerFunc {
	return p.inside(lang, func(w http.ResponseWriter, r *http.Request, in member) {
		if wait := p.writes.Take(in.principal.SessionID); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			p.render(w, r, http.StatusTooManyRequests, lang, pageBusy, p.viewIn(r, lang, in))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limits.APIMaxBodyBytes)
		if err := r.ParseForm(); err != nil {
			// Not a form that a page of the panel sends.
			p.missing(w, r, lang)
			return
		}
		change(w, r, in)
	})
}
