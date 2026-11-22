package panel

import (
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/rollup"
)

// thirtyDays returns a period of thirty days and its series, with nothing
// in it.
func thirtyDays(t *testing.T) (rollup.Period, []rollup.Point) {
	t.Helper()
	from := time.Date(2026, time.October, 21, 0, 0, 0, 0, time.UTC)
	period, err := rollup.NewPeriod(from, from.AddDate(0, 0, 30), rollup.ByDay)
	if err != nil {
		t.Fatal(err)
	}
	series := make([]rollup.Point, 30)
	for i := range series {
		series[i].Start = from.AddDate(0, 0, i)
	}
	return period, series
}

func TestTheSeriesNamesItsLastDayAndOneAWeekBack(t *testing.T) {
	period, series := thirtyDays(t)
	series[12].Requests, series[20].Requests = 40, 40
	series[12].EstimatedHumanClicks = 10
	shown := linkView{Series: series}
	shown.draw(period)

	// The first line is of the requests and the second of the estimated
	// clicks: on the thirteenth day, forty on the top edge and ten a
	// quarter of the way up.
	if len(shown.Chart.Lines) != 2 {
		t.Fatalf("the series has %d lines, want two", len(shown.Chart.Lines))
	}
	requests, clicks := shown.Chart.Lines[0].Markers[12], shown.Chart.Lines[1].Markers[12]
	if requests.Y != shown.Chart.Top || clicks.Y != shown.Chart.Bottom-(shown.Chart.Bottom-shown.Chart.Top)/4 {
		t.Errorf("the thirteenth day is drawn at %v for its requests and %v for its clicks", requests.Y, clicks.Y)
	}

	var days []int
	for _, label := range shown.DayLabels {
		days = append(days, int(label.Day.Sub(period.From())/(24*time.Hour)))
	}
	if len(days) != 5 || days[0] != 1 || days[4] != 29 || days[4]-days[3] != 7 {
		t.Errorf("the days named under the series are %v, want the last and one a week back from it", days)
	}
	if last := shown.DayLabels[4]; last.X != shown.Chart.Right {
		t.Errorf("the last day is named at %v, want under the end of the series, %v", last.X, shown.Chart.Right)
	}
	// Of two days with as many requests, the first is the one with the
	// most.
	if !shown.Peak.Start.Equal(series[12].Start) {
		t.Errorf("the day with the most requests is %s, want %s", shown.Peak.Start, series[12].Start)
	}
	if len(shown.RuleMarks) != 0 {
		t.Errorf("a series with no version of the rules has the marks %+v", shown.RuleMarks)
	}
}

// A change of rules is marked on the series (PLAN.md section 5): where a
// version began inside the period, and not where the first one was
// already in use.
func TestAChangeOfRulesIsMarkedOnTheDayItBegan(t *testing.T) {
	period, series := thirtyDays(t)
	shown := linkView{Series: series, Versions: []rollup.RuleVersion{
		{Version: 1, FirstHour: period.From().AddDate(0, 0, -40)},
		// An hour before the period: it was in use when the period began.
		{Version: 2, FirstHour: period.From().Add(-time.Hour)},
		{Version: 3, FirstHour: period.From().AddDate(0, 0, 10).Add(13 * time.Hour)},
		{Version: 4, FirstHour: period.From().AddDate(0, 0, 29).Add(23 * time.Hour)},
		// After the period: there is no day of the series to mark.
		{Version: 5, FirstHour: period.To().Add(time.Hour)},
	}}
	shown.draw(period)
	if len(shown.RuleMarks) != 2 {
		t.Fatalf("the series marks %d changes of rules, want two: %+v", len(shown.RuleMarks), shown.RuleMarks)
	}
	if mark := shown.RuleMarks[0]; mark.Version != 3 || mark.X != shown.Chart.X(10) || mark.LabelX <= mark.X {
		t.Errorf("the first mark is %+v, want the third version on the eleventh day, %v", mark, shown.Chart.X(10))
	}
	if mark := shown.RuleMarks[1]; mark.Version != 4 || mark.X != shown.Chart.Right {
		t.Errorf("the second mark is %+v, want the fourth version on the last day", mark)
	}

	// The only version of a period is no change, wherever it began.
	only := linkView{Series: series, Versions: []rollup.RuleVersion{{Version: 6, FirstHour: period.From().AddDate(0, 0, 3)}}}
	only.draw(period)
	if len(only.RuleMarks) != 0 {
		t.Errorf("a period with one version marks %+v", only.RuleMarks)
	}
}

func TestTheReferrersAreRowsWithTheRestCountedTogetherAtTheEnd(t *testing.T) {
	var shown linkView
	shown.share([]rollup.Share{
		{Value: "news.example.org", Clicks: 200}, {Value: rollup.NoReferrer, Clicks: 100}, {Value: "blog.example.org", Clicks: 1},
	}, 50)
	if len(shown.Referrers) != 4 || shown.ReferrerClicks != 351 {
		t.Fatalf("the breakdown has %d rows that add up to %d, want four and 351", len(shown.Referrers), shown.ReferrerClicks)
	}
	want := []referrer{
		{Site: "news.example.org", Clicks: 200, PercentOfMost: 100},
		{Site: rollup.NoReferrer, None: true, Clicks: 100, PercentOfMost: 50},
		{Site: "blog.example.org", Clicks: 1, PercentOfMost: 0.5},
		{Others: true, Clicks: 50, PercentOfMost: 25},
	}
	for i, row := range shown.Referrers {
		if row != want[i] {
			t.Errorf("row %d of the breakdown is %+v, want %+v", i, row, want[i])
		}
	}

	var empty linkView
	empty.share(nil, 0)
	if len(empty.Referrers) != 0 || empty.ReferrerClicks != 0 {
		t.Errorf("a breakdown of nothing has the rows %+v", empty.Referrers)
	}
}

func TestAnActiveLinkWhoseTimeIsUpIsSaidToHaveExpired(t *testing.T) {
	now := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Second), now.Add(time.Second)
	for _, c := range []struct {
		found link.Link
		want  string
	}{
		{found: link.Link{State: link.StateActive}, want: "link.state.active"},
		{found: link.Link{State: link.StateActive, ExpiresAt: &future}, want: "link.state.active"},
		{found: link.Link{State: link.StateActive, ExpiresAt: &past}, want: "link.state.expired"},
		{found: link.Link{State: link.StateActive, ExpiresAt: &now}, want: "link.state.expired"},
		{found: link.Link{State: link.StateDisabled, ExpiresAt: &past}, want: "link.state.disabled"},
		{found: link.Link{State: link.StateBlocked}, want: "link.state.blocked"},
	} {
		if got := stateKey(c.found, now); got != c.want {
			t.Errorf("the state of %+v is said by %q, want %q", c.found, got, c.want)
		}
	}
}
