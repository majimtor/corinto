package panel

import (
	"net/http"
	"time"

	"github.com/majimtor/corinto/internal/ingest"
)

// statusPeriods are the periods the status page can be asked for, in days:
// the ones a reconciliation covers unasked and at the most.
var statusPeriods = []int{ingest.DefaultDays, ingest.MaxDays}

// statusView is what the status page is rendered with.
type statusView struct {
	view
	// Demo says that the events of the workspace are generated.
	Demo bool
	// The period: how many days, the first and the last, by the clock of
	// the database, which is the one that dates the terms. Periods are the
	// ones the page can be asked for.
	Days        int
	First, Last time.Time
	Periods     []choice
	// Terms is what became of the events of each day, the oldest first,
	// and Total what the days add up to.
	Terms []ingest.Day
	Total ingest.Day
	// Attempts is how many times an event fails before it is set aside.
	Attempts int
	// Through is how far the events of the whole service are processed,
	// and Retention when what has been kept for its time was last deleted,
	// or nil when it never was.
	Through   time.Time
	Retention *time.Time
}

// statusPage serves what became of the events of the workspace of a
// session, day by day: the reconciliation the plan calls the visible proof
// (PLAN.md section 17), with how far the service has processed and when it
// last deleted what was due. It reads what the ingestion status of the API
// reads, in its order (internal/api, ingestion.status): the two share no
// code, because one answers a program and the other a person.
func (p *panel) statusPage(lang language) func(http.ResponseWriter, *http.Request, member) {
	return func(w http.ResponseWriter, r *http.Request, in member) {
		days, ok := periodOf(r.URL.Query(), statusPeriods, ingest.DefaultDays)
		if !ok {
			p.missing(w, r, lang)
			return
		}
		ctx := r.Context()
		shown := statusView{view: p.viewIn(r, lang, in), Demo: in.workspace.Demo, Days: days, Attempts: ingest.MaxAttempts}
		here := "/" + string(lang) + "/status"
		shown.Periods = choicesOf(here, statusPeriods, ingest.DefaultDays, days)
		shown.OtherPath += periodQuery(days, ingest.DefaultDays)

		var err error
		if shown.Through, err = p.settings.Inbox.ProcessedThrough(ctx); err != nil {
			p.fail(w, r, lang, err)
			return
		}
		if shown.Terms, err = p.settings.Inbox.Reconciliation(ctx, in.principal.WorkspaceID, days); err != nil {
			p.fail(w, r, lang, err)
			return
		}
		if shown.Retention, err = p.settings.Aggregates.LastRetention(ctx); err != nil {
			p.fail(w, r, lang, err)
			return
		}
		for _, day := range shown.Terms {
			shown.Total.Served += day.Served
			shown.Total.Accepted += day.Accepted
			shown.Total.Lost += day.Lost
			shown.Total.Processed += day.Processed
			shown.Total.Pending += day.Pending
			shown.Total.DeadLettered += day.DeadLettered
		}
		// A reconciliation has a row for each of its days, the oldest first.
		shown.First, shown.Last = shown.Terms[0].Day, shown.Terms[len(shown.Terms)-1].Day
		p.render(w, r, http.StatusOK, lang, pageStatus, shown)
	}
}
