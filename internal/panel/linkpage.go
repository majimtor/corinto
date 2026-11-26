package panel

import (
	"errors"
	"net/http"
	"time"

	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/panel/chart"
	"github.com/majimtor/corinto/internal/rollup"
)

const (
	// periodDays is how many days of UTC the page of a link covers, today
	// included.
	periodDays = 30
	// labelEvery is how many days apart the days named under the series
	// are: a week, so each label is the same weekday.
	labelEvery = 7
	// namedReferrers is how many referring sites the page names. The rest
	// are counted together.
	namedReferrers = 10
)

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

	// The period: how many days, the first and the last.
	Days        int
	First, Last time.Time
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

// linkPage serves the page of a link in one language, for whoever is in a
// workspace: what the link is, and its figures of the last days with a
// series and a breakdown.
func (p *panel) linkPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		ctx := r.Context()
		principal := in.principal
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
		period, err := rollup.NewPeriod(tomorrow.AddDate(0, 0, -periodDays), tomorrow, rollup.ByDay)
		if err != nil {
			p.fail(w, r, lang, err)
			return
		}
		shown := linkView{
			view: p.viewIn(r, lang, in), Link: found, Name: found.Title, StateKey: stateKey(found, now), Demo: demo,
			Days: periodDays, First: period.From(), Last: period.To().AddDate(0, 0, -1),
		}
		if shown.Name == "" {
			shown.Name = found.Identifier
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

		shown.draw(period)
		shown.share(shares, others)
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
	// The last day is always named, and then one a week back from it.
	for i := (len(v.Series) - 1) % labelEvery; i < len(v.Series); i += labelEvery {
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
		v.Referrers[i].PercentOfMost = float64(v.Referrers[i].Clicks*1000/max(most, 1)) / 10
	}
}
