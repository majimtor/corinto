package seed

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/majimtor/corinto/internal/classify"
	"github.com/majimtor/corinto/internal/ingest"
	"github.com/majimtor/corinto/internal/postgres"
	"github.com/majimtor/corinto/internal/rollup"
)

// historyDays is how many days the history of the seed covers: the ninety
// before the day it runs (PLAN.md section 8). It ends the day before, so
// that it does not depend on the hour the seed runs at, and the day itself
// is left to whoever tries the system.
const historyDays = 90

// The two numbers the generator of the history starts from. They are
// fixed: a seed leaves the history that any other seed leaves on that day.
const (
	generatorFirst  = 0x636f72696e746f // "corinto"
	generatorSecond = 8                // the section of the plan that asks for the seed
)

// audience is who follows the links shared through a channel.
type audience struct {
	// referrers are the sites a click says it comes from, each as likely
	// as the others. An empty one is a click that names no site.
	referrers []string
	// handheld is how many of every ten people come with a phone or a
	// tablet.
	handheld int
	// previews is how many of every hundred requests are the preview that
	// a social network or a messenger asks for when the link is posted.
	previews int
}

// audiences are the audiences of the channels, by the source the campaign
// parameters name. The one with no name is of the links in no campaign,
// which are found by searching or typed. The sites are names of the
// domains kept for documentation.
var audiences = map[string]audience{
	"newsletter": {referrers: []string{"", "", "", "mail.example.net", "mail.example.org"}, handheld: 5, previews: 2},
	"instagram":  {referrers: []string{"", "social.example", "social.example", "l.social.example"}, handheld: 9, previews: 12},
	"facebook":   {referrers: []string{"", "social.example.net", "social.example.net", "m.social.example.net"}, handheld: 7, previews: 14},
	"youtube":    {referrers: []string{"", "video.example", "video.example", "m.video.example"}, handheld: 6, previews: 5},
	"podcast":    {referrers: []string{"", "", "", "podcasts.example"}, handheld: 9, previews: 3},
	"poster":     {referrers: []string{""}, handheld: 10, previews: 0},
	"":           {referrers: []string{"", "search.example.net", "search.example.net", "maps.example.org", "www.example.com"}, handheld: 6, previews: 4},
}

// Of every hundred requests to a link, how many come from what is not a
// person, whatever the channel, and how many of the people's are a page
// that their browser fetches ahead.
const (
	crawlersPerHundred   = 6
	automationPerHundred = 3
	prefetchPerHundred   = 4
)

// The clients of the history, by the user agent each sends. The history
// holds requests and not verdicts: the classifier of the redirect says
// what each one is, so a rule that changes classifies the history as it
// classifies that client. A test keeps each list in the class it is here
// for.
var (
	desktops = []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.1 Safari/605.1.15",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:145.0) Gecko/20100101 Firefox/145.0",
		"Mozilla/5.0 (X11; Linux x86_64; rv:145.0) Gecko/20100101 Firefox/145.0",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0",
	}
	handhelds = []string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 19_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.1 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 19_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.1 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Mobile Safari/537.36",
		"Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Mobile Safari/537.36",
		"Mozilla/5.0 (Linux; Android 16; SM-S931B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/29.0 Chrome/136.0.0.0 Mobile Safari/537.36",
		"Mozilla/5.0 (iPad; CPU OS 19_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.1 Mobile/15E148 Safari/604.1",
	}
	crawlers = []string{
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
	}
	previewers = []string{
		"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
		"Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)",
		"WhatsApp/2.23.20.0",
	}
	libraries = []string{
		"curl/8.11.1",
		"python-requests/2.32.3",
		"Go-http-client/2.0",
	}
)

// hours is how the requests of a day spread over its hours, in UTC: few at
// night, most from the morning to the evening.
var hours = [24]int{1, 1, 1, 1, 1, 2, 4, 7, 10, 12, 12, 11, 11, 12, 11, 10, 10, 11, 12, 12, 10, 7, 4, 2}

// launch is how much busier than an ordinary day a link is on each of the
// first days it is shared, in hundredths: four times on the first, and
// back to ordinary within a week.
var launch = []int{400, 320, 250, 200, 160, 130, 115}

// lateShare is what part of its ordinary requests a link still gets once
// its time is up: one in twelve, from whoever kept the link.
const lateShare = 12

// The reason the seed gives for its dead letter. 23514 is the code of a
// check that does not hold, which is how the database refuses an event
// when it refuses one; the constraint it names exists nowhere, and says
// whose it is.
const (
	deadLetterCode       = "23514"
	deadLetterConstraint = "seed_example"
	// deadLetterDaysAgo is how long before the day of the seed the access
	// of that event was, at noon.
	deadLetterDaysAgo = 3
)

// countPollEvery is how often the seed asks whether its history is counted
// while a consumer of another process holds the last of it.
const countPollEvery = 100 * time.Millisecond

// sown is a link of the seed once it exists: what the seed says of it, and
// the identifier the database gave it.
type sown struct {
	planted
	id string
}

// field is a demo workspace once it exists, with its links.
type field struct {
	demo        demo
	workspaceID string
	links       []sown
}

// requestsOn returns how many requests a link gets on a day of its
// history, a number of days before the day of the seed, and how they are
// answered: a link whose time is up is still asked for now and then, and
// redirects no longer.
func (p planted) requestsOn(ago int, weekday time.Weekday, week [7]int, random *rand.Rand) (int, ingest.Outcome) {
	if p.sharedDaysAgo > 0 && ago > p.sharedDaysAgo {
		return 0, ingest.OutcomeRedirected
	}
	// The day of the week, and between 85 and 115 hundredths of it: no two
	// Mondays are the same.
	share := week[weekday] * (85 + random.IntN(31))
	// A link that expired did so at the midnight that begins that day.
	if p.expiredDaysAgo > 0 && ago <= p.expiredDaysAgo {
		return (p.daily*share + 5000*lateShare) / (10000 * lateShare), ingest.OutcomeInactive
	}
	if since := p.sharedDaysAgo - ago; p.sharedDaysAgo > 0 && since < len(launch) {
		share = share * launch[since] / 100
	}
	return (p.daily*share + 5000) / 10000, ingest.OutcomeRedirected
}

// request makes up a request to a link shared with an audience: its method
// and the headers the redirect reads.
func request(crowd audience, random *rand.Rand) (string, http.Header) {
	header := http.Header{}
	pick := func(from []string) string { return from[random.IntN(len(from))] }
	switch kind := random.IntN(100); {
	case kind < crowd.previews:
		header.Set("User-Agent", pick(previewers))
		return http.MethodGet, header
	case kind < crowd.previews+crawlersPerHundred:
		header.Set("User-Agent", pick(crawlers))
		return http.MethodGet, header
	case kind < crowd.previews+crawlersPerHundred+automationPerHundred:
		header.Set("User-Agent", pick(libraries))
		// A monitor asks for the headers alone as often as for the page.
		if random.IntN(2) == 0 {
			return http.MethodHead, header
		}
		return http.MethodGet, header
	}

	// A person, with a browser.
	browsers := desktops
	if random.IntN(10) < crowd.handheld {
		browsers = handhelds
	}
	header.Set("User-Agent", pick(browsers))
	if site := pick(crowd.referrers); site != "" {
		header.Set("Referer", "https://"+site+"/")
	}
	// A browser that fetches the page ahead, in case the person follows
	// the link it shows.
	if random.IntN(100) < prefetchPerHundred {
		header.Set("Sec-Purpose", "prefetch")
	}
	return http.MethodGet, header
}

// moment makes up when in its day a request is made.
func moment(random *rand.Rand) time.Duration {
	whole := 0
	for _, weight := range hours {
		whole += weight
	}
	hour, at := 0, random.IntN(whole)
	for at >= hours[hour] {
		at -= hours[hour]
		hour++
	}
	return time.Duration(hour)*time.Hour + time.Duration(random.IntN(3600))*time.Second
}

// history makes up the accesses of a number of days before a day, for the
// links of the demo workspaces, as the events the redirect would have
// written for them: each is a request that the classifier of the redirect
// classified. It returns them by workspace, in the order of the fields.
// The same day gives the same history.
func history(classifier *classify.Classifier, day time.Time, days int, fields []field) [][]ingest.Event {
	random := rand.New(rand.NewPCG(generatorFirst, generatorSecond)) //nolint:gosec // A history anyone can make again, on purpose: nothing here is a secret.
	told := make([][]ingest.Event, len(fields))
	for i, field := range fields {
		for _, link := range field.links {
			crowd := audiences[link.campaign.Source]
			// From the oldest day to the day before the seed.
			for ago := days; ago >= 1; ago-- {
				date := day.AddDate(0, 0, -ago)
				requests, outcome := link.requestsOn(ago, date.Weekday(), field.demo.week, random)
				for range requests {
					method, header := request(crowd, random)
					told[i] = append(told[i], ingest.Event{
						LinkID: link.id, WorkspaceID: field.workspaceID,
						OccurredAt: date.Add(moment(random)),
						Outcome:    outcome, Method: method,
						ReferrerHost:   ingest.ReferrerHost(header.Get("Referer")),
						Classification: classifier.Classify(method, header),
					})
				}
			}
		}
	}
	return told
}

// tell writes the history of the demo workspaces and has it counted, and
// returns how many events it wrote for each. The events reach the inbox as
// the redirect delivers them and the aggregates through the consumer the
// worker runs: the figures of a demo workspace are made the way any others
// are. One more event, of the first workspace, is set aside as a dead
// letter, so that the reconciliation has one to show.
//
// It returns when nothing of the history waits to be counted.
func tell(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, day time.Time, days int, fields []field) ([]int, error) {
	written := make([]int, len(fields))
	if days <= 0 {
		return written, nil
	}
	classifier, err := classify.New()
	if err != nil {
		return nil, err
	}
	inbox := ingest.NewStore(pool, logger)
	for i, events := range history(classifier, day, days, fields) {
		if err := ingest.Deliver(ctx, inbox, logger, events); err != nil {
			return nil, err
		}
		written[i] = len(events)
	}
	if err := setAside(ctx, pool, classifier, day, fields[0]); err != nil {
		return nil, err
	}
	written[0]++

	consumer := rollup.NewConsumer(pool, logger)
	for {
		processed, err := consumer.Batch(ctx)
		if err != nil {
			return nil, fmt.Errorf("count the history of the seed: %w", err)
		}
		if processed > 0 {
			continue
		}
		// Nothing is left to take. The consumer of another process may
		// still hold some of it: the history is counted when none waits.
		var pending int64
		for _, field := range fields {
			waiting, err := inbox.DemoEventsPending(ctx, field.workspaceID)
			if err != nil {
				return nil, err
			}
			pending += waiting
		}
		if pending == 0 {
			return written, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("count the history of the seed: %w", ctx.Err())
		case <-time.After(countPollEvery):
		}
	}
}

// setAside leaves one dead letter in a demo workspace: a click on its first
// link, a few days before the day of the seed, with the reason of the
// seed. No event of the seed can fail, so this one does not get there the
// way a real one would (ingest.SetAsideDemoEvent).
func setAside(ctx context.Context, pool *pgxpool.Pool, classifier *classify.Classifier, day time.Time, in field) error {
	header := http.Header{"User-Agent": {desktops[0]}}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the dead letter of the seed: %w", err)
	}
	// After a commit this does nothing; before one it undoes everything.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := ingest.SetAsideDemoEvent(ctx, tx, ingest.Event{
		LinkID: in.links[0].id, WorkspaceID: in.workspaceID,
		OccurredAt: day.AddDate(0, 0, -deadLetterDaysAgo).Add(12 * time.Hour),
		Outcome:    ingest.OutcomeRedirected, Method: http.MethodGet,
		Classification: classifier.Classify(http.MethodGet, header),
	}, postgres.Refusal{Code: deadLetterCode, Constraint: deadLetterConstraint}); err != nil {
		return fmt.Errorf("set aside the dead letter of the seed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the dead letter of the seed: %w", err)
	}
	return nil
}
