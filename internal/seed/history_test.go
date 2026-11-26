package seed

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
)

// The day the histories of these tests are made up for, a Friday.
var historyDay = time.Date(2026, time.November, 20, 0, 0, 0, 0, time.UTC)

// sownFields returns the demo workspaces as the history is written for
// them, without a database: each link is known by its alias.
func sownFields() []field {
	fields := make([]field, len(demos))
	for i, demo := range demos {
		fields[i] = field{demo: demo, workspaceID: demo.name}
		for _, entry := range demo.links {
			fields[i].links = append(fields[i].links, sown{planted: entry, id: entry.alias})
		}
	}
	return fields
}

// told makes up the whole history of a day.
func told(t *testing.T, day time.Time) [][]ingest.Event {
	t.Helper()
	classifier, err := classify.New()
	if err != nil {
		t.Fatal(err)
	}
	return history(classifier, day, historyDays, sownFields())
}

// daysAgo returns how many days before a day an event occurred.
func daysAgo(day time.Time, event ingest.Event) int {
	return int(day.Sub(event.OccurredAt.Truncate(24*time.Hour)) / (24 * time.Hour))
}

// perDay counts the events of a link by how many days before the day they
// occurred.
func perDay(events []ingest.Event, alias string) [historyDays + 1]int {
	var counted [historyDays + 1]int
	for _, event := range events {
		if event.LinkID == alias {
			counted[daysAgo(historyDay, event)]++
		}
	}
	return counted
}

// PLAN.md section 8: a fixed generator seed for the data and dates
// relative to the day it runs.
func TestTheHistoryOfADayIsAlwaysTheSame(t *testing.T) {
	first, second := told(t, historyDay), told(t, historyDay)
	for i := range first {
		if !slices.EqualFunc(first[i], second[i], func(a, b ingest.Event) bool { return a == b }) {
			t.Fatalf("two histories of one day differ for %s", demos[i].name)
		}
	}
	// A week later it is the same history, a week later: same days of the
	// week, same requests.
	later := told(t, historyDay.AddDate(0, 0, 7))
	for i := range first {
		moved := slices.Clone(first[i])
		for n := range moved {
			moved[n].OccurredAt = moved[n].OccurredAt.AddDate(0, 0, 7)
		}
		if !slices.EqualFunc(moved, later[i], func(a, b ingest.Event) bool { return a == b }) {
			t.Fatalf("the history of a week later is not the same history, a week later, for %s", demos[i].name)
		}
	}
	// A day later the days of the week fall elsewhere, and it is another.
	if next := told(t, historyDay.AddDate(0, 0, 1)); len(next[0]) == len(first[0]) && len(next[1]) == len(first[1]) {
		t.Error("the history of the day after has as many events for both workspaces: the week does not show in it")
	}
}

// Ninety days of history (PLAN.md section 8), which end the day before the
// seed: nothing in it depends on the hour the seed runs at.
func TestTheHistoryCoversTheNinetyDaysBeforeTheSeed(t *testing.T) {
	total := 0
	for i, events := range told(t, historyDay) {
		total += len(events)
		var days [historyDays + 1]int
		for _, event := range events {
			ago := daysAgo(historyDay, event)
			if ago < 1 || ago > historyDays || !event.OccurredAt.Before(historyDay) || event.OccurredAt.Location() != time.UTC {
				t.Fatalf("%s has an event at %v, outside the %d days before %v", demos[i].name, event.OccurredAt, historyDays, historyDay)
			}
			if event.WorkspaceID != demos[i].name || event.ID != "" {
				t.Fatalf("an event of %s is of the workspace %q, or already has the identifier %q", demos[i].name, event.WorkspaceID, event.ID)
			}
			days[ago]++
		}
		for ago := 1; ago <= historyDays; ago++ {
			if days[ago] == 0 {
				t.Errorf("%s has no event %d days before the seed", demos[i].name, ago)
			}
		}
	}
	// About sixty thousand: enough for every chart to have something in
	// it, and few enough for the seed to take seconds.
	if total < 55_000 || total > 62_000 {
		t.Errorf("the history has %d events, want about 60,000", total)
	}
	// And none at all when no day is asked for.
	classifier, err := classify.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, events := range history(classifier, historyDay, 0, sownFields()) {
		if len(events) != 0 {
			t.Errorf("a history of no days has %d events", len(events))
		}
	}
}

// A launch peak (PLAN.md section 8): nothing asks for a link before it is
// shared, and its first day is its busiest.
func TestALinkHasNoHistoryBeforeItIsSharedAndStartsWithAPeak(t *testing.T) {
	history := told(t, historyDay)
	shared, steady := 0, 0
	for i, demo := range demos {
		for _, entry := range demo.links {
			days := perDay(history[i], entry.alias)
			if entry.daily <= 0 {
				t.Errorf("%s gets no request on an ordinary day", entry.alias)
			}
			if entry.sharedDaysAgo == 0 {
				steady++
				if days[historyDays] == 0 {
					t.Errorf("%s was there all along, and has no request on the first day of the history", entry.alias)
				}
				continue
			}
			shared++
			for ago := entry.sharedDaysAgo + 1; ago <= historyDays; ago++ {
				if days[ago] != 0 {
					t.Fatalf("%s has %d requests %d days before the seed, before it was shared", entry.alias, days[ago], ago)
				}
			}
			// Its first week against its third, which is an ordinary one:
			// the same days of the week, and about twice as busy.
			if entry.sharedDaysAgo > 21 {
				first, third := 0, 0
				for since := range 7 {
					first += days[entry.sharedDaysAgo-since]
					third += days[entry.sharedDaysAgo-14-since]
				}
				if first*10 < third*18 || first*10 > third*28 {
					t.Errorf("%s had %d requests in its first week and %d in its third; want about twice as many", entry.alias, first, third)
				}
			}
			// And its busiest day is one of its first three: the very
			// first, unless that one fell on a quiet day of the week.
			busiest := slices.Max(days[:])
			if slices.Max(days[entry.sharedDaysAgo-2:entry.sharedDaysAgo+1]) != busiest {
				t.Errorf("the busiest day of %s, with %d requests, is not one of its first three", entry.alias, busiest)
			}
		}
	}
	if shared < 10 || steady < 3 {
		t.Errorf("%d links were shared at some day and %d were there all along; want both kinds", shared, steady)
	}
	// Each workspace has a launch that the last thirty days show.
	for _, demo := range demos {
		if !slices.ContainsFunc(demo.links, func(entry planted) bool { return entry.sharedDaysAgo > 7 && entry.sharedDaysAgo < 30 }) {
			t.Errorf("no link of %s was first shared within the last thirty days", demo.name)
		}
	}
}

// Weekly seasonality (PLAN.md section 8), and not the same week for both.
func TestTheWeekOfEachWorkspaceShowsInItsHistory(t *testing.T) {
	history := told(t, historyDay)
	// By day of the week, the requests to a link that was there all along.
	week := func(workspace int, alias string) (byDay [7]int) {
		for ago, requests := range perDay(history[workspace], alias) {
			if ago >= 1 {
				byDay[historyDay.AddDate(0, 0, -ago).Weekday()] += requests
			}
		}
		return byDay
	}
	books, cycles := week(0, "opening-hours"), week(1, "workshop-booking")
	if books[time.Sunday]*3 > books[time.Tuesday]*2 || books[time.Saturday] > books[time.Wednesday] {
		t.Errorf("the bookshop has %v requests by day of the week, from Sunday; want a quiet weekend", books)
	}
	if cycles[time.Saturday]*10 < cycles[time.Tuesday]*12 || cycles[time.Sunday] < cycles[time.Monday] {
		t.Errorf("the bicycle shop has %v requests by day of the week, from Sunday; want a busy weekend", cycles)
	}
	for _, demo := range demos {
		for day, share := range demo.week {
			if share < 30 || share > 200 {
				t.Errorf("%s gets %d hundredths of an ordinary day on day %d of the week", demo.name, share, day)
			}
		}
	}
}

// Requests to an expired link (PLAN.md section 8): it redirects until its
// time is up, and whoever kept it still asks for it afterwards.
func TestTheLinkThatExpiredRedirectsUntilItsTimeIsUp(t *testing.T) {
	expired, late := 0, 0
	for i, events := range told(t, historyDay) {
		over := make(map[string]time.Time)
		for _, entry := range demos[i].links {
			if entry.expiredDaysAgo > 0 {
				expired++
				over[entry.alias] = historyDay.AddDate(0, 0, -entry.expiredDaysAgo)
			}
		}
		for _, event := range events {
			end, expires := over[event.LinkID]
			want := ingest.OutcomeRedirected
			if expires && !event.OccurredAt.Before(end) {
				want = ingest.OutcomeInactive
				late++
			}
			if event.Outcome != want {
				t.Fatalf("an access to %s at %v is %q, want %q", event.LinkID, event.OccurredAt, event.Outcome, want)
			}
		}
	}
	// A few a day, not a crowd: the link is over.
	if expired != 1 || late < 40 || late > 200 {
		t.Errorf("%d links have expired, with %d requests after their time; want one, asked for a few times a day", expired, late)
	}
}

// Traffic of all five classes (PLAN.md section 8). The history holds
// requests, and the classifier of the redirect says what each is: these are
// the clients the seed makes up, each in the class it is there for.
func TestTheClientsOfTheHistoryAreWhatTheSeedTakesThemFor(t *testing.T) {
	classifier, err := classify.New()
	if err != nil {
		t.Fatal(err)
	}
	classified := func(method, userAgent string, more ...string) classify.Classification {
		header := http.Header{"User-Agent": {userAgent}}
		for i := 1; i < len(more); i += 2 {
			header.Set(more[i-1], more[i])
		}
		return classifier.Classify(method, header)
	}
	for _, agent := range desktops {
		if got := classified(http.MethodGet, agent); got.Class != classify.ClassPresumedHuman || got.Device != classify.DeviceDesktop || got.Browser == classify.BrowserOther {
			t.Errorf("a desktop browser of the seed is %+v", got)
		}
		if got := classified(http.MethodGet, agent, "Sec-Purpose", "prefetch"); got.Class != classify.ClassPrefetch {
			t.Errorf("a browser that fetches ahead is %q", got.Class)
		}
	}
	for _, agent := range handhelds {
		if got := classified(http.MethodGet, agent); got.Class != classify.ClassPresumedHuman || got.Browser == classify.BrowserOther ||
			(got.Device != classify.DeviceMobile && got.Device != classify.DeviceTablet) {
			t.Errorf("a browser of a phone or a tablet of the seed is %+v", got)
		}
	}
	for class, agents := range map[classify.Class][]string{
		classify.ClassCrawler: crawlers, classify.ClassPreview: previewers, classify.ClassAutomation: libraries,
	} {
		for _, agent := range agents {
			if got := classified(http.MethodGet, agent); got.Class != class {
				t.Errorf("the client that sends %q is %q, and the seed takes it for %q", agent, got.Class, class)
			}
		}
	}
	if got := classified(http.MethodHead, libraries[0]); got.Class != classify.ClassAutomation {
		t.Errorf("a request for the headers alone is %q", got.Class)
	}

	// And the history has them all: mostly people, on every kind of device
	// and on several browsers, who come from sites that nobody has.
	var (
		classes  = make(map[classify.Class]int)
		devices  = make(map[classify.Device]int)
		browsers = make(map[classify.Browser]int)
		sites    = make(map[string]int)
		total    int
	)
	for _, events := range told(t, historyDay) {
		for _, event := range events {
			total++
			classes[event.Classification.Class]++
			if event.Classification.Class == classify.ClassPresumedHuman {
				devices[event.Classification.Device]++
				browsers[event.Classification.Browser]++
				sites[event.ReferrerHost]++
			} else if event.Classification.Class != classify.ClassPrefetch && event.ReferrerHost != "" {
				t.Fatalf("a request of the class %q names the site %q", event.Classification.Class, event.ReferrerHost)
			}
			if event.Method != http.MethodGet && event.Classification.Class != classify.ClassAutomation {
				t.Fatalf("a request of the class %q asks with %s", event.Classification.Class, event.Method)
			}
			if event.Classification.Version != classifier.Version() || event.Classification.Rule == "" {
				t.Fatalf("an event was classified by the version %d of the rules, under the rule %q", event.Classification.Version, event.Classification.Rule)
			}
		}
	}
	for _, class := range classify.Classes() {
		if classes[class] < total/100 {
			t.Errorf("%d of %d requests are of the class %q; want every class to show", classes[class], total, class)
		}
	}
	if people := classes[classify.ClassPresumedHuman]; people < total*7/10 || people > total*9/10 {
		t.Errorf("%d of %d requests are taken for people, want most and not all", people, total)
	}
	for _, device := range []classify.Device{classify.DeviceDesktop, classify.DeviceMobile, classify.DeviceTablet} {
		if devices[device] == 0 {
			t.Errorf("nobody in the history comes with a %s", device)
		}
	}
	if len(browsers) < 5 || browsers[classify.BrowserOther] != 0 {
		t.Errorf("the people of the history use %v", browsers)
	}
	if sites[""] == 0 || len(sites) < 8 {
		t.Errorf("the people of the history come from %d sites, and %d name none; want several and some that name none", len(sites), sites[""])
	}
	for site := range sites {
		documented := site == "" || strings.HasSuffix(site, ".example") ||
			slices.ContainsFunc([]string{"example.com", "example.net", "example.org"}, func(domain string) bool {
				return site == domain || strings.HasSuffix(site, "."+domain)
			})
		if !documented {
			t.Errorf("the history names the site %q, which is not of a domain kept for documentation", site)
		}
	}
}

// Every link is shared with an audience the seed knows: one it did not
// know would have nobody in it.
func TestEveryLinkIsSharedWithAnAudience(t *testing.T) {
	used := make(map[string]bool)
	for _, demo := range demos {
		for _, entry := range demo.links {
			crowd, known := audiences[entry.campaign.Source]
			if !known || len(crowd.referrers) == 0 || crowd.handheld < 0 || crowd.handheld > 10 ||
				crowd.previews < 0 || crowd.previews+crawlersPerHundred+automationPerHundred > 50 {
				t.Errorf("%s is shared through %q, whose audience is %+v", entry.alias, entry.campaign.Source, crowd)
			}
			used[entry.campaign.Source] = true
		}
	}
	for source := range audiences {
		if !used[source] {
			t.Errorf("the seed has an audience for %q, and no link shared through it", source)
		}
	}
	whole := 0
	for _, weight := range hours {
		whole += weight
		if weight < 1 {
			t.Errorf("an hour of the day gets %d requests of %v", weight, hours)
		}
	}
	// Every hour of the day is reached, and the afternoon is busier than
	// the small hours.
	var byHour [24]int
	for _, events := range told(t, historyDay) {
		for _, event := range events {
			byHour[event.OccurredAt.Hour()]++
		}
	}
	if slices.Min(byHour[:]) == 0 || byHour[3]*4 > byHour[15] || whole == 0 {
		t.Errorf("the requests of the history, by hour of the day: %v", byHour)
	}
}
