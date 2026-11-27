package panel

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var (
	// The block of the stylesheet that names its colours, and a colour in
	// it.
	paletteBlock  = regexp.MustCompile(`:root\s*\{([^}]*)\}`)
	paletteColour = regexp.MustCompile(`--([a-z0-9-]+):\s*#([0-9a-f]{6});`)
	// A colour written where it is used, and a colour taken by its name.
	literalColour = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\b(?:rgb|rgba|hsl|hsla)\(`)
	namedColour   = regexp.MustCompile(`var\(--([a-z0-9-]+)\)`)
)

// palette returns the colours the stylesheet names, and the stylesheet
// without the block that names them.
func palette(t *testing.T) (map[string]string, string) {
	t.Helper()
	sheet := string(stylesheet)
	block := paletteBlock.FindStringSubmatch(sheet)
	if block == nil {
		t.Fatal("the stylesheet has no block that names its colours")
	}
	colours := make(map[string]string)
	for _, found := range paletteColour.FindAllStringSubmatch(block[1], -1) {
		colours[found[1]] = found[2]
	}
	if len(colours) == 0 {
		t.Fatal("the stylesheet names no colour")
	}
	return colours, strings.Replace(sheet, block[0], "", 1)
}

// luminance returns the relative luminance of a colour, as WCAG 2.2
// defines it for sRGB.
func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	channel := func(at int) float64 {
		value, err := strconv.ParseUint(hex[at:at+2], 16, 8)
		if err != nil {
			t.Fatalf("%q is not a colour: %v", hex, err)
		}
		c := float64(value) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(0) + 0.7152*channel(2) + 0.0722*channel(4)
}

// contrast returns the contrast ratio of two colours, from 1 to 21.
func contrast(t *testing.T, one, other string) float64 {
	t.Helper()
	a, b := luminance(t, one), luminance(t, other)
	return (max(a, b) + 0.05) / (min(a, b) + 0.05)
}

// The colours of the panel keep the contrast WCAG 2.2 asks at level AA:
// 4.5 to 1 for text (1.4.3) and 3 to 1 for what a chart draws and for the
// edges that tell the parts of a page apart (1.4.11). Each pair below is
// one the stylesheet puts together, and the stylesheet uses no colour that
// is not in one of them: a colour that changes, or a new one, is checked
// here and not by eye. docs/accessibility.md gives the ratios.
func TestTheColoursOfTheStylesheetKeepTheirContrast(t *testing.T) {
	colours, rules := palette(t)
	const text, graphic = 4.5, 3.0
	pairs := []struct {
		fore, back string
		atLeast    float64
		what       string
	}{
		{"ink", "surface", text, "the text of a page"},
		{"ink-soft", "surface", text, "the text that explains, and the labels inside a chart"},
		{"link", "surface", text, "a link, and the ring of the focus"},
		{"critical", "surface", text, "what is wrong with a form"},
		{"surface", "link", text, "the text of the button a page is for"},
		{"surface", "critical", text, "the text of the button that cannot be taken back"},
		{"ink", "notice", text, "the text of a notice"},
		{"edge", "surface", graphic, "the edge between two parts of a page, and the mark of a change of rules"},
		{"series-1", "surface", graphic, "the first series of a chart"},
		{"series-2", "surface", graphic, "the second series of a chart"},
		{"series-1", "grid", graphic, "the bar of a breakdown on its track"},
	}
	checked := make(map[string]bool)
	for _, pair := range pairs {
		fore, known := colours[pair.fore]
		back, also := colours[pair.back]
		if !known || !also {
			t.Errorf("the stylesheet does not name %q or %q, which %s needs", pair.fore, pair.back, pair.what)
			continue
		}
		checked[pair.fore], checked[pair.back] = true, true
		if got := contrast(t, fore, back); got < pair.atLeast {
			t.Errorf("%s: %s on %s has a contrast of %.2f to 1, under %.1f", pair.what, pair.fore, pair.back, got, pair.atLeast)
		}
	}
	// The contrast is of two colours, whichever is in front: white on black
	// is 21 to 1, and a colour on itself 1 to 1.
	if got := contrast(t, "ffffff", "000000"); math.Abs(got-21) > 0.001 || contrast(t, "777777", "777777") != 1 || contrast(t, "000000", "ffffff") != got {
		t.Errorf("white on black has a contrast of %.3f to 1, want 21, the same as black on white", got)
	}

	// Every colour the stylesheet names is in a pair. The grid alone is
	// behind something: the lines that help to read a value in a chart and
	// the lines between rows, which say nothing by themselves.
	for name := range colours {
		if !checked[name] {
			t.Errorf("the stylesheet names the colour %q, and no pair checks its contrast", name)
		}
	}
	// And every colour it uses is one it names: none is written where it
	// is used, where this test would not see it.
	if stray := literalColour.FindAllString(rules, -1); len(stray) > 0 {
		t.Errorf("the stylesheet writes colours where it uses them: %q", stray)
	}
	var used []string
	for _, found := range namedColour.FindAllStringSubmatch(rules, -1) {
		if _, known := colours[found[1]]; !known {
			t.Errorf("the stylesheet uses the colour %q, which it does not name", found[1])
		}
		used = append(used, found[1])
	}
	slices.Sort(used)
	if used = slices.Compact(used); len(used) != len(colours) {
		t.Errorf("the stylesheet names %d colours and uses %d of them: %v", len(colours), len(used), used)
	}
}
