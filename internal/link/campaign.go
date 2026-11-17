package link

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxCampaignValueLength bounds one campaign parameter, in characters. It is
// generous for any real campaign name and keeps the resolved destination
// bounded.
const maxCampaignValueLength = 200

// Why campaign parameters are refused.
var (
	ErrCampaignValue    = errors.New("a campaign parameter has at most 200 characters and no control characters")
	ErrCampaignConflict = errors.New("the destination already carries a campaign parameter that the link sets")
)

// Campaign holds the campaign parameters of a link. They are attributes of
// the link, not of a request: one channel is one link. An empty field is a
// parameter the link does not set.
type Campaign struct {
	Source  string
	Medium  string
	Name    string
	Term    string
	Content string
}

// parameters returns the parameters the campaign sets, in the fixed order
// in which they are appended.
func (c Campaign) parameters() [][2]string {
	all := [][2]string{
		{"utm_source", c.Source},
		{"utm_medium", c.Medium},
		{"utm_campaign", c.Name},
		{"utm_term", c.Term},
		{"utm_content", c.Content},
	}
	set := all[:0]
	for _, parameter := range all {
		if parameter[1] != "" {
			set = append(set, parameter)
		}
	}
	return set
}

// Resolve returns what the redirect serves: the destination, as
// ParseDestination returned it, with the campaign parameters appended to
// its query. It is computed when the link is written, never per request.
//
// A destination that already carries a parameter the campaign sets is
// refused: one of the two values would win in silence.
func Resolve(destination string, campaign Campaign) (string, error) {
	parameters := campaign.parameters()
	if len(parameters) == 0 {
		return destination, nil
	}

	base, fragment := destination, ""
	if i := strings.IndexByte(destination, '#'); i >= 0 {
		base, fragment = destination[:i], destination[i:]
	}
	_, query, hasQuery := strings.Cut(base, "?")

	var appended strings.Builder
	for i, parameter := range parameters {
		name, value := parameter[0], parameter[1]
		if !validCampaignValue(value) {
			return "", ErrCampaignValue
		}
		if carries(query, name) {
			return "", ErrCampaignConflict
		}
		if i > 0 {
			appended.WriteByte('&')
		}
		appended.WriteString(name + "=" + url.QueryEscape(value))
	}

	separator := "?"
	switch {
	case hasQuery && (query == "" || strings.HasSuffix(query, "&")):
		separator = ""
	case hasQuery:
		separator = "&"
	}
	resolved := base + separator + appended.String() + fragment
	if len(resolved) > maxDestinationLength {
		return "", ErrDestinationTooLong
	}
	return resolved, nil
}

// carries reports whether a query already has a parameter of that name.
// Names are compared decoded, so that utm%5Fsource is utm_source, and only
// the ampersand separates parameters, as in a browser.
func carries(query, name string) bool {
	for pair := range strings.SplitSeq(query, "&") {
		key, _, _ := strings.Cut(pair, "=")
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		if key == name {
			return true
		}
	}
	return false
}

func validCampaignValue(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxCampaignValueLength {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}
