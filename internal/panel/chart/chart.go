// Package chart turns the numbers of a series into the geometry of the line
// chart the panel draws with them: where each point, each marker and each
// line of the grid goes. It writes no markup and holds no text; a template
// places both, so everything a page shows is escaped by html/template
// (ADR 0014).
package chart

import (
	"math"
	"strconv"
	"strings"
)

// The size of the drawing, in the units of its viewBox, and the room it
// leaves around the plot for the labels of the two axes.
const (
	width, height            = 720, 280
	left, right, top, bottom = 56, 28, 12, 32
)

// MaxMarkers is how many points of a line get a marker. A longer series is
// marked every few steps, and always at its last one: markers that touch
// say less than the line does.
const MaxMarkers = 31

// Point is a place in the drawing.
type Point struct{ X, Y float64 }

// Tick is a line of the grid and the value it stands for.
type Tick struct {
	Y     float64
	Value int64
}

// Line is one series: its points as a polyline takes them, and the places
// of its markers.
type Line struct {
	Points  string
	Markers []Point
}

// Lines is the geometry of a line chart of series that share their steps.
type Lines struct {
	Width, Height int
	// The edges of the plot.
	Left, Right, Top, Bottom float64
	// Ticks are the lines of the grid, from zero up.
	Ticks []Tick
	Lines []Line
	// Where the labels of the two axes go: the right end of the values,
	// and the baseline of what names a step.
	ValueLabelsX, StepLabelsY float64

	steps int
}

// NewLines places series that have a value for each of the same steps. The
// vertical axis starts at zero and ends at a round number, so the height of
// a point can be compared with any other.
func NewLines(series ...[]int64) Lines {
	drawn := Lines{
		Width: width, Height: height,
		Left: left, Right: width - right, Top: top, Bottom: height - bottom,
		ValueLabelsX: left - 8, StepLabelsY: height - bottom + 20,
	}
	var most int64
	for _, values := range series {
		drawn.steps = max(drawn.steps, len(values))
		for _, value := range values {
			most = max(most, value)
		}
	}
	step, count := scale(most)
	ceiling := float64(step * int64(count))
	y := func(value int64) float64 {
		return round(drawn.Bottom - float64(value)/ceiling*(drawn.Bottom-drawn.Top))
	}
	for i := 0; i <= count; i++ {
		value := step * int64(i)
		drawn.Ticks = append(drawn.Ticks, Tick{Y: y(value), Value: value})
	}
	every := (drawn.steps + MaxMarkers - 1) / MaxMarkers
	for _, values := range series {
		var (
			points strings.Builder
			line   Line
		)
		for i, value := range values {
			at := Point{X: drawn.X(i), Y: y(value)}
			if i > 0 {
				points.WriteByte(' ')
			}
			points.WriteString(number(at.X) + "," + number(at.Y))
			if i%every == 0 || i == len(values)-1 {
				line.Markers = append(line.Markers, at)
			}
		}
		line.Points = points.String()
		drawn.Lines = append(drawn.Lines, line)
	}
	return drawn
}

// X returns where a step is along the horizontal axis. A series of one
// step is drawn in the middle.
func (l Lines) X(step int) float64 {
	if l.steps < 2 {
		return round((l.Left + l.Right) / 2)
	}
	return round(l.Left + float64(step)*(l.Right-l.Left)/float64(l.steps-1))
}

// scale returns the distance between two lines of the grid and how many
// there are above zero, for values up to most: the smallest of 1, 2 and 5
// times a power of ten that needs at most five lines.
func scale(most int64) (step int64, count int) {
	most = max(most, 1)
	for power := int64(1); ; power *= 10 {
		for _, multiple := range []int64{1, 2, 5} {
			step = multiple * power
			if lines := (most + step - 1) / step; lines <= 5 {
				return step, int(lines)
			}
		}
	}
}

// round keeps one decimal: a tenth of a unit of the drawing is under what a
// screen shows, and the markup stays short.
func round(value float64) float64 { return math.Round(value*10) / 10 }

func number(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
