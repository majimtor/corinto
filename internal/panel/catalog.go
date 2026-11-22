package panel

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// language is a language the panel is read in. It is the first segment of
// the path of a page, and the declared language of the page follows it
// (PLAN.md section 16).
type language string

// The languages of the panel. Spanish is the source: a text that a
// catalogue lacks is shown in Spanish (ADR 0019).
const (
	spanish language = "es"
	english language = "en"
)

var languages = []language{spanish, english}

// The catalogues: one file a language, with the texts of the interface by
// key. Nothing else in the panel holds a text a person reads.
//
//go:embed catalog/*.json
var catalogFiles embed.FS

// message is a text of a catalogue. Most have one form. One that counts
// something has two, for one thing and for any other number of them, which
// is all the plural these two languages need.
type message struct {
	one, other string
	counts     bool
}

func (m *message) UnmarshalJSON(written []byte) error {
	if err := json.Unmarshal(written, &m.other); err == nil {
		return nil
	}
	var forms struct {
		One   *string `json:"one"`
		Other *string `json:"other"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(written)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&forms); err != nil || forms.One == nil || forms.Other == nil {
		return errors.New("a message is a text, or the two texts \"one\" and \"other\"")
	}
	m.one, m.other, m.counts = *forms.One, *forms.Other, true
	return nil
}

// catalogs holds the texts of every language.
type catalogs map[language]map[string]message

func readCatalogs() (catalogs, error) {
	read := make(catalogs, len(languages))
	for _, lang := range languages {
		written, err := catalogFiles.ReadFile("catalog/" + string(lang) + ".json")
		if err != nil {
			return nil, fmt.Errorf("read the catalogue of %q: %w", lang, err)
		}
		var texts map[string]message
		if err := json.Unmarshal(written, &texts); err != nil {
			return nil, fmt.Errorf("read the catalogue of %q: %w", lang, err)
		}
		read[lang] = texts
	}
	return read, nil
}

// find returns the message of a key in a language, or in Spanish when that
// language lacks it.
func (c catalogs) find(lang language, key string) (message, error) {
	if found, ok := c[lang][key]; ok {
		return found, nil
	}
	if found, ok := c[spanish][key]; ok {
		return found, nil
	}
	return message{}, fmt.Errorf("no catalogue has the text %q", key)
}

// placeholder is a part of a message that a value takes the place of.
var placeholder = regexp.MustCompile(`\{[a-z_]+\}`)

// fill puts values in the placeholders of a text. The values come in pairs,
// the name of a placeholder and what goes there. A whole number is written
// as the language writes numbers; anything else, as it is.
func (c catalogs) fill(lang language, text string, pairs ...any) (string, error) {
	if len(pairs)%2 != 0 {
		return "", errors.New("the values of a text come in pairs, a name and a value")
	}
	replacements := make([]string, 0, len(pairs))
	for i := 1; i < len(pairs); i += 2 {
		name, ok := pairs[i-1].(string)
		if !ok {
			return "", fmt.Errorf("the name of a value is a text, not %v", pairs[i-1])
		}
		var value string
		switch given := pairs[i].(type) {
		case int:
			value = c.number(lang, int64(given))
		case int64:
			value = c.number(lang, given)
		default:
			value = fmt.Sprint(given)
		}
		replacements = append(replacements, "{"+name+"}", value)
	}
	// What is left unfilled is looked for before the values go in: a value
	// may hold braces of its own.
	names := make(map[string]bool, len(replacements)/2)
	for i := 0; i < len(replacements); i += 2 {
		names[replacements[i]] = true
	}
	for _, found := range placeholder.FindAllString(text, -1) {
		if !names[found] {
			return "", fmt.Errorf("the text %q was given no value for %s", text, found)
		}
	}
	return strings.NewReplacer(replacements...).Replace(text), nil
}

// text returns the text of a key, with its placeholders filled.
func (c catalogs) text(lang language, key string, pairs ...any) (string, error) {
	found, err := c.find(lang, key)
	if err != nil {
		return "", err
	}
	if found.counts {
		return "", fmt.Errorf("the text %q counts something: it is asked for with its number", key)
	}
	return c.fill(lang, found.other, pairs...)
}

// plural returns the text of a key that counts something, in the form of
// its number, with the number where the text says {n}.
func (c catalogs) plural(lang language, key string, n int64, pairs ...any) (string, error) {
	found, err := c.find(lang, key)
	if err != nil {
		return "", err
	}
	if !found.counts {
		return "", fmt.Errorf("the text %q has one form: it counts nothing", key)
	}
	form := found.other
	if n == 1 {
		form = found.one
	}
	return c.fill(lang, form, append([]any{"n", n}, pairs...)...)
}

// plain returns a text that takes no value and that every catalogue must
// have, such as the mark between thousands. One that is missing is a broken
// build, which the tests of the catalogues catch; here it reads as the key.
func (c catalogs) plain(lang language, key string) string {
	found, err := c.find(lang, key)
	if err != nil {
		return key
	}
	return found.other
}

// number writes a whole number with the mark the language puts between
// thousands.
func (c catalogs) number(lang language, n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if n < 0 {
		sign, digits = "-", digits[1:]
	}
	var grouped strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteString(c.plain(lang, "format.thousands"))
		}
		grouped.WriteRune(digit)
	}
	return sign + grouped.String()
}

// percent writes what part of a whole something is, to the whole percent.
// A part that is there and rounds to nothing is said to be under one, not
// to be nothing.
func (c catalogs) percent(lang language, part, whole int64) (string, error) {
	if whole <= 0 {
		return c.fill(lang, c.plain(lang, "format.percent"), "n", 0)
	}
	rounded := (part*200 + whole) / (whole * 2)
	if rounded == 0 && part > 0 {
		return c.plain(lang, "format.percent_under_one"), nil
	}
	return c.fill(lang, c.plain(lang, "format.percent"), "n", rounded)
}

// The panel shows every time in UTC, and says so (PLAN.md section 5).

// day writes a day without its year, for the labels of a chart.
func (c catalogs) day(lang language, at time.Time) (string, error) {
	at = at.UTC()
	return c.fill(lang, c.plain(lang, "format.day"),
		"day", strconv.Itoa(at.Day()), "month", c.plain(lang, "month."+strconv.Itoa(int(at.Month()))))
}

// date writes a day with its year.
func (c catalogs) date(lang language, at time.Time) (string, error) {
	at = at.UTC()
	return c.fill(lang, c.plain(lang, "format.date"),
		"day", strconv.Itoa(at.Day()), "month", c.plain(lang, "month."+strconv.Itoa(int(at.Month()))),
		"year", strconv.Itoa(at.Year()))
}

// moment writes a day and the time of it, to the minute, with its zone.
func (c catalogs) moment(lang language, at time.Time) (string, error) {
	on, err := c.date(lang, at)
	if err != nil {
		return "", err
	}
	return c.fill(lang, c.plain(lang, "format.moment"), "date", on, "time", at.UTC().Format("15:04"))
}
