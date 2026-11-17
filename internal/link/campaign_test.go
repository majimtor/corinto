package link_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/majimtor/corinto/internal/link"
)

func TestResolveAppendsTheCampaignInAFixedOrder(t *testing.T) {
	full := link.Campaign{Source: "newsletter", Medium: "email", Name: "black friday", Term: "a&b", Content: "top=1"}
	cases := []struct {
		name        string
		destination string
		campaign    link.Campaign
		want        string
	}{
		{"no campaign leaves the destination as it is", "https://example.com/a?b=c#d", link.Campaign{}, "https://example.com/a?b=c#d"},
		{"a destination without a query", "https://example.com/a", link.Campaign{Source: "newsletter"}, "https://example.com/a?utm_source=newsletter"},
		{"a destination with a query", "https://example.com/a?b=c", link.Campaign{Source: "newsletter"}, "https://example.com/a?b=c&utm_source=newsletter"},
		{"an empty query", "https://example.com/a?", link.Campaign{Source: "newsletter"}, "https://example.com/a?utm_source=newsletter"},
		{"a query that ends in an ampersand", "https://example.com/a?b=c&", link.Campaign{Source: "newsletter"}, "https://example.com/a?b=c&utm_source=newsletter"},
		{"the fragment stays at the end", "https://example.com/a#top", link.Campaign{Medium: "email"}, "https://example.com/a?utm_medium=email#top"},
		{"a question mark inside the fragment is not a query", "https://example.com/a#top?x", link.Campaign{Medium: "email"}, "https://example.com/a?utm_medium=email#top?x"},
		{
			"every parameter, in order and encoded", "https://example.com/", full,
			"https://example.com/?utm_source=newsletter&utm_medium=email&utm_campaign=black+friday&utm_term=a%26b&utm_content=top%3D1",
		},
		{"only the parameters that are set", "https://example.com/", link.Campaign{Name: "launch", Content: "footer"}, "https://example.com/?utm_campaign=launch&utm_content=footer"},
		{"a value outside ASCII is encoded", "https://example.com/", link.Campaign{Name: "caf\u00e9"}, "https://example.com/?utm_campaign=caf%C3%A9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := link.Resolve(c.destination, c.campaign)
			if err != nil {
				t.Fatalf("Resolve error = %v", err)
			}
			if got != c.want {
				t.Errorf("Resolve = %q, want %q", got, c.want)
			}
			// What is served is itself a destination the rules accept.
			if again, err := link.ParseDestination(got, link.Policy{}); err != nil || again != got {
				t.Errorf("the resolved %q is not a normal destination: %q, %v", got, again, err)
			}
		})
	}
}

func TestResolveRefusesAParameterTheDestinationAlreadyCarries(t *testing.T) {
	campaign := link.Campaign{Source: "newsletter"}
	for _, destination := range []string{
		"https://example.com/?utm_source=other",
		"https://example.com/?a=1&utm_source=other&b=2",
		"https://example.com/?utm_source",
		"https://example.com/?utm%5Fsource=other",
		"https://example.com/?utm_source=a;x=1",
	} {
		if _, err := link.Resolve(destination, campaign); !errors.Is(err, link.ErrCampaignConflict) {
			t.Errorf("Resolve(%q) error = %v, want %v", destination, err, link.ErrCampaignConflict)
		}
	}
	// A parameter the link does not set, or one only named in the fragment,
	// collides with nothing.
	for _, destination := range []string{
		"https://example.com/?utm_medium=email",
		"https://example.com/?my_utm_source=x",
		"https://example.com/#utm_source=x",
	} {
		if _, err := link.Resolve(destination, campaign); err != nil {
			t.Errorf("Resolve(%q) error = %v, want none", destination, err)
		}
	}
}

func TestResolveBoundsTheValuesAndTheResult(t *testing.T) {
	cases := []struct {
		name        string
		destination string
		campaign    link.Campaign
		want        error
	}{
		{"200 characters", "https://example.com/", link.Campaign{Name: strings.Repeat("a", 200)}, nil},
		{"200 characters outside ASCII count as 200", "https://example.com/", link.Campaign{Name: strings.Repeat("\u00e9", 200)}, nil},
		{"201 characters", "https://example.com/", link.Campaign{Name: strings.Repeat("a", 201)}, link.ErrCampaignValue},
		{"a line break", "https://example.com/", link.Campaign{Name: "a\nb"}, link.ErrCampaignValue},
		{"a control character", "https://example.com/", link.Campaign{Term: "a\x00b"}, link.ErrCampaignValue},
		{"bytes that are not text", "https://example.com/", link.Campaign{Term: "a\xffb"}, link.ErrCampaignValue},
		{
			"a resolved destination over the limit",
			"https://example.com/" + strings.Repeat("a", 2000), link.Campaign{Name: strings.Repeat("b", 100)},
			link.ErrDestinationTooLong,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := link.Resolve(c.destination, c.campaign)
			if !errors.Is(err, c.want) {
				t.Fatalf("Resolve error = %v, want %v", err, c.want)
			}
			if err != nil && got != "" {
				t.Errorf("Resolve = %q with an error, want nothing", got)
			}
		})
	}
}

// Whatever the values, what Resolve returns reads back as the same
// parameters and is still a destination the rules accept.
func FuzzResolve(f *testing.F) {
	f.Add("newsletter", "email", "black friday")
	f.Add("a&b=c", "#top", "100%")
	f.Add("", "", "")
	f.Fuzz(func(t *testing.T, source, medium, name string) {
		campaign := link.Campaign{Source: source, Medium: medium, Name: name}
		resolved, err := link.Resolve("https://example.com/a?b=c#d", campaign)
		if err != nil {
			return
		}
		if again, err := link.ParseDestination(resolved, link.Policy{}); err != nil || again != resolved {
			t.Fatalf("the resolved %q is not a normal destination: %q, %v", resolved, again, err)
		}
		seen, err := url.Parse(resolved)
		if err != nil {
			t.Fatal(err)
		}
		query := seen.Query()
		for parameter, value := range map[string]string{"utm_source": source, "utm_medium": medium, "utm_campaign": name} {
			if query.Get(parameter) != value {
				t.Fatalf("%s reads back as %q, want %q, in %q", parameter, query.Get(parameter), value, resolved)
			}
		}
		if query.Get("b") != "c" || seen.Fragment != "d" {
			t.Fatalf("the query or the fragment of the destination changed in %q", resolved)
		}
	})
}
