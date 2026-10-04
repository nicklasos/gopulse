package pulse

import (
	"encoding/json"
	"html"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
)

func renderWidget(t *testing.T, w Widget) string {
	t.Helper()
	out, err := w.render(parseTemplates())
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func points(start time.Time, step time.Duration, values ...float64) []Point {
	pts := make([]Point, len(values))
	for i, v := range values {
		pts[i] = Point{T: start.Add(time.Duration(i) * step), V: v}
	}
	return pts
}

var reHover = regexp.MustCompile(`data-hover="([^"]*)"`)

func hoverData(t *testing.T, markup string) chartHover {
	t.Helper()
	m := reHover.FindStringSubmatch(markup)
	if m == nil {
		t.Fatal("chart has no hover data")
	}
	var h chartHover
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &h); err != nil {
		t.Fatalf("hover data is not valid JSON: %v", err)
	}
	return h
}

func TestLineChartBreaksAtGaps(t *testing.T) {
	start := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	nan := math.NaN()
	out := renderWidget(t, TimeSeries{
		Unit:  "ms",
		Lines: []Line{{Name: "Average", Points: points(start, time.Minute, 10, 20, nan, nan, 40, nan, 30)}},
	})

	if got := strings.Count(out, "<polyline"); got != 3 {
		t.Fatalf("drew %d line segments, want 3 separated by the gaps", got)
	}
	if got := strings.Count(out, "<polygon"); got != 3 {
		t.Errorf("drew %d area fills, want one under each segment of a single-line chart", got)
	}
	if strings.Contains(out, `class="legend"`) {
		t.Error("a single series should have no legend")
	}
	if strings.Contains(out, "NaN") {
		t.Error("NaN leaked into the markup")
	}
	h := hoverData(t, out)
	if h.Bars || len(h.Labels) != 7 || len(h.Series) != 1 {
		t.Fatalf("hover = %+v", h)
	}
	if got := h.Series[0].Values; got[0] != "10 ms" || got[2] != "–" || got[4] != "40 ms" {
		t.Errorf("hover values = %v", got)
	}
	if y := h.Series[0].Y; y[4] != 80 || y[2] != -1 {
		t.Errorf("hover positions = %v, want 40 of a 50 top at 80%% and gaps marked -1", y)
	}
	for _, tick := range []string{">50 ms<", ">25 ms<", ">0 ms<", ">10:00<", ">10:06<"} {
		if !strings.Contains(out, tick) {
			t.Errorf("axis tick %s missing", tick)
		}
	}
}

func TestMultiLineChartHasLegendAndNoArea(t *testing.T) {
	start := time.Now()
	lines := make([]Line, 6)
	for i := range lines {
		lines[i] = Line{Name: string(rune('A' + i)), Points: points(start, time.Minute, 1, 2, 3)}
	}
	out := renderWidget(t, TimeSeries{Lines: lines})
	if got := strings.Count(out, "<polyline"); got != maxChartLines {
		t.Fatalf("drew %d lines, want at most %d", got, maxChartLines)
	}
	if strings.Contains(out, "<polygon") {
		t.Error("area fills should only be drawn for a single line")
	}
	if got := strings.Count(out, `class="sw s`); got != maxChartLines {
		t.Errorf("legend has %d entries, want %d", got, maxChartLines)
	}
	if h := hoverData(t, out); len(h.Series) != maxChartLines || h.Series[3].Slot != 4 {
		t.Errorf("hover series = %+v", h.Series)
	}
}

func TestBarChart(t *testing.T) {
	out := renderWidget(t, TimeSeries{
		Bars:  true,
		Unit:  "/min",
		Lines: []Line{{Name: "Requests", Points: points(time.Now(), time.Minute, 0, 50, 100)}},
	})
	for _, want := range []string{`style="height:0%"`, `style="height:50%"`, `style="height:100%"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing bar %s", want)
		}
	}
	if strings.Contains(out, "<svg") {
		t.Error("a bar-only chart should not draw lines")
	}
	if h := hoverData(t, out); !h.Bars || h.Series[0].Values[2] != "100 /min" {
		t.Errorf("hover = %+v", h)
	}
}

func TestChartEmptyStates(t *testing.T) {
	now := time.Now()
	nan := math.NaN()
	cases := map[string]TimeSeries{
		"no lines":           {},
		"no points":          {Lines: []Line{{Name: "a"}}},
		"all-zero bar chart": {Bars: true, Lines: []Line{{Name: "a", Points: points(now, time.Minute, 0, 0)}}},
		"only gaps":          {Lines: []Line{{Name: "a", Points: points(now, time.Minute, nan, nan)}}},
	}
	for name, ts := range cases {
		if out := renderWidget(t, ts); !strings.Contains(out, "No data in this period.") {
			t.Errorf("%s: want the empty message, got %s", name, out)
		}
	}
	flat := TimeSeries{Lines: []Line{{Name: "a", Points: points(now, time.Minute, 0, 0)}}}
	if out := renderWidget(t, flat); !strings.Contains(out, "<polyline") {
		t.Error("a line that is genuinely zero should still be drawn")
	}
}

func TestChartFixedMaxClampsValues(t *testing.T) {
	out := renderWidget(t, TimeSeries{
		Unit: "%", Max: 100,
		Lines: []Line{{Name: "CPU", Points: points(time.Now(), time.Minute, 25, 250)}},
	})
	h := hoverData(t, out)
	if y := h.Series[0].Y; y[0] != 25 || y[1] != 100 {
		t.Fatalf("positions = %v, want values above the fixed top clamped to it", y)
	}
	if !strings.Contains(out, ">100%<") || !strings.Contains(out, ">50%<") {
		t.Error("y axis should use the fixed maximum")
	}
}

func TestChartTimeLabelsFollowWindow(t *testing.T) {
	start := time.Date(2026, 3, 9, 8, 0, 0, 0, time.UTC)
	short := hoverData(t, renderWidget(t, TimeSeries{Lines: []Line{{Name: "a", Points: points(start, 10*time.Second, 1, 2, 3)}}}))
	if short.Labels[1] != "08:00:10" {
		t.Errorf("sub-minute label = %q, want seconds shown", short.Labels[1])
	}
	hourly := hoverData(t, renderWidget(t, TimeSeries{Lines: []Line{{Name: "a", Points: points(start, time.Hour, 1, 2, 3)}}}))
	if hourly.Labels[1] != "09:00" {
		t.Errorf("hourly label = %q", hourly.Labels[1])
	}
	week := renderWidget(t, TimeSeries{Lines: []Line{{Name: "a", Points: points(start, 24*time.Hour, 1, 2, 3)}}})
	if !strings.Contains(week, ">Mar 9 08:00<") || hoverData(t, week).Labels[2] != "Wed Mar 11, 08:00" {
		t.Errorf("multi-day chart should label dates: %v", hoverData(t, week).Labels)
	}
}

func TestNiceCeil(t *testing.T) {
	for in, want := range map[float64]float64{0: 1, -3: 1, 0.7: 1, 1: 1, 1.2: 2, 23: 25, 26: 50, 99: 100, 870: 1000, 1001: 2000} {
		if got := niceCeil(in); got != want {
			t.Errorf("niceCeil(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestFormatters(t *testing.T) {
	for in, want := range map[float64]string{
		0: "0 ms", 0.34: "0.3 ms", 4.56: "4.6 ms", 12.4: "12 ms", 999.4: "999 ms",
		1500: "1.5 s", 59_000: "59 s", 90_000: "1.5 min", 5_400_000: "1.5 h",
	} {
		if got := FormatMS(in); got != want {
			t.Errorf("FormatMS(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{
		0: "0", 7: "7", 0.256: "0.26", 12.5: "12.5", 999: "999", 1284: "1,284", 9999: "9,999",
		12_900: "12.9K", 4_200_000: "4.2M", -1500: "-1,500", 150.7: "151",
	} {
		if got := FormatCount(in); got != want {
			t.Errorf("FormatCount(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[uint64]string{
		0: "0 B", 1023: "1023 B", 1024: "1 KiB", 1536: "1.5 KiB", 5 << 20: "5 MiB", 3 << 30: "3 GiB",
	} {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		v    float64
		unit string
		want string
	}{{12.34, "%", "12.3%"}, {1500, "ms", "1.5 s"}, {1500, "", "1,500"}, {1500, "USD", "1,500 USD"}} {
		if got := FormatValue(c.v, c.unit); got != c.want {
			t.Errorf("FormatValue(%v, %q) = %q, want %q", c.v, c.unit, got, c.want)
		}
	}
	now := time.Now()
	for ago, want := range map[time.Duration]string{
		time.Second: "just now", 30 * time.Second: "30s ago", 5 * time.Minute: "5m ago",
		3 * time.Hour: "3h ago", 72 * time.Hour: "3d ago",
	} {
		if got := FormatAgo(now.Add(-ago)); got != want {
			t.Errorf("FormatAgo(-%s) = %q, want %q", ago, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		12 * time.Minute: "12m", 3*time.Hour + 5*time.Minute: "3h 5m", 75 * time.Hour: "3d 3h",
	} {
		if got := formatUptime(d); got != want {
			t.Errorf("formatUptime(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestTableWidget(t *testing.T) {
	out := renderWidget(t, Table{
		Columns:     []string{"Name", "Count", "Load"},
		ColumnHrefs: []string{"", "/sort?by=count"},
		Rows: [][]any{
			{Link("<b>alpha</b>", "/item?id=1&x=2"), Num("12"), Cell{Text: "95%", Meter: Percent(95), Tone: ToneBad}},
			{"beta", 7, Cell{Text: "80%", Meter: Percent(80)}},
			{Cell{Text: "gamma", Mono: true, Title: "tip"}, Num("0"), Cell{Text: "150%", Meter: Percent(150)}},
		},
	})
	for _, want := range []string{
		`<th class="r"><a href="/sort?by=count">Count</a></th>`,
		`<a href="/item?id=1&amp;x=2">&lt;b&gt;alpha&lt;/b&gt;</a>`,
		`<td class="r">12</td>`,
		`<td class="">7</td>`,
		`class="meter bad"><i style="width:95%">`,
		`class="meter warn"><i style="width:80%">`,
		`style="width:100%"`,
		`<i class="dot bad"`,
		`<td class=" mono" title="tip">gamma</td>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table markup is missing %s\n%s", want, out)
		}
	}

	if out := renderWidget(t, Table{Columns: []string{"A"}, Empty: "Nothing here"}); !strings.Contains(out, "Nothing here") || strings.Contains(out, "<table") {
		t.Errorf("empty table = %s", out)
	}
	if out := renderWidget(t, Table{}); !strings.Contains(out, "Nothing recorded yet.") {
		t.Errorf("default empty message missing: %s", out)
	}
}

func TestOtherWidgets(t *testing.T) {
	stats := renderWidget(t, Stats{
		{Label: "Disk", Value: "91%", Hint: "of 100 GB", Tone: ToneWarn, Meter: Percent(91)},
		{Label: "<i>x</i>", Value: "1"},
	})
	for _, want := range []string{`<i class="dot warn"`, `class="meter bad"`, "of 100 GB", "&lt;i&gt;x&lt;/i&gt;"} {
		if !strings.Contains(stats, want) {
			t.Errorf("stats markup is missing %s", want)
		}
	}
	if kv := renderWidget(t, KeyValue{{"Key <1>", "Value & more"}}); !strings.Contains(kv, "Key &lt;1&gt;") || !strings.Contains(kv, "Value &amp; more") {
		t.Errorf("key-value = %s", kv)
	}
	if pre := renderWidget(t, Pre("line 1\n<script>")); !strings.Contains(pre, "line 1\n&lt;script&gt;") {
		t.Errorf("pre = %s", pre)
	}
	if raw := renderWidget(t, HTML("<em>raw</em>")); raw != "<em>raw</em>" {
		t.Errorf("HTML widget = %q, want it passed through untouched", raw)
	}
}
