package seed

import "github.com/majimtor/corinto/internal/link"

// planted is a link of the seed. Every one has an alias, so that the same
// short link exists after every run and can be named in a document or a
// test.
type planted struct {
	alias       string
	destination string
	campaign    link.Campaign
	title       string
	tags        []string
	// expiredDaysAgo is how long before the day of the seed the link
	// expired; zero is a link that does not expire.
	expiredDaysAgo int
	// daily is how many requests the link gets on an ordinary day of its
	// history, and sharedDaysAgo how long before the day of the seed it
	// began to be shared: nothing asked for it before, and its first days
	// are its busiest. Zero is a link that was there all along.
	daily         int
	sharedDaysAgo int
}

// demo is a demo workspace of the seed, with its links.
type demo struct {
	name  string
	links []planted
	// week is how the traffic of the workspace goes through a week: what
	// part of an ordinary day each day gets, in hundredths, from Sunday.
	week [7]int
}

// The channels a link is shared through, as the campaign parameters name
// them.
var (
	newsletter = [2]string{"newsletter", "email"}
	instagram  = [2]string{"instagram", "social"}
	facebook   = [2]string{"facebook", "social"}
	youtube    = [2]string{"youtube", "video"}
	podcast    = [2]string{"podcast", "audio"}
	poster     = [2]string{"poster", "print"}
)

// in returns the parameters of a link of a campaign shared through a
// channel.
func in(campaign string, channel [2]string) link.Campaign {
	return link.Campaign{Source: channel[0], Medium: channel[1], Name: campaign}
}

// allowed is where the links of a demo workspace may lead: the two hosts
// the links of the seed lead to, and nowhere else. A demo workspace is open
// to whoever starts the system, and without a list it would redirect to
// any address it was given (PLAN.md section 14).
var allowed = []string{
	"https://www.example.com/",
	"https://www.example.org/",
}

// demos are the two demo workspaces: two, so that what a key of one can do
// to the resources of the other can be tried. Sixteen links in three
// campaigns, some of them in none and one that has expired (PLAN.md
// section 8). The names are made up and the addresses are of the domains
// kept for documentation.
var demos = []demo{
	// A bookshop is followed from desks, on working days.
	{name: "Lighthouse Books", week: [7]int{55, 100, 105, 100, 100, 95, 70}, links: []planted{
		{alias: "spring-reading", destination: "https://www.example.com/books/spring-reading", campaign: in("spring-reading", newsletter),
			title: "Spring reading list", tags: []string{"spring", "newsletter"}, daily: 120, sharedDaysAgo: 62},
		{alias: "spring-reading-ig", destination: "https://www.example.com/books/spring-reading", campaign: in("spring-reading", instagram),
			title: "Spring reading list, Instagram", tags: []string{"spring", "social"}, daily: 90, sharedDaysAgo: 62},
		{alias: "spring-reading-pod", destination: "https://www.example.com/books/spring-reading", campaign: in("spring-reading", podcast),
			title: "Spring reading list, podcast", tags: []string{"spring", "audio"}, daily: 30, sharedDaysAgo: 55},
		{alias: "staff-picks", destination: "https://www.example.com/books/staff-picks", campaign: in("spring-reading", newsletter),
			title: "Staff picks", tags: []string{"spring", "newsletter"}, daily: 55, sharedDaysAgo: 48},
		{alias: "signed-copies", destination: "https://www.example.com/books/signed-copies", campaign: in("spring-reading", instagram),
			title: "Signed copies", tags: []string{"spring", "social"}, daily: 25, sharedDaysAgo: 40},
		{alias: "author-tour", destination: "https://www.example.com/events/author-tour", campaign: in("author-tour", newsletter),
			title: "Author tour dates", tags: []string{"events", "newsletter"}, daily: 110, sharedDaysAgo: 21},
		{alias: "author-tour-poster", destination: "https://www.example.com/events/author-tour", campaign: in("author-tour", poster),
			title: "Author tour, poster", tags: []string{"events", "print"}, daily: 20, sharedDaysAgo: 18},
		{alias: "tour-tickets", destination: "https://www.example.com/events/author-tour/tickets", campaign: in("author-tour", facebook),
			title: "Author tour tickets", tags: []string{"events", "social"}, daily: 80, sharedDaysAgo: 16},
		{alias: "opening-hours", destination: "https://www.example.com/visit", title: "Opening hours", tags: []string{"shop"}, daily: 45},
		{alias: "winter-clearance", destination: "https://www.example.com/books/winter-clearance", title: "Winter clearance",
			tags: []string{"ended"}, expiredDaysAgo: 20, daily: 60},
	}},
	// A bicycle shop is followed when a ride is being planned: at weekends.
	{name: "Cascade Cycles", week: [7]int{125, 90, 90, 95, 100, 110, 135}, links: []planted{
		{alias: "summer-sale", destination: "https://www.example.org/cycles/summer-sale", campaign: in("summer-sale", newsletter),
			title: "Summer sale", tags: []string{"summer", "newsletter"}, daily: 150, sharedDaysAgo: 45},
		{alias: "summer-sale-ig", destination: "https://www.example.org/cycles/summer-sale", campaign: in("summer-sale", instagram),
			title: "Summer sale, Instagram", tags: []string{"summer", "social"}, daily: 110, sharedDaysAgo: 45},
		{alias: "summer-sale-yt", destination: "https://www.example.org/cycles/summer-sale", campaign: in("summer-sale", youtube),
			title: "Summer sale, YouTube", tags: []string{"summer", "video"}, daily: 75, sharedDaysAgo: 38},
		{alias: "tune-up-offer", destination: "https://www.example.org/workshop/tune-up", campaign: in("summer-sale", newsletter),
			title: "Tune-up offer", tags: []string{"summer", "workshop"}, daily: 40, sharedDaysAgo: 30},
		{alias: "ride-map", destination: "https://www.example.org/rides/map", campaign: in("summer-sale", instagram),
			title: "Summer ride map", tags: []string{"summer", "rides"}, daily: 110, sharedDaysAgo: 12},
		{alias: "workshop-booking", destination: "https://www.example.org/workshop/booking", title: "Workshop booking", tags: []string{"workshop"}, daily: 55},
	}},
}
