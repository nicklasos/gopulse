package pulse

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

func (p *Pulse) registerBuiltinPages() {
	p.addPage(&Page{Title: "Overview", Slug: "overview", Cards: []Card{
		{Render: p.cardTrafficStats("")},
		{Title: "Requests per minute", Width: Half, Render: p.cardRequestRate("")},
		{Title: "Response time", Width: Half, Render: p.cardResponseTime("")},
		{Title: "Slowest routes", Width: Half, Render: p.cardRoutes(5, false)},
		{Title: "Recent errors", Width: Half, Render: p.cardErrors(5)},
		{Title: "Servers", Render: p.cardInstances},
	}})
	p.addPage(&Page{Title: "Routes", Slug: "routes", Cards: []Card{
		{Title: "Routes", Render: p.cardRoutes(0, true)},
		{Title: "Slow requests", Render: p.cardSlowRequests("")},
	}})
	p.addPage(&Page{Title: "Route", Slug: "route", Hidden: true, Parent: "routes", Cards: []Card{
		{Render: p.routeCard(p.cardTrafficStats)},
		{Title: "Requests per minute", Width: Half, Render: p.routeCard(p.cardRequestRate)},
		{Title: "Response time", Width: Half, Render: p.routeCard(p.cardResponseTime)},
		{Title: "Slow requests", Render: p.routeCard(p.cardSlowRequests)},
	}})
	p.addPage(&Page{Title: "Errors", Slug: "errors", Cards: []Card{
		{Title: "Errors", Render: p.cardErrors(0)},
	}})
	p.addPage(&Page{Title: "Error", Slug: "error", Hidden: true, Parent: "errors", Cards: []Card{
		{Render: p.cardErrorDetail},
		{Title: "Stack trace", Render: p.cardErrorStack},
	}})
	p.addPage(&Page{Title: "Server", Slug: "server", Cards: []Card{
		{Title: "Instances", Render: p.cardInstances},
		{Title: "Disks", Render: p.cardDisks},
		{Title: "CPU", Width: Half, Render: p.cardHostSeries(MetricHostCPU, "%", 100)},
		{Title: "Memory", Width: Half, Render: p.cardHostSeries(MetricHostMem, "%", 100)},
		{Title: "Load average (1m)", Width: Half, Render: p.cardHostSeries(MetricHostLoad, "", 0)},
		{Title: "Goroutines", Width: Half, Render: p.cardHostSeries(MetricHostGoroutines, "", 0)},
		{Title: "Recorder", Render: p.cardRecorder},
	}})
}

type cardFunc = func(ctx context.Context, v View) (Widget, error)

// routeCard adapts a card that takes a route key to read it from the URL.
func (p *Pulse) routeCard(build func(route string) cardFunc) cardFunc {
	return func(ctx context.Context, v View) (Widget, error) {
		route := v.Param("route")
		if route == "" {
			return nil, fmt.Errorf("no route selected")
		}
		return build(route)(ctx, v)
	}
}

func errorRate(errors, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(errors) / float64(total) * 100
}

func pick(totals map[string]Agg, key string) Agg {
	if key != "" {
		return totals[key]
	}
	var sum Agg
	for _, a := range totals {
		sum.Merge(a)
	}
	return sum
}

func (p *Pulse) cardTrafficStats(route string) cardFunc {
	return func(ctx context.Context, v View) (Widget, error) {
		totals, err := p.Totals(ctx, MetricHTTP, v.From, v.To)
		if err != nil {
			return nil, err
		}
		t4, err := p.Totals(ctx, MetricHTTP4xx, v.From, v.To)
		if err != nil {
			return nil, err
		}
		t5, err := p.Totals(ctx, MetricHTTP5xx, v.From, v.To)
		if err != nil {
			return nil, err
		}
		recent, err := p.Totals(ctx, MetricHTTP, v.To.Add(-time.Minute), v.To)
		if err != nil {
			return nil, err
		}
		all, c4, c5 := pick(totals, route), pick(t4, route).Count, pick(t5, route).Count
		last := pick(recent, route)

		rate := errorRate(c5, all.Count)
		tone := ToneGood
		switch {
		case all.Count == 0:
			tone = ToneNone
		case rate >= 5:
			tone = ToneBad
		case rate >= 1:
			tone = ToneWarn
		}
		stats := Stats{}
		if route != "" {
			stats = append(stats, Stat{Label: "Route", Value: route})
		}
		return append(stats,
			Stat{Label: "Requests", Value: FormatCount(float64(all.Count)), Hint: "last " + v.Period},
			Stat{Label: "Throughput", Value: FormatCount(float64(last.Count)/60) + " /s", Hint: "last minute"},
			Stat{Label: "Average", Value: FormatMS(all.Avg())},
			Stat{Label: "P95", Value: FormatMS(all.Percentile(0.95))},
			Stat{Label: "P99", Value: FormatMS(all.Percentile(0.99))},
			Stat{Label: "Server errors", Value: FormatValue(rate, "%"), Tone: tone, Hint: FormatCount(float64(c5)) + " of 5xx"},
			Stat{Label: "Client errors", Value: FormatCount(float64(c4)), Hint: "4xx responses"},
		), nil
	}
}

func (p *Pulse) cardRequestRate(route string) cardFunc {
	return func(ctx context.Context, v View) (Widget, error) {
		s, err := p.Series(ctx, MetricHTTP, route, v.From, v.To, 60)
		if err != nil {
			return nil, err
		}
		line := Line{Name: "Requests"}
		for _, pt := range completed(s.Points) {
			line.Points = append(line.Points, Point{T: pt.Time, V: float64(pt.Count) / s.Step.Minutes()})
		}
		return TimeSeries{Lines: []Line{line}, Unit: "/min", Bars: true}, nil
	}
}

func (p *Pulse) cardResponseTime(route string) cardFunc {
	return func(ctx context.Context, v View) (Widget, error) {
		s, err := p.Series(ctx, MetricHTTP, route, v.From, v.To, 120)
		if err != nil {
			return nil, err
		}
		avg, p95 := Line{Name: "Average"}, Line{Name: "P95"}
		for _, pt := range completed(s.Points) {
			avg.Points = append(avg.Points, Point{T: pt.Time, V: gauge(pt.Agg, pt.Avg())})
			p95.Points = append(p95.Points, Point{T: pt.Time, V: gauge(pt.Agg, pt.Percentile(0.95))})
		}
		return TimeSeries{Lines: []Line{avg, p95}, Unit: "ms"}, nil
	}
}

// completed drops the last point, whose interval is still filling up and
// would otherwise draw as a misleading dip.
func completed(pts []SeriesPoint) []SeriesPoint {
	if len(pts) > 2 {
		return pts[:len(pts)-1]
	}
	return pts
}

// gauge returns v, or a gap when nothing was measured in the interval.
func gauge(a Agg, v float64) float64 {
	if a.Count == 0 {
		return math.NaN()
	}
	return v
}

type routeRow struct {
	key    string
	agg    Agg
	p95    float64
	c4, c5 int64
}

func (p *Pulse) cardRoutes(limit int, sortable bool) cardFunc {
	return func(ctx context.Context, v View) (Widget, error) {
		totals, err := p.Totals(ctx, MetricHTTP, v.From, v.To)
		if err != nil {
			return nil, err
		}
		t4, err := p.Totals(ctx, MetricHTTP4xx, v.From, v.To)
		if err != nil {
			return nil, err
		}
		t5, err := p.Totals(ctx, MetricHTTP5xx, v.From, v.To)
		if err != nil {
			return nil, err
		}
		rows := make([]routeRow, 0, len(totals))
		for key, a := range totals {
			rows = append(rows, routeRow{key: key, agg: a, p95: a.Percentile(0.95), c4: t4[key].Count, c5: t5[key].Count})
		}

		order := "p95"
		if sortable {
			order = v.Param("sort")
		}
		less := map[string]func(a, b routeRow) bool{
			"requests": func(a, b routeRow) bool { return a.agg.Count > b.agg.Count },
			"avg":      func(a, b routeRow) bool { return a.agg.Avg() > b.agg.Avg() },
			"p95":      func(a, b routeRow) bool { return a.p95 > b.p95 },
			"errors":   func(a, b routeRow) bool { return a.c5 > b.c5 },
			"time":     func(a, b routeRow) bool { return a.agg.Sum > b.agg.Sum },
		}[order]
		if less == nil {
			less = func(a, b routeRow) bool { return a.agg.Sum > b.agg.Sum }
		}
		sort.Slice(rows, func(i, j int) bool {
			if less(rows[i], rows[j]) != less(rows[j], rows[i]) {
				return less(rows[i], rows[j])
			}
			return rows[i].key < rows[j].key
		})
		if limit > 0 && len(rows) > limit {
			rows = rows[:limit]
		}

		minutes := v.To.Sub(v.From).Minutes()
		table := Table{Empty: "No requests recorded in this period."}
		if !sortable {
			table.Columns = []string{"Route", "Requests", "Average", "P95"}
			for _, r := range rows {
				table.Rows = append(table.Rows, []any{
					Cell{Text: r.key, Href: v.PageURL("route", "route", r.key), Mono: true},
					Num(FormatCount(float64(r.agg.Count))),
					Num(FormatMS(r.agg.Avg())),
					Num(FormatMS(r.p95)),
				})
			}
			return table, nil
		}
		table.Columns = []string{"Route", "Requests", "Per minute", "Average", "P95", "P99", "Total time", "4xx", "5xx"}
		link := func(key string) string { return v.PageURL("routes", "sort", key) }
		table.ColumnHrefs = []string{"", link("requests"), link("requests"), link("avg"), link("p95"), "", link("time"), "", link("errors")}
		for _, r := range rows {
			errCell := Num(FormatCount(float64(r.c5)))
			if r.c5 > 0 {
				errCell.Tone = ToneBad
			}
			table.Rows = append(table.Rows, []any{
				Cell{Text: r.key, Href: v.PageURL("route", "route", r.key), Mono: true},
				Num(FormatCount(float64(r.agg.Count))),
				Num(FormatCount(float64(r.agg.Count) / minutes)),
				Num(FormatMS(r.agg.Avg())),
				Num(FormatMS(r.p95)),
				Num(FormatMS(r.agg.Percentile(0.99))),
				Num(FormatMS(r.agg.Sum)),
				Num(FormatCount(float64(r.c4))),
				errCell,
			})
		}
		return table, nil
	}
}

func statusCell(status int) Cell {
	c := Num(strconv.Itoa(status))
	switch {
	case status >= 500:
		c.Tone = ToneBad
	case status >= 400:
		c.Tone = ToneWarn
	}
	return c
}

func timeCell(t time.Time) Cell {
	return Cell{Text: FormatAgo(t), Title: t.Format(time.RFC3339)}
}

func (p *Pulse) cardSlowRequests(route string) cardFunc {
	return func(ctx context.Context, v View) (Widget, error) {
		reqs, err := p.SlowRequests(ctx, p.cfg.MaxEntries)
		if err != nil {
			return nil, err
		}
		table := Table{
			Columns: []string{"When", "Route", "Path", "Status", "Duration"},
			Empty:   fmt.Sprintf("No requests slower than %s.", FormatMS(float64(p.cfg.SlowRequest.Milliseconds()))),
		}
		for _, r := range reqs {
			key := routeKey(r.Method, r.Route)
			if route != "" && key != route {
				continue
			}
			if len(table.Rows) == 50 {
				break
			}
			table.Rows = append(table.Rows, []any{
				timeCell(r.Time),
				Cell{Text: key, Href: v.PageURL("route", "route", key), Mono: true},
				Cell{Text: r.Path, Mono: true},
				statusCell(r.Status),
				Num(FormatMS(r.DurationMS)),
			})
		}
		return table, nil
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (p *Pulse) cardErrors(limit int) cardFunc {
	return func(ctx context.Context, v View) (Widget, error) {
		n := limit
		if n == 0 {
			n = p.cfg.MaxEntries
		}
		errs, err := p.Errors(ctx, n)
		if err != nil {
			return nil, err
		}
		table := Table{
			Columns: []string{"Last seen", "Kind", "Message", "Route", "Count"},
			Empty:   "No errors recorded.",
		}
		for _, e := range errs {
			kind := Cell{Text: e.Kind}
			if e.Kind == "panic" {
				kind.Tone = ToneBad
			}
			table.Rows = append(table.Rows, []any{
				timeCell(e.LastSeen),
				kind,
				Link(truncate(e.Message, 140), v.PageURL("error", "fp", e.Fingerprint)),
				Cell{Text: routeKey(e.Method, e.Route), Mono: true},
				Num(FormatCount(float64(e.Count))),
			})
		}
		return table, nil
	}
}

func (p *Pulse) errorFromView(ctx context.Context, v View) (ErrorInfo, error) {
	g, ok, err := p.store.Error(ctx, v.Param("fp"))
	if err != nil {
		return ErrorInfo{}, err
	}
	if !ok {
		return ErrorInfo{}, fmt.Errorf("error not found; it may have been rotated out")
	}
	return decodeError(g), nil
}

func (p *Pulse) cardErrorDetail(ctx context.Context, v View) (Widget, error) {
	e, err := p.errorFromView(ctx, v)
	if err != nil {
		return nil, err
	}
	kv := KeyValue{
		{"Message", e.Message},
		{"Kind", e.Kind},
		{"Occurrences", FormatCount(float64(e.Count))},
		{"First seen", e.FirstSeen.Format(time.RFC1123)},
		{"Last seen", e.LastSeen.Format(time.RFC1123)},
	}
	if e.Route != "" {
		kv = append(kv, KV{"Route", routeKey(e.Method, e.Route)})
	}
	if e.Path != "" {
		kv = append(kv, KV{"Last path", e.Path})
	}
	if e.Status != 0 {
		kv = append(kv, KV{"Status", strconv.Itoa(e.Status)})
	}
	if e.RequestID != "" {
		kv = append(kv, KV{"Last request ID", e.RequestID})
	}
	return kv, nil
}

func (p *Pulse) cardErrorStack(ctx context.Context, v View) (Widget, error) {
	e, err := p.errorFromView(ctx, v)
	if err != nil {
		return nil, err
	}
	if e.Stack == "" {
		return Pre("No stack trace. Stacks are captured for panics only."), nil
	}
	return Pre(e.Stack), nil
}

func (p *Pulse) sortedHosts(ctx context.Context) ([]HostSample, error) {
	hosts, err := p.Hosts(ctx)
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Instance < hosts[j].Instance })
	return hosts, err
}

func formatUptime(d time.Duration) string {
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func (p *Pulse) cardInstances(ctx context.Context, _ View) (Widget, error) {
	hosts, err := p.sortedHosts(ctx)
	if err != nil {
		return nil, err
	}
	table := Table{
		Columns: []string{"Instance", "CPU", "Memory", "Load 1m / 5m / 15m", "Goroutines", "Heap", "Uptime", "Go"},
		Empty:   "No instance has reported yet. The first sample arrives within a few seconds.",
	}
	for _, h := range hosts {
		table.Rows = append(table.Rows, []any{
			Cell{Text: h.Instance, Title: "PID " + strconv.Itoa(h.PID)},
			Cell{Text: FormatValue(h.CPUPercent, "%") + " of " + strconv.Itoa(h.CPUs) + " cores", Meter: Percent(h.CPUPercent)},
			Cell{Text: FormatBytes(h.MemUsed) + " of " + FormatBytes(h.MemTotal), Meter: Percent(h.MemPercent())},
			fmt.Sprintf("%.2f / %.2f / %.2f", h.Load1, h.Load5, h.Load15),
			Num(FormatCount(float64(h.Goroutines))),
			Num(FormatBytes(h.HeapAlloc)),
			Num(formatUptime(h.Time.Sub(h.StartedAt))),
			h.GoVersion,
		})
	}
	return table, nil
}

func (p *Pulse) cardDisks(ctx context.Context, _ View) (Widget, error) {
	hosts, err := p.sortedHosts(ctx)
	if err != nil {
		return nil, err
	}
	table := Table{Columns: []string{"Instance", "Mount", "Used", "Free", "Total"}, Empty: "No disk information yet."}
	for _, h := range hosts {
		for _, d := range h.Disks {
			table.Rows = append(table.Rows, []any{
				h.Instance,
				Cell{Text: d.Path, Mono: true},
				Cell{Text: FormatValue(d.Percent(), "%") + " · " + FormatBytes(d.Used), Meter: Percent(d.Percent())},
				Num(FormatBytes(d.Total - d.Used)),
				Num(FormatBytes(d.Total)),
			})
		}
	}
	return table, nil
}

func (p *Pulse) cardHostSeries(metric, unit string, top float64) cardFunc {
	return func(ctx context.Context, v View) (Widget, error) {
		totals, err := p.Totals(ctx, metric, v.From, v.To)
		if err != nil {
			return nil, err
		}
		instances := make([]string, 0, len(totals))
		for k := range totals {
			instances = append(instances, k)
		}
		sort.Strings(instances)
		ts := TimeSeries{Unit: unit, Max: top}
		for _, inst := range instances {
			s, err := p.Series(ctx, metric, inst, v.From, v.To, 120)
			if err != nil {
				return nil, err
			}
			line := Line{Name: inst}
			for _, pt := range completed(s.Points) {
				line.Points = append(line.Points, Point{T: pt.Time, V: gauge(pt.Agg, pt.Avg())})
			}
			ts.Lines = append(ts.Lines, line)
		}
		return ts, nil
	}
}

func (p *Pulse) cardRecorder(context.Context, View) (Widget, error) {
	status := "OK"
	if e := p.StoreError(); e != "" {
		status = "Failing: " + e
	}
	return KeyValue{
		{"Store", status},
		{"Dropped events (this instance)", FormatCount(float64(p.Dropped()))},
		{"Flush interval", p.cfg.FlushInterval.String()},
		{"Slow request threshold", p.cfg.SlowRequest.String()},
		{"Slow query threshold", p.cfg.SlowQuery.String()},
		{"Entries kept per list", strconv.Itoa(p.cfg.MaxEntries)},
	}, nil
}
