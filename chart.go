package pulse

import (
	"encoding/json"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
)

// Point is one value of a chart line. A NaN value leaves a gap in the line,
// for intervals where nothing was measured.
type Point struct {
	T time.Time
	V float64
}

// Line is one named series of a TimeSeries. At most four lines are drawn.
type Line struct {
	Name   string
	Points []Point
}

// TimeSeries is a chart over time. All lines share one y axis, so only put
// values of the same unit on one chart.
type TimeSeries struct {
	Lines []Line
	// Unit formats axis and tooltip values: "ms", "%", or any suffix.
	Unit string
	// Bars draws the first line as columns, for counts per interval.
	Bars bool
	// Max fixes the top of the y axis. Zero scales to the data.
	Max float64
}

const maxChartLines = 4

type chartLine struct {
	Slot     int
	Segments []string
	Areas    []string
}

type chartBar struct {
	Height float64
}

type chartLegend struct {
	Slot int
	Name string
}

type chartVM struct {
	Empty  bool
	Bars   []chartBar
	Lines  []chartLine
	Legend []chartLegend
	YTicks []string
	XTicks []string
	Hover  string
}

type chartHover struct {
	Labels []string        `json:"labels"`
	Series []chartHoverSet `json:"series"`
	Bars   bool            `json:"bars"`
}

type chartHoverSet struct {
	Name   string    `json:"name"`
	Slot   int       `json:"slot"`
	Values []string  `json:"values"`
	Y      []float64 `json:"y"`
}

func (ts TimeSeries) render(t *template.Template) (template.HTML, error) {
	lines := ts.Lines
	if len(lines) > maxChartLines {
		lines = lines[:maxChartLines]
	}
	vm := chartVM{Empty: true}
	var maxV float64
	var n int
	for _, l := range lines {
		n = max(n, len(l.Points))
		for _, pt := range l.Points {
			if math.IsNaN(pt.V) {
				continue
			}
			maxV = math.Max(maxV, pt.V)
			if pt.V != 0 || !ts.Bars {
				vm.Empty = false
			}
		}
	}
	if n == 0 || vm.Empty {
		vm.Empty = true
		return execWidget(t, "chart", vm)
	}

	top := ts.Max
	if top <= 0 {
		top = niceCeil(maxV)
	}
	vm.YTicks = []string{FormatValue(top, ts.Unit), FormatValue(top/2, ts.Unit), FormatValue(0, ts.Unit)}

	ref := lines[0].Points
	span := ref[len(ref)-1].T.Sub(ref[0].T)
	step := time.Duration(0)
	if len(ref) > 1 {
		step = ref[1].T.Sub(ref[0].T)
	}
	for _, i := range []int{0, len(ref) / 2, len(ref) - 1} {
		vm.XTicks = append(vm.XTicks, axisTime(ref[i].T, span))
	}

	hover := chartHover{Bars: ts.Bars}
	for _, pt := range ref {
		hover.Labels = append(hover.Labels, hoverTime(pt.T, step, span))
	}

	for li, l := range lines {
		set := chartHoverSet{Name: l.Name, Slot: li + 1}
		cl := chartLine{Slot: li + 1}
		var seg []string
		closeSegment := func() {
			if len(seg) == 0 {
				return
			}
			first, _, _ := strings.Cut(seg[0], ",")
			last, _, _ := strings.Cut(seg[len(seg)-1], ",")
			if len(seg) == 1 {
				seg = append(seg, seg[0])
			}
			points := strings.Join(seg, " ")
			cl.Segments = append(cl.Segments, points)
			if len(lines) == 1 {
				cl.Areas = append(cl.Areas, first+",100 "+points+" "+last+",100")
			}
			seg = nil
		}
		for i, pt := range l.Points {
			if math.IsNaN(pt.V) {
				set.Values = append(set.Values, "–")
				set.Y = append(set.Y, -1)
				closeSegment()
				continue
			}
			y := math.Min(pt.V/top, 1) * 100
			set.Values = append(set.Values, FormatValue(pt.V, ts.Unit))
			set.Y = append(set.Y, round2(y))
			if ts.Bars && li == 0 {
				vm.Bars = append(vm.Bars, chartBar{Height: round2(y)})
				continue
			}
			x := 50.0
			if len(l.Points) > 1 {
				x = float64(i) / float64(len(l.Points)-1) * 100
			}
			seg = append(seg, strconv.FormatFloat(round2(x), 'f', -1, 64)+","+strconv.FormatFloat(round2(100-y), 'f', -1, 64))
		}
		closeSegment()
		hover.Series = append(hover.Series, set)
		if len(cl.Segments) > 0 {
			vm.Lines = append(vm.Lines, cl)
		}
		if len(lines) > 1 {
			vm.Legend = append(vm.Legend, chartLegend{Slot: li + 1, Name: l.Name})
		}
	}

	data, err := json.Marshal(hover)
	if err != nil {
		return "", err
	}
	vm.Hover = string(data)
	return execWidget(t, "chart", vm)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// niceCeil rounds up to 1, 2, 2.5 or 5 times a power of ten.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*exp {
			return m * exp
		}
	}
	return 10 * exp
}

func axisTime(t time.Time, span time.Duration) string {
	if span > 24*time.Hour {
		return t.Format("Jan 2 15:04")
	}
	return t.Format("15:04")
}

func hoverTime(t time.Time, step, span time.Duration) string {
	switch {
	case span > 24*time.Hour:
		return t.Format("Mon Jan 2, 15:04")
	case step < time.Minute:
		return t.Format("15:04:05")
	default:
		return t.Format("15:04")
	}
}

// FormatValue renders a number with a unit the way the built-in cards do.
func FormatValue(v float64, unit string) string {
	switch unit {
	case "ms":
		return FormatMS(v)
	case "%":
		return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) + "%"
	case "":
		return FormatCount(v)
	default:
		return FormatCount(v) + " " + unit
	}
}

// FormatMS renders a duration given in milliseconds.
func FormatMS(ms float64) string {
	switch {
	case ms == 0:
		return "0 ms"
	case ms < 10:
		return strconv.FormatFloat(math.Round(ms*10)/10, 'f', -1, 64) + " ms"
	case ms < 1000:
		return strconv.FormatFloat(math.Round(ms), 'f', 0, 64) + " ms"
	case ms < 60_000:
		return strconv.FormatFloat(math.Round(ms/10)/100, 'f', -1, 64) + " s"
	case ms < 3_600_000:
		return strconv.FormatFloat(math.Round(ms/6000)/10, 'f', -1, 64) + " min"
	default:
		return strconv.FormatFloat(math.Round(ms/360_000)/10, 'f', -1, 64) + " h"
	}
}

// FormatCount renders a number compactly: 950, 1.2K, 3.4M.
func FormatCount(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1e6:
		return strconv.FormatFloat(math.Round(v/1e5)/10, 'f', -1, 64) + "M"
	case abs >= 1e4:
		return strconv.FormatFloat(math.Round(v/1e2)/10, 'f', -1, 64) + "K"
	case abs >= 100 || v == math.Trunc(v):
		return groupThousands(int64(math.Round(v)))
	default:
		return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
	}
}

func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// FormatBytes renders a byte count in binary units.
func FormatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return strconv.FormatUint(b, 10) + " B"
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(math.Round(float64(b)/float64(div)*10)/10, 'f', -1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

// FormatAgo renders how long ago t was, e.g. "3m ago".
func FormatAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}
