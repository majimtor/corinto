package chart

import (
	"strings"
	"testing"
)

func TestTheGridEndsAtARoundNumberWithAtMostFiveLines(t *testing.T) {
	for _, c := range []struct {
		most  int64
		step  int64
		count int
	}{
		{most: 0, step: 1, count: 1},
		{most: 1, step: 1, count: 1},
		{most: 3, step: 1, count: 3},
		{most: 5, step: 1, count: 5},
		{most: 6, step: 2, count: 3},
		{most: 7, step: 2, count: 4},
		{most: 11, step: 5, count: 3},
		{most: 89, step: 20, count: 5},
		{most: 100, step: 20, count: 5},
		{most: 101, step: 50, count: 3},
		{most: 2_600, step: 1_000, count: 3},
		{most: 1_000_000, step: 200_000, count: 5},
	} {
		step, count := scale(c.most)
		if step != c.step || count != c.count {
			t.Errorf("scale(%d) = a line every %d, %d of them; want every %d, %d", c.most, step, count, c.step, c.count)
		}
		if ceiling := step * int64(count); ceiling < c.most {
			t.Errorf("scale(%d) ends at %d, under the value it is for", c.most, ceiling)
		}
	}
}

func TestTheLinesStartAtZeroAndKeepTheirProportions(t *testing.T) {
	drawn := NewLines([]int64{0, 50, 100}, []int64{0, 25, 0})
	if drawn.Width != 720 || drawn.Height != 280 || len(drawn.Lines) != 2 {
		t.Fatalf("NewLines drew %d lines in %d by %d", len(drawn.Lines), drawn.Width, drawn.Height)
	}
	// Zero is on the bottom edge and the largest value on the top one: the
	// grid of 100 ends at 100.
	if first, last := drawn.Ticks[0], drawn.Ticks[len(drawn.Ticks)-1]; first.Value != 0 || first.Y != drawn.Bottom || last.Value != 100 || last.Y != drawn.Top {
		t.Errorf("the grid goes from %+v to %+v, want from 0 on the bottom edge to 100 on the top one", first, last)
	}
	if got, want := drawn.Lines[0].Points, "56,248 374,130 692,12"; got != want {
		t.Errorf("the first line is %q, want %q", got, want)
	}
	// A quarter of the height for a quarter of the value.
	if got, want := drawn.Lines[1].Points, "56,248 374,189 692,248"; got != want {
		t.Errorf("the second line is %q, want %q", got, want)
	}
	if markers := drawn.Lines[0].Markers; len(markers) != 3 || markers[1] != (Point{X: 374, Y: 130}) {
		t.Errorf("the markers of the first line are %+v", markers)
	}
	if drawn.X(0) != drawn.Left || drawn.X(2) != drawn.Right {
		t.Errorf("the steps go from %v to %v, want from edge to edge", drawn.X(0), drawn.X(2))
	}
}

func TestASeriesOfOneStepIsDrawnInTheMiddle(t *testing.T) {
	drawn := NewLines([]int64{4})
	if got, want := drawn.Lines[0].Points, "374,12"; got != want {
		t.Errorf("the line is %q, want %q", got, want)
	}
}

func TestASeriesWithNothingInItIsAFlatLineOnTheAxis(t *testing.T) {
	drawn := NewLines(make([]int64, 30))
	for point := range strings.SplitSeq(drawn.Lines[0].Points, " ") {
		if !strings.HasSuffix(point, ",248") {
			t.Fatalf("a point of a series of zeros is at %s, off the bottom edge", point)
		}
	}
	if len(drawn.Ticks) != 2 {
		t.Errorf("the grid of a series of zeros has %d lines, want zero and one", len(drawn.Ticks))
	}
}

func TestALongSeriesIsMarkedEveryFewStepsAndAtItsEnd(t *testing.T) {
	drawn := NewLines(make([]int64, 90))
	markers := drawn.Lines[0].Markers
	if len(markers) > MaxMarkers+1 || markers[len(markers)-1].X != drawn.Right {
		t.Errorf("a series of 90 steps has %d markers, the last at %v; want at most %d, the last on the right edge",
			len(markers), markers[len(markers)-1].X, MaxMarkers+1)
	}
	if short := NewLines(make([]int64, 30)).Lines[0].Markers; len(short) != 30 {
		t.Errorf("a series of 30 steps has %d markers, want one for each", len(short))
	}
}
