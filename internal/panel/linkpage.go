package panel

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/panel/chart"
	"github.com/majimtor/corinto/internal/rollup"
)

const (
	// defaultPeriod is how many days of UTC the page of a link covers when
	// it is asked for no other period, today included.
	defaultPeriod = 30
	// periodField is what names the period in the address of the page.
	periodField = "days"
	// maxDayLabels is how many days are named under a series at the most:
	// more of them run into each other on the narrowest drawing.
	maxDayLabels = 7
	// namedReferrers is how many referring sites the page names. The rest
	// are counted together.
	namedReferrers = 10
)

// periods are the periods the page of a link can be asked for, in days.
// The seed writes a history as long as the longest.
var periods = []int{7, defaultPeriod, 90}

// linkView is what the page of a link is rendered with.
type linkView struct {
	view
	Link link.Link
	// Name is what the page calls the link: its title, or its identifier
	// when it has none.
	Name string
	// StateKey is the key of the text that says the state of the link.
	StateKey string
	// Demo says that the traffic of the workspace is generated.
	Demo bool
	// Edit and Delete are where the link is changed and where it is
	// deleted. Switch is where its state is changed, to disabled or back,
	// and Disabled says which of the two it does. A link the operator
	// blocked can only be deleted: Blocked says so.
	Edit, Delete, Switch string
	Disabled, Blocked    bool

	// The period: how many days, the first and the last. Periods are the
	// ones the page can be asked for.
	Days        int
	First, Last time.Time
	Periods     []choice
	// Through is how far the events are processed.
	Through  time.Time
	Versions []rollup.RuleVersion

	Total rollup.Figures
	// Series is the figures of each day, and Peak the day with the most
	// requests.
	Series []rollup.Point
	Peak   rollup.Point
	Chart  chart.Lines
	// DayLabels are the days named under the series, and RuleMarks where
	// a version of the rules began inside the period.
	DayLabels []dayLabel
	RuleMarks []ruleMark

	// Referrers are the referring sites, the one with the most estimated
	// human clicks first, and ReferrerClicks what they add up to.
	Referrers      []referrer
	ReferrerClicks int64
	// Devices and Browsers are the same clicks by what the rules say of
	// the client that made each.
	Devices, Browsers parts
}

// choice is one of the periods a page can be asked for: how many days it
// has, where the page of that period is, and whether it is the one shown.
type choice struct {
	Days    int64
	Path    string
	Current bool
}

type dayLabel struct {
	X   float64
	Day time.Time
}

// ruleMark is where a version of the rules began: the line at X, and its
// label beside it.
type ruleMark struct {
	X, LabelX float64
	Version   int
}

// referrer is a row of the breakdown by referring site. Site is a host, as
// the request wrote it, or empty for the two rows that are not one: the
// clicks that named no referrer, and the sites the page does not name.
type referrer struct {
	Site          string
	None, Others  bool
	Clicks        int64
	PercentOfMost float64
}

// part is a row of a breakdown by a closed enumeration of the rules: the
// kind of device or the family of browser. NameKey is the key of the text
// that names its value.
type part struct {
	NameKey       string
	Clicks        int64
	PercentOfMost float64
}

// parts is a breakdown by a closed enumeration: its rows, what they add up
// to, and the first of the rows with the most clicks.
type parts struct {
	Rows   []part
	Clicks int64
	Top    part
}

// periodOf returns how many days the address of a page asks for, of the
// periods the page offers: the one it shows unasked when the address names
// no period, and false when it names anything that is not one of them,
// written as the page writes it.
func periodOf(query url.Values, offered []int, unasked int) (int, bool) {
	asked, named := query[periodField]
	if !named {
		return unasked, true
	}
	for _, days := range offered {
		if len(asked) == 1 && asked[0] == strconv.Itoa(days) {
			return days, true
		}
	}
	return 0, false
}

// periodQuery returns what the address of a page carries for a period:
// nothing for the one the page shows unasked, so that the page has one
// address when it is asked for no period.
func periodQuery(days, unasked int) string {
	if days == unasked {
		return ""
	}
	return "?" + url.Values{periodField: {strconv.Itoa(days)}}.Encode()
}

// choicesOf returns the periods a page offers, each at its address, with
// the one that is shown marked.
func choicesOf(page string, offered []int, unasked, shown int) []choice {
	choices := make([]choice, len(offered))
	for i, days := range offered {
		choices[i] = choice{Days: int64(days), Path: page + periodQuery(days, unasked), Current: days == shown}
	}
	return choices
}

// linkPage serves the page of a link in one language, for whoever is in a
// workspace: what the link is, and its figures of the last days with a
// series and three breakdowns. The address says how many days.
func (p *panel) linkPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		ctx := r.Context()
		principal := in.principal
		// A period the page does not have is a page that does not exist,
		// like a list of links that starts after something that is not one.
		days, ok := periodOf(r.URL.Query(), periods, defaultPeriod)
		if !ok {
			p.missing(w, r, lang)
			return
		}
		// The link is loaded by workspace and identifier, as the API loads
		// it: one of another workspace is not found.
		id, demo, err := p.settings.Links.InWorkspace(ctx, principal.WorkspaceID, r.PathValue("id"))
		if errors.Is(err, link.ErrNotFound) {
			p.missing(w, r, lang)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		found, err := p.settings.Links.Get(ctx, principal.WorkspaceID, id)
		if errors.Is(err, link.ErrNotFound) {
			p.missing(w, r, lang)
			return
		}
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}

		now := p.settings.Now().UTC()
		tomorrow := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
		period, err := rollup.NewPeriod(tomorrow.AddDate(0, 0, -days), tomorrow, rollup.ByDay)
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		shown := linkView{
			view: p.viewIn(r, lang, in), Link: found, Name: found.Title, StateKey: stateKey(found, now), Demo: demo,
			Days: days, First: period.From(), Last: period.To().AddDate(0, 0, -1),
		}
		if shown.Name == "" {
			shown.Name = found.Identifier
		}
		page := "/" + string(lang) + "/links/" + found.ID
		shown.Periods = choicesOf(page, periods, defaultPeriod, days)
		// The page in the other language is of the same period.
		shown.OtherPath += periodQuery(days, defaultPeriod)
		shown.Edit, shown.Delete, shown.Switch = page+"/edit", page+"/delete", page+"/disable"
		shown.Disabled, shown.Blocked = found.State == link.StateDisabled, found.State == link.StateBlocked
		if shown.Disabled {
			shown.Switch = page + "/enable"
		}

		// How far the events are processed is read before any figure, as
		// the API does (internal/api, metrics.about): the figures read
		// later hold at least what that moment says
		// (docs/delivery-semantics.md, statement 7). The two sites read
		// in the same order and share no code, because what each does
		// with a failure differs.
		if shown.Through, err = p.settings.Inbox.ProcessedThrough(ctx); err != nil {
			p.fail(w, r, lang, err)
			return
		}
		if shown.Versions, err = p.settings.Aggregates.RuleVersions(ctx, period); err != nil {
			p.fail(w, r, lang, err)
			return
		}
		if shown.Total, err = p.settings.Aggregates.Summary(ctx, id, period); err != nil {
			p.fail(w, r, lang, err)
			return
		}
		if shown.Series, err = p.settings.Aggregates.Series(ctx, id, period); err != nil {
			p.fail(w, r, lang, err)
			return
		}
		shares, others, err := p.settings.Aggregates.Breakdown(ctx, id, period, rollup.DimensionReferrerHost, namedReferrers)
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		// The two enumerations are asked for whole, and for more values
		// than they have: what is past them is counted, not lost.
		devices, otherDevices, err := p.settings.Aggregates.Breakdown(ctx, id, period, rollup.DimensionDevice, rollup.DefaultShares)
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		browsers, otherBrowsers, err := p.settings.Aggregates.Breakdown(ctx, id, period, rollup.DimensionBrowser, rollup.DefaultShares)
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}

		shown.draw(period)
		shown.share(shares, others)
		shown.Devices = partsOf("device.", classify.Devices(), classify.DeviceOther, devices, otherDevices)
		shown.Browsers = partsOf("browser.", classify.Browsers(), classify.BrowserOther, browsers, otherBrowsers)
		p.render(w, r, http.StatusOK, lang, pageLink, shown)
	}
}

// stateKey returns the key of the text that says the state of a link. An
// active link whose time is up is said to have expired: it is what the
// redirect answers by.
func stateKey(found link.Link, now time.Time) string {
	if found.State == link.StateActive && found.ExpiresAt != nil && !found.ExpiresAt.After(now) {
		return "link.state.expired"
	}
	return "link.state." + string(found.State)
}

// draw places the series of the page: its two lines, the days named under
// them and the marks of the rules.
func (v *linkView) draw(period rollup.Period) {
	requests, clicks := make([]int64, len(v.Series)), make([]int64, len(v.Series))
	for i, point := range v.Series {
		requests[i], clicks[i] = point.Requests, point.EstimatedHumanClicks
		if point.Requests > v.Peak.Requests {
			v.Peak = point
		}
	}
	v.Chart = chart.NewLines(requests, clicks)
	// The last day is always named, and then the ones a step back from it.
	every := labelEvery(len(v.Series))
	for i := (len(v.Series) - 1) % every; i < len(v.Series); i += every {
		v.DayLabels = append(v.DayLabels, dayLabel{X: v.Chart.X(i), Day: v.Series[i].Start})
	}
	// The first version of a period was already in use when it began, or
	// began with its first event: only a later one is a change to mark
	// (PLAN.md section 5), and only when it began inside the period. That
	// is asked of the moment itself: the division below rounds towards
	// zero, and would place on the first day a version that began an hour
	// before it.
	for i, version := range v.Versions {
		if i == 0 || version.FirstHour.Before(period.From()) {
			continue
		}
		if day := int(version.FirstHour.Sub(period.From()) / (24 * time.Hour)); day < len(v.Series) {
			v.RuleMarks = append(v.RuleMarks, ruleMark{X: v.Chart.X(day), LabelX: v.Chart.X(day) + 4, Version: version.Version})
		}
	}
}

// share turns the breakdown by referrer host into the rows of the page.
// The length of the bar of a row is its part of the row with the most
// clicks, so the longest bar is always whole.
func (v *linkView) share(shares []rollup.Share, others int64) {
	for _, share := range shares {
		v.Referrers = append(v.Referrers, referrer{
			Site: share.Value, None: share.Value == rollup.NoReferrer, Clicks: share.Clicks,
		})
		v.ReferrerClicks += share.Clicks
	}
	if others > 0 {
		v.Referrers = append(v.Referrers, referrer{Others: true, Clicks: others})
		v.ReferrerClicks += others
	}
	var most int64
	for _, row := range v.Referrers {
		most = max(most, row.Clicks)
	}
	for i := range v.Referrers {
		v.Referrers[i].PercentOfMost = percentOfMost(v.Referrers[i].Clicks, most)
	}
}

// percentOfMost returns how long the bar of a row is: its part of the row
// with the most clicks, to a tenth of a percent.
func percentOfMost(clicks, most int64) float64 {
	return float64(clicks*1000/max(most, 1)) / 10
}

// labelEvery returns how many days apart the days named under a series
// are. Every day of a short series is named. A longer one names a day every
// so many whole weeks, as few as leave no more names than fit: each is then
// the same day of the week.
func labelEvery(days int) int {
	if days <= maxDayLabels {
		return 1
	}
	every := 7
	for (days+every-1)/every > maxDayLabels {
		every += 7
	}
	return every
}

// partsOf turns a breakdown by a closed enumeration into the rows of the
// page: the value with the most clicks first, as the breakdown gives them,
// and last the one the rules give to what they do not tell apart. A value
// is named by the text whose key is the prefix and the value. That last
// row also counts what the page has no name for: a value the enumeration
// gained after the page was written, and the values past the ones the
// breakdown names.
func partsOf[V ~string](prefix string, known []V, rest V, shares []rollup.Share, others int64) parts {
	var found parts
	unnamed := others
	for _, share := range shares {
		if value := V(share.Value); value == rest || !slices.Contains(known, value) {
			unnamed += share.Clicks
			continue
		}
		found.Rows = append(found.Rows, part{NameKey: prefix + share.Value, Clicks: share.Clicks})
	}
	if unnamed > 0 {
		found.Rows = append(found.Rows, part{NameKey: prefix + string(rest), Clicks: unnamed})
	}
	for _, row := range found.Rows {
		found.Clicks += row.Clicks
		// The first of the rows with the most: a later one with as many
		// does not take its place.
		if row.Clicks > found.Top.Clicks {
			found.Top = row
		}
	}
	for i := range found.Rows {
		found.Rows[i].PercentOfMost = percentOfMost(found.Rows[i].Clicks, found.Top.Clicks)
	}
	// The top is one of the rows, as the page has it: with its bar.
	found.Top.PercentOfMost = percentOfMost(found.Top.Clicks, found.Top.Clicks)
	return found
}
