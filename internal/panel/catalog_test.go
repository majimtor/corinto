package panel

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/majimtor/corinto/internal/link"
)

// placeholdersOf returns the placeholders of a text, in order and once
// each.
func placeholdersOf(text string) []string {
	found := placeholder.FindAllString(text, -1)
	slices.Sort(found)
	return slices.Compact(found)
}

// Both catalogues hold the same texts, each with the same forms and the
// same placeholders (the engineering rules): a text missing from one, or
// one that takes a value the other does not, is found here and not by
// whoever reads the page.
func TestTheCataloguesHoldTheSameTextsWithTheSameValues(t *testing.T) {
	texts, err := readCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	source := texts[spanish]
	if len(source) == 0 {
		t.Fatal("the Spanish catalogue is empty")
	}
	for _, lang := range languages {
		translated := texts[lang]
		for key, original := range source {
			found, ok := translated[key]
			if !ok {
				t.Errorf("the catalogue of %q lacks %q", lang, key)
				continue
			}
			if found.counts != original.counts {
				t.Errorf("%q counts something in one catalogue and not in the one of %q", key, lang)
			}
			if strings.TrimSpace(found.other) == "" || (found.counts && strings.TrimSpace(found.one) == "") {
				t.Errorf("%q is empty in the catalogue of %q", key, lang)
			}
			if got, want := placeholdersOf(found.other), placeholdersOf(original.other); !slices.Equal(got, want) {
				t.Errorf("%q takes %v in the catalogue of %q and %v in the Spanish one", key, got, lang, want)
			}
			if got, want := placeholdersOf(found.one), placeholdersOf(original.one); !slices.Equal(got, want) {
				t.Errorf("%q, for one thing, takes %v in the catalogue of %q and %v in the Spanish one", key, got, lang, want)
			}
			// A text that counts says its number in both of its forms.
			if found.counts && (!strings.Contains(found.one, "{n}") || !strings.Contains(found.other, "{n}")) {
				t.Errorf("%q counts something and does not say how many, in the catalogue of %q", key, lang)
			}
		}
		for key := range translated {
			if _, ok := source[key]; !ok {
				t.Errorf("the catalogue of %q has %q, which the Spanish one lacks", lang, key)
			}
		}
	}
}

// The texts that the code asks for by a key it puts together, which no
// test of the templates can see: the state of a link and the months.
func TestTheCataloguesNameEveryStateAndEveryMonth(t *testing.T) {
	texts, err := readCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	keys := []string{
		stateKey(link.Link{State: link.StateActive}, now), stateKey(link.Link{State: link.StateDisabled}, now),
		stateKey(link.Link{State: link.StateBlocked}, now), stateKey(link.Link{State: link.StateActive, ExpiresAt: &past}, now),
		"language.name", "format.thousands", "format.percent", "format.percent_under_one", "format.day", "format.date", "format.moment",
	}
	for month := 1; month <= 12; month++ {
		keys = append(keys, "month."+strconv.Itoa(month))
	}
	if len(slices.Compact(slices.Sorted(slices.Values(keys)))) != len(keys) {
		t.Fatalf("two of the keys the code puts together are the same: %v", keys)
	}
	for _, lang := range languages {
		for _, key := range keys {
			if _, ok := texts[lang][key]; !ok {
				t.Errorf("the catalogue of %q lacks %q, which the code asks for", lang, key)
			}
		}
	}
}

func TestATextThatNoCatalogueHasIsAnErrorAndOneOnlyInSpanishIsShownInSpanish(t *testing.T) {
	texts := catalogs{
		spanish: {"greeting": {other: "source {name}"}, "things": {one: "{n} unit", other: "{n} units", counts: true}, "format.thousands": {other: "."}},
		english: {"format.thousands": {other: ","}},
	}
	if got, err := texts.text(english, "greeting", "name", "Ada"); err != nil || got != "source Ada" {
		t.Errorf("a text only the Spanish catalogue has = %q, %v; want it in Spanish", got, err)
	}
	if got, err := texts.plural(english, "things", 1500); err != nil || got != "1,500 units" {
		t.Errorf("a text that counts, only in the Spanish catalogue = %q, %v; want it in Spanish with the number of the language", got, err)
	}
	for what, failed := range map[string]func() (string, error){
		"a text no catalogue has":                          func() (string, error) { return texts.text(english, "farewell") },
		"a text given no value for its name":               func() (string, error) { return texts.text(spanish, "greeting") },
		"a value without its name":                         func() (string, error) { return texts.text(spanish, "greeting", "Ada") },
		"a name that is not a text":                        func() (string, error) { return texts.text(spanish, "greeting", 7, "Ada") },
		"a text that counts, asked for without its number": func() (string, error) { return texts.text(spanish, "things") },
		"a text that does not count, asked for with one":   func() (string, error) { return texts.plural(spanish, "greeting", 2) },
	} {
		if got, err := failed(); err == nil {
			t.Errorf("%s = %q, want an error", what, got)
		}
	}
	// A value may hold braces of its own: they are not a placeholder left
	// unfilled.
	if got, err := texts.text(spanish, "greeting", "name", "{name} {other}"); err != nil || got != "source {name} {other}" {
		t.Errorf("a value with braces = %q, %v", got, err)
	}
}

// Numbers, parts of a whole, plurals and days, as each language writes
// them. What is expected is put together from the catalogue: this test
// holds how a number is grouped and where each piece goes, and no word of
// either language.
func TestNumbersPluralsAndDaysFollowTheLanguage(t *testing.T) {
	texts, err := readCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		n      int64
		es, en string
	}{
		{n: 0, es: "0", en: "0"}, {n: 7, es: "7", en: "7"}, {n: 999, es: "999", en: "999"},
		{n: 1000, es: "1.000", en: "1,000"}, {n: 12345, es: "12.345", en: "12,345"},
		{n: 1234567, es: "1.234.567", en: "1,234,567"}, {n: -1234, es: "-1.234", en: "-1,234"},
	} {
		if es, en := texts.number(spanish, c.n), texts.number(english, c.n); es != c.es || en != c.en {
			t.Errorf("%d is written %q and %q, want %q and %q", c.n, es, en, c.es, c.en)
		}
	}
	for _, lang := range languages {
		percent := func(n string) string { return strings.ReplaceAll(texts.plain(lang, "format.percent"), "{n}", n) }
		for _, c := range []struct {
			part, whole int64
			want        string
		}{
			{part: 1, whole: 3, want: percent("33")}, {part: 2, whole: 3, want: percent("67")},
			{part: 3, whole: 3, want: percent("100")}, {part: 0, whole: 3, want: percent("0")},
			{part: 1, whole: 200, want: percent("1")}, {part: 1, whole: 1000, want: texts.plain(lang, "format.percent_under_one")},
			{part: 5, whole: 0, want: percent("0")},
		} {
			if got, err := texts.percent(lang, c.part, c.whole); err != nil || got != c.want {
				t.Errorf("%d of %d in %q = %q, %v; want %q", c.part, c.whole, lang, got, err, c.want)
			}
		}
		for _, n := range []int64{0, 1, 2, 1500} {
			form := texts[lang]["count.requests"].other
			if n == 1 {
				form = texts[lang]["count.requests"].one
			}
			want := strings.ReplaceAll(form, "{n}", texts.number(lang, n))
			if got, err := texts.plural(lang, "count.requests", n); err != nil || got != want || got == form {
				t.Errorf("%d requests in %q = %q, %v; want %q", n, lang, got, err, want)
			}
		}
		// A moment of another zone is written in UTC: five past seven, the
		// day before the one its own zone says.
		at := time.Date(2026, time.March, 8, 0, 5, 9, 0, time.FixedZone("", 5*60*60))
		month := texts.plain(lang, "month.3")
		for what, c := range map[string]struct {
			write func(language, time.Time) (string, error)
			want  string
		}{
			"the day":    {write: texts.day, want: "7 " + month},
			"the date":   {write: texts.date, want: "7 " + month + " 2026"},
			"the moment": {write: texts.moment, want: "7 " + month + " 2026, 19:05 UTC"},
		} {
			if got, err := c.write(lang, at); err != nil || got != c.want {
				t.Errorf("%s in %q = %q, %v; want %q", what, lang, got, err, c.want)
			}
		}
	}
	if es, en := texts.plain(spanish, "month.3"), texts.plain(english, "month.3"); es == en || es == "month.3" {
		t.Errorf("the third month is %q in Spanish and %q in English", es, en)
	}
	// The two catalogues are the ones of the languages of the panel.
	if got := slices.Sorted(maps.Keys(texts)); !slices.Equal(got, []language{english, spanish}) {
		t.Errorf("the catalogues are of %v", got)
	}
}

// What is wrong with a form is said by a text whose key the code holds, in
// a table or a constant, and no test of the templates can see. Every one
// of them is in both catalogues, and the catalogues have no other.
func TestTheCataloguesSayEverythingThatCanBeWrongWithAForm(t *testing.T) {
	texts, err := readCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	asked := map[string]bool{keyDestinationRequired: true, keyExpiryInvalid: true, keyExpiryPast: true}
	fields := []string{fieldDestination, fieldAlias, fieldTitle, fieldTags, fieldCampaign, fieldExpiresAt, fieldForm}
	rules := make(map[error]bool)
	for _, known := range refusals {
		if asked[known.Key] || rules[known.rule] {
			t.Errorf("the rule %q, or the text %q, is in the table twice", known.rule, known.Key)
		}
		asked[known.Key], rules[known.rule] = true, true
		if !slices.Contains(fields, known.Field) || !strings.HasPrefix(known.Key, "error.") {
			t.Errorf("what breaks the rule %q is said of the field %q with the text %q", known.rule, known.Field, known.Key)
		}
	}
	for _, lang := range languages {
		for key := range asked {
			if found, ok := texts[lang][key]; !ok || found.counts {
				t.Errorf("the catalogue of %q lacks %q, which the code asks for, or has it as a text that counts", lang, key)
			}
		}
		for key := range texts[lang] {
			if strings.HasPrefix(key, "error.") && !asked[key] {
				t.Errorf("the catalogue of %q has %q, which nothing asks for", lang, key)
			}
		}
	}
	// A problem of the form as a whole has no field for the page to lead
	// to, and any other has.
	if (problem{Field: fieldForm}).Linked() || !(problem{Field: fieldDestination}).Linked() {
		t.Error("a problem of the whole form is linked to a field, or one of a field is not")
	}
}
