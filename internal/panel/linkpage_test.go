package panel

import (
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/link"
	"github.com/majimtor/corinto/internal/rollup"
)

// thirtyDays returns a period of thirty days and its series, with nothing
// in it.
func thirtyDays(t *testing.T) (rollup.Period, []rollup.Point) {
	t.Helper()
	return daysOf(t, 30)
}

// daysOf returns a period of so many days and its series, with nothing in
// it.
func daysOf(t *testing.T, days int) (rollup.Period, []rollup.Point) {
	t.Helper()
	from := time.Date(2026, time.October, 21, 0, 0, 0, 0, time.UTC)
	period, err := rollup.NewPeriod(from, from.AddDate(0, 0, days), rollup.ByDay)
	if err != nil {
		t.Fatal(err)
	}
	series := make([]rollup.Point, days)
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

// The days named under a series never run into each other: every day of a
// week, one a week of thirty days and one a fortnight of ninety, the last
// day always among them.
func TestTheDaysNamedUnderASeriesAreAsManyAsFit(t *testing.T) {
	for _, c := range []struct {
		days  int
		named []int
	}{
		{days: 1, named: []int{0}},
		{days: 7, named: []int{0, 1, 2, 3, 4, 5, 6}},
		{days: 8, named: []int{0, 7}},
		{days: 30, named: []int{1, 8, 15, 22, 29}},
		{days: 49, named: []int{6, 13, 20, 27, 34, 41, 48}},
		{days: 50, named: []int{7, 21, 35, 49}},
		{days: 90, named: []int{5, 19, 33, 47, 61, 75, 89}},
		{days: 397, named: []int{18, 81, 144, 207, 270, 333, 396}},
	} {
		period, series := daysOf(t, c.days)
		shown := linkView{Series: series}
		shown.draw(period)
		var named []int
		for _, label := range shown.DayLabels {
			named = append(named, int(label.Day.Sub(period.From())/(24*time.Hour)))
		}
		if !slices.Equal(named, c.named) {
			t.Errorf("a series of %d days names the days %v, want %v", c.days, named, c.named)
		}
		if len(named) > maxDayLabels {
			t.Errorf("a series of %d days names %d of them, and %d fit", c.days, len(named), maxDayLabels)
		}
	}
	// The periods of the page are among the ones this test draws.
	for _, days := range periods {
		if !slices.Contains([]int{7, 30, 90}, days) {
			t.Errorf("the page offers a period of %d days, which this test does not draw", days)
		}
	}
}

// The address of the page of a link names its period, as the page itself
// writes it, or names none.
func TestThePeriodOfAPageIsOneOfThree(t *testing.T) {
	for _, c := range []struct {
		query string
		days  int
		ok    bool
	}{
		{query: "", days: 30, ok: true},
		{query: "days=7", days: 7, ok: true},
		{query: "days=30", days: 30, ok: true},
		{query: "days=90", days: 90, ok: true},
		// Whatever else the address carries is not what names the period.
		{query: "after=7", days: 30, ok: true},
		{query: "days=90&after=7", days: 90, ok: true},
		{query: "days=14"}, {query: "days=0"}, {query: "days=-7"}, {query: "days=91"}, {query: "days=397"},
		{query: "days="}, {query: "days"}, {query: "days=seven"},
		// A number written another way is not what the page writes.
		{query: "days=07"}, {query: "days=7.0"}, {query: "days=%207"}, {query: "days=7+"}, {query: "days=0x7"}, {query: "days=9e1"},
		// Twice is not once, even twice the same.
		{query: "days=7&days=7"}, {query: "days=7&days=90"}, {query: "days=30&days="},
	} {
		query, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatal(err)
		}
		if days, ok := periodOf(query, periods, defaultPeriod); days != c.days || ok != c.ok {
			t.Errorf("the period of %q = %d, %t; want %d, %t", c.query, days, ok, c.days, c.ok)
		}
	}
	// The page of the default period has one address, and each of the
	// others names its days.
	if got := []string{periodQuery(7, defaultPeriod), periodQuery(30, defaultPeriod), periodQuery(90, defaultPeriod)}; !slices.Equal(got, []string{"?days=7", "", "?days=90"}) {
		t.Errorf("the addresses of the three periods carry %q", got)
	}
	// What the page writes for a period is what it reads as that period.
	for _, days := range periods {
		written := periodQuery(days, defaultPeriod)
		query, err := url.ParseQuery(written[min(1, len(written)):])
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := periodOf(query, periods, defaultPeriod); got != days || !ok {
			t.Errorf("the address the page writes for %d days is read as %d, %t", days, got, ok)
		}
	}
	if !slices.Contains(periods, defaultPeriod) || !slices.IsSorted(periods) || len(slices.Compact(slices.Clone(periods))) != len(periods) {
		t.Errorf("the periods are %v: the default one is not among them, or they are not each once and in order", periods)
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

// A breakdown by a closed enumeration: the value with the most first, as
// the breakdown gives them, and the one of what the rules do not tell apart
// last, whatever it adds up to. Nothing the breakdown counted is lost.
func TestTheRowsOfAnEnumerationEndWithWhatTheRulesDoNotTellApart(t *testing.T) {
	devices := partsOf("device.", classify.Devices(), classify.DeviceOther, []rollup.Share{
		{Value: "other", Clicks: 300}, {Value: "mobile", Clicks: 200}, {Value: "desktop", Clicks: 50},
	}, 0)
	want := []part{
		{NameKey: "device.mobile", Clicks: 200, PercentOfMost: 66.6},
		{NameKey: "device.desktop", Clicks: 50, PercentOfMost: 16.6},
		{NameKey: "device.other", Clicks: 300, PercentOfMost: 100},
	}
	if !slices.Equal(devices.Rows, want) || devices.Clicks != 550 || devices.Top != want[2] {
		t.Errorf("the devices are %+v, which add up to %d with %+v at the top; want %+v, 550 and the last", devices.Rows, devices.Clicks, devices.Top, want)
	}

	// A value this page has no name for, and the clicks of the values the
	// breakdown did not name, are counted in the last row; with no click
	// of its own, that row is still there for them.
	browsers := partsOf("browser.", classify.Browsers(), classify.BrowserOther, []rollup.Share{
		{Value: "safari", Clicks: 40}, {Value: "chrome", Clicks: 40}, {Value: "a_browser_of_tomorrow", Clicks: 7},
	}, 3)
	want = []part{
		{NameKey: "browser.safari", Clicks: 40, PercentOfMost: 100},
		{NameKey: "browser.chrome", Clicks: 40, PercentOfMost: 100},
		{NameKey: "browser.other", Clicks: 10, PercentOfMost: 25},
	}
	// Of two rows with as many clicks, the first is the one at the top.
	if !slices.Equal(browsers.Rows, want) || browsers.Clicks != 90 || browsers.Top != want[0] {
		t.Errorf("the browsers are %+v, which add up to %d with %+v at the top; want %+v, 90 and the first", browsers.Rows, browsers.Clicks, browsers.Top, want)
	}

	// The row of the rest is there only when something is counted in it.
	only := partsOf("device.", classify.Devices(), classify.DeviceOther, []rollup.Share{{Value: "tablet", Clicks: 1}}, 0)
	if len(only.Rows) != 1 || only.Rows[0] != (part{NameKey: "device.tablet", Clicks: 1, PercentOfMost: 100}) || only.Clicks != 1 {
		t.Errorf("a breakdown of one value is %+v", only)
	}
	if empty := partsOf("device.", classify.Devices(), classify.DeviceOther, nil, 0); len(empty.Rows) != 0 || empty.Clicks != 0 || empty.Top != (part{}) {
		t.Errorf("a breakdown of nothing is %+v", empty)
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
