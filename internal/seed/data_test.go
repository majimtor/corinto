package seed

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/majimtor/corinto/internal/link"
)

// The seed is what PLAN.md section 8 says it is, and every link of it is
// one a workspace could have created.
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
			destination, err := link.ParseDestination(entry.destination, link.Policy{})
			if err != nil {
				t.Errorf("%s: the destination is not one a link can have: %v", entry.alias, err)
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
