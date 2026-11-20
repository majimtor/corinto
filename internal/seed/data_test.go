package seed

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/majimtor/corinto/internal/link"
)

// The seed is what PLAN.md section 8 says it is, and every link of it is
// one a workspace could have created.
// policy is the list of the demo workspaces as a workspace holds it: an
// entry is written as an address, and kept as its scheme, host and path.
func policy(t *testing.T) link.Policy {
	t.Helper()
	var entries []link.AllowedPrefix
	for _, entry := range allowed {
		parsed, err := url.Parse(entry)
		if err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
		entries = append(entries, link.AllowedPrefix{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path})
	}
	return link.Policy{Allowed: entries}
}

func TestTheSeedIsTwoWorkspacesWithTheirLinksInThreeCampaigns(t *testing.T) {
	if len(demos) != 2 || demos[0].name == demos[1].name {
		t.Fatalf("the seed has %d demo workspaces, want two with different names", len(demos))
	}
	var (
		aliases   []string
		campaigns []string
		expired   int
	)
	for _, demo := range demos {
		inACampaign, inNone := 0, 0
		for _, entry := range demo.links {
			aliases = append(aliases, entry.alias)
			if _, err := link.ParseAlias(entry.alias); err != nil {
				t.Errorf("%s: the alias is not one a workspace can choose: %v", entry.alias, err)
			}
			// With the list of the demo workspaces, which the seed itself
			// has to keep to.
			destination, err := link.ParseDestination(entry.destination, policy(t))
			if err != nil {
				t.Errorf("%s: the destination is not one a link of a demo workspace can have: %v", entry.alias, err)
			}
			if _, err := link.Resolve(destination, entry.campaign); err != nil {
				t.Errorf("%s: the campaign cannot be added to the destination: %v", entry.alias, err)
			}
			// Nothing of the seed points at a site that somebody has.
			if parsed, err := url.Parse(entry.destination); err != nil || !slices.Contains([]string{"www.example.com", "www.example.org"}, parsed.Host) {
				t.Errorf("%s: the destination %s is not of a domain kept for documentation", entry.alias, entry.destination)
			}
			if entry.title == "" || len(entry.title) > link.MaxTitleLength || len(entry.tags) == 0 || len(entry.tags) > link.MaxTags {
				t.Errorf("%s: the title is %q and the tags %v", entry.alias, entry.title, entry.tags)
			}
			switch {
			case entry.campaign == (link.Campaign{}):
				inNone++
			case entry.campaign.Source == "" || entry.campaign.Medium == "" || entry.campaign.Name == "":
				t.Errorf("%s: a link of a campaign names its source, its medium and the campaign: %+v", entry.alias, entry.campaign)
			default:
				inACampaign++
				if !slices.Contains(campaigns, entry.campaign.Name) {
					campaigns = append(campaigns, entry.campaign.Name)
				}
			}
			if entry.expiredDaysAgo > 0 {
				expired++
			}
		}
		if inACampaign == 0 || inNone == 0 {
			t.Errorf("%s has %d links in a campaign and %d in none, want some of each", demo.name, inACampaign, inNone)
		}
	}
	if len(aliases) < 12 || len(aliases) > 20 || len(campaigns) != 3 || expired != 1 {
		t.Errorf("the seed has %d links in %d campaigns (%s), %d of them expired; want 12 to 20 in three, and one expired",
			len(aliases), len(campaigns), strings.Join(campaigns, ", "), expired)
	}
	slices.Sort(aliases)
	if unique := slices.Compact(slices.Clone(aliases)); len(unique) != len(aliases) {
		t.Errorf("two links of the seed have the same alias: %v", aliases)
	}
}

// A demo workspace is open to whoever starts the system: its links lead to
// the two hosts of the seed and nowhere else.
func TestALinkOfADemoWorkspaceLeadsNowhereElse(t *testing.T) {
	policy := policy(t)
	if len(policy.Allowed) != 2 {
		t.Fatalf("the list of the demo workspaces has %d entries, want the two hosts of the seed", len(policy.Allowed))
	}
	for _, destination := range []string{
		"https://www.example.com/",
		"https://www.example.com/books/new-arrivals",
		"https://www.example.org/cycles/gravel?size=m",
	} {
		if _, err := link.ParseDestination(destination, policy); err != nil {
			t.Errorf("%s: %v, want it allowed", destination, err)
		}
	}
	for _, destination := range []string{
		"https://www.example.net/books",        // another domain kept for documentation
		"https://example.com/books",            // the domain, which is another host
		"https://shop.example.com/books",       // another host of the domain
		"https://www.example.com.example.net/", // a host that begins like one of the list
		"http://www.example.com/books",         // the host of the list without TLS
		"https://www.example.com:8443/books",   // and on another port
	} {
		if _, err := link.ParseDestination(destination, policy); !errors.Is(err, link.ErrDestinationNotAllowed) {
			t.Errorf("%s: %v, want %v", destination, err, link.ErrDestinationNotAllowed)
		}
	}
}
