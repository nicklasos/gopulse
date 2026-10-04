package pulse

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	c := Config{Path: "monitor/"}.withDefaults()
	if c.Path != "/monitor" {
		t.Errorf("path = %q, want it normalised to /monitor", c.Path)
	}
	if c.App == "" || c.Store == nil || c.Instance == "" || len(c.DiskPaths) != 1 {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.SlowRequest != 500*time.Millisecond || c.SlowQuery != 100*time.Millisecond || c.MaxEntries != 200 || c.MaxLogs != 1000 {
		t.Errorf("threshold defaults = %+v", c)
	}
	kept := Config{SlowRequest: time.Second, MaxLogs: 5, Instance: "web-9"}.withDefaults()
	if kept.SlowRequest != time.Second || kept.MaxLogs != 5 || kept.Instance != "web-9" {
		t.Errorf("explicit values were overwritten: %+v", kept)
	}
}

func TestCustomPathIsHonoured(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.Path = "/admin/monitor" })
	if p.Path() != "/admin/monitor" || !p.OwnsPath("/admin/monitor/routes") || p.OwnsPath("/admin/monitoring") {
		t.Fatalf("path handling wrong for %q", p.Path())
	}
	req := httptest.NewRequest("GET", "/admin/monitor/routes", nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `href="/admin/monitor/overview?period=1h"`) {
		t.Fatalf("dashboard under a custom path: status %d", rec.Code)
	}
}

func TestReport(t *testing.T) {
	p := newTestPulse(t, nil)
	ctx, span := p.Start(context.Background(), "POST", "/orders")
	p.Report(ctx, errors.New("webhook delivery failed"))
	p.Report(context.Background(), errors.New("cron job failed"))
	p.Report(ctx, nil)
	p.Flush()

	errs, _ := p.Errors(context.Background(), 10)
	if len(errs) != 2 {
		t.Fatalf("recorded %d errors, want 2 (nil ignored)", len(errs))
	}
	for _, e := range errs {
		switch e.Message {
		case "webhook delivery failed":
			if e.Route != "/orders" || e.Method != "POST" || e.RequestID != span.ID {
				t.Errorf("in-request report not attributed: %+v", e.ErrorSample)
			}
		case "cron job failed":
			if e.Route != "" || e.RequestID != "" {
				t.Errorf("background report wrongly attributed: %+v", e.ErrorSample)
			}
		default:
			t.Errorf("unexpected error %q", e.Message)
		}
	}
}

func TestSpanEndIsIdempotent(t *testing.T) {
	p := newTestPulse(t, nil)
	_, span := p.Start(context.Background(), "GET", "/once")
	span.End(Result{Status: 200})
	span.End(Result{Status: 500})
	p.Flush()
	now := time.Now()
	all, _ := p.Total(context.Background(), MetricHTTP, now.Add(-time.Hour), now)
	failed, _ := p.Total(context.Background(), MetricHTTP5xx, now.Add(-time.Hour), now)
	if all.Count != 1 || failed.Count != 0 {
		t.Fatalf("count = %d, 5xx = %d; want only the first End recorded", all.Count, failed.Count)
	}
}

func TestBackgroundFlushAndCloseWriteData(t *testing.T) {
	store := NewMemoryStore()
	p := New(Config{Store: store, HostInterval: -1, FlushInterval: 20 * time.Millisecond})
	p.Record("m", "ticker", 1)
	now := time.Now()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pts, _ := store.Aggregates(context.Background(), "m", Res1h, Res1h.Floor(now), now.Add(time.Hour))
		if len(pts) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the ticker never flushed the recorded value")
		}
		time.Sleep(5 * time.Millisecond)
	}

	p2 := New(Config{Store: store, HostInterval: -1, FlushInterval: time.Hour})
	p2.Record("m", "close", 1)
	p2.Close()
	p2.Close()
	p.Close()
	pts, _ := store.Aggregates(context.Background(), "m", Res1h, Res1h.Floor(now), now.Add(time.Hour))
	if len(pts) != 2 {
		t.Fatalf("got %d keys, want the value recorded before Close flushed too", len(pts))
	}
	p2.Flush()
}

func TestHostSamplerReportsInstance(t *testing.T) {
	p := newTestPulse(t, func(c *Config) {
		c.HostInterval = 20 * time.Millisecond
		c.Instance = "web-7"
		c.DiskPaths = []string{"/", "/definitely/not/a/mount"}
	})
	var hosts []HostSample
	deadline := time.Now().Add(3 * time.Second)
	for len(hosts) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no host sample arrived")
		}
		time.Sleep(10 * time.Millisecond)
		p.Flush()
		hosts, _ = p.Hosts(context.Background())
	}
	h := hosts[0]
	if h.Instance != "web-7" || h.CPUs == 0 || h.MemTotal == 0 || h.Goroutines == 0 || h.GoVersion == "" {
		t.Errorf("host sample = %+v", h)
	}
	if len(h.Disks) != 1 || h.Disks[0].Path != "/" || h.Disks[0].Total == 0 || h.Disks[0].Percent() <= 0 {
		t.Errorf("disks = %+v, want only the existing mount", h.Disks)
	}
	if h.MemPercent() <= 0 || h.MemPercent() > 100 {
		t.Errorf("memory percent = %v", h.MemPercent())
	}
	if (HostSample{}).MemPercent() != 0 || (DiskUsage{}).Percent() != 0 {
		t.Error("zero totals must not divide by zero")
	}

	now := time.Now()
	mem, _ := p.Totals(context.Background(), MetricHostMem, now.Add(-time.Hour), now)
	if mem["web-7"].Count == 0 {
		t.Error("host gauges were not recorded as a time series")
	}

	req := httptest.NewRequest("GET", "/_pulse/server", nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "web-7") || strings.Contains(rec.Body.String(), "Could not load") {
		t.Error("server page does not show the sampled instance")
	}
}

func TestMemoryStoreHostsExpire(t *testing.T) {
	s := NewMemoryStore()
	clock := time.Now()
	s.now = func() time.Time { return clock }
	ctx := context.Background()
	_ = s.PutHost(ctx, "a", []byte("1"), time.Minute)
	_ = s.PutHost(ctx, "b", []byte("2"), time.Hour)
	clock = clock.Add(2 * time.Minute)
	hosts, _ := s.Hosts(ctx)
	if len(hosts) != 1 || string(hosts["b"]) != "2" {
		t.Fatalf("hosts = %v, want only b", hosts)
	}
}

func TestSeriesDownsamplesAndMergesHistograms(t *testing.T) {
	p := newTestPulse(t, nil)
	ctx := context.Background()
	now := time.Now()
	hist := func(i int) []int64 {
		h := make([]int64, len(HistBounds)+1)
		h[i] = 1
		return h
	}
	var deltas []AggDelta
	for i := range 240 {
		at := Res1m.Floor(now.Add(-time.Duration(i) * time.Minute))
		deltas = append(deltas, AggDelta{
			AggKey: AggKey{Metric: "m", Key: "k", Res: Res1m.Name, Start: at.Unix()},
			Agg:    Agg{Count: 1, Sum: 2, Hist: hist(i % 3)},
		})
	}
	_ = p.Store().AddAggregates(ctx, deltas)

	s, err := p.Series(ctx, "m", "k", now.Add(-239*time.Minute), now, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Points) > 60 || s.Step != 4*time.Minute {
		t.Fatalf("%d points of %s, want at most 60 points of 4m", len(s.Points), s.Step)
	}
	var total Agg
	for _, pt := range s.Points {
		total.Merge(pt.Agg)
	}
	if total.Count != 240 || total.Sum != 480 || total.Hist[0]+total.Hist[1]+total.Hist[2] != 240 {
		t.Fatalf("merged total = %+v, want nothing lost in downsampling", total)
	}
	other, _ := p.Series(ctx, "m", "missing", now.Add(-time.Hour), now, 60)
	for _, pt := range other.Points {
		if pt.Count != 0 {
			t.Fatal("series for another key must be empty")
		}
	}
}

func TestPageRegistry(t *testing.T) {
	p := newTestPulse(t, nil)
	if got := slugify("  Business KPIs & More! "); got != "business-kpis-more" {
		t.Errorf("slug = %q", got)
	}
	first := p.AddPage("My Page", Card{Title: "old"})
	p.AddPage("My Page", Card{Title: "new"})
	if got := p.page(first.Slug); got == nil || got.Cards[0].Title != "new" {
		t.Error("adding a page with the same slug should replace it")
	}
	count := 0
	for _, pg := range p.pages {
		if pg.Slug == "my-page" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("page registered %d times", count)
	}
	if p.page("") == nil || p.page("").Slug != "overview" {
		t.Error("the empty slug should resolve to the first page")
	}
}

func TestViewPageURL(t *testing.T) {
	v := View{Period: "24h", Params: url.Values{"route": {"GET /a"}}, base: "/_pulse"}
	if got := v.PageURL("route", "route", "GET /users/:id", "dangling"); got != "/_pulse/route?period=24h&route=GET+%2Fusers%2F%3Aid" {
		t.Errorf("PageURL = %q", got)
	}
	if got := (View{base: "/x"}).PageURL("logs"); got != "/x/logs" {
		t.Errorf("PageURL without period = %q", got)
	}
	if v.Param("route") != "GET /a" || v.Param("nope") != "" {
		t.Error("Param lookup wrong")
	}
}

func TestPeriodSelection(t *testing.T) {
	p := newTestPulse(t, nil)
	var seen View
	p.AddPage("Probe", Card{Render: func(_ context.Context, v View) (Widget, error) {
		seen = v
		return nil, nil
	}})
	for query, want := range map[string]time.Duration{"": time.Hour, "?period=24h": 24 * time.Hour, "?period=bogus": time.Hour, "?period=7d": 7 * 24 * time.Hour} {
		req := httptest.NewRequest("GET", "/_pulse/probe"+query, nil)
		req.SetBasicAuth("admin", "secret")
		p.Handler().ServeHTTP(httptest.NewRecorder(), req)
		if got := seen.To.Sub(seen.From); got != want {
			t.Errorf("%q: window = %s, want %s", query, got, want)
		}
	}
}

func TestAssetsAndMethods(t *testing.T) {
	p := newTestPulse(t, nil)
	do := func(method, path string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if auth {
			req.SetBasicAuth("admin", "secret")
		}
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		return rec
	}
	js := do("GET", "/_pulse/_assets/app.js", true)
	if js.Code != http.StatusOK || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") || js.Body.Len() == 0 {
		t.Errorf("script = %d %q", js.Code, js.Header().Get("Content-Type"))
	}
	if do("GET", "/_pulse/_assets/app.css", false).Code != http.StatusUnauthorized {
		t.Error("assets must sit behind the login too")
	}
	if do("POST", "/_pulse/overview", true).Code != http.StatusMethodNotAllowed {
		t.Error("the dashboard should be read-only")
	}
	page := do("GET", "/_pulse/overview", true)
	if page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("page headers = %v", page.Header())
	}
}

func TestHTTPMiddlewarePanicAndDefaults(t *testing.T) {
	p := newTestPulse(t, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom/{id}", func(http.ResponseWriter, *http.Request) { panic("net/http boom") })
	mux.HandleFunc("GET /silent", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /body", func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		_, _ = w.Write([]byte("ok"))
		if err := rc.Flush(); err != nil {
			t.Errorf("wrapped writer lost Flush: %v", err)
		}
	})
	h := p.Middleware(mux)

	func() {
		defer func() {
			if recover() != "net/http boom" {
				t.Error("the panic should be re-raised for the server to handle")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/boom/1", nil))
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/silent", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/body", nil))
	p.Flush()

	ctx := context.Background()
	now := time.Now()
	totals, _ := p.Totals(ctx, MetricHTTP, now.Add(-time.Hour), now)
	failed, _ := p.Totals(ctx, MetricHTTP5xx, now.Add(-time.Hour), now)
	if len(totals) != 3 || failed["GET /boom/{id}"].Count != 1 || len(failed) != 1 {
		t.Fatalf("totals = %v, 5xx = %v", totals, failed)
	}
	errs, _ := p.Errors(ctx, 10)
	if len(errs) != 1 || errs[0].Kind != "panic" || errs[0].Route != "/boom/{id}" || !strings.Contains(errs[0].Stack, "core_test.go") {
		t.Fatalf("errors = %+v", errs)
	}
}

func TestErrorPagesHandleMissingAndStackless(t *testing.T) {
	p := newTestPulse(t, nil)
	_, span := p.Start(context.Background(), "GET", "/x")
	span.End(Result{Status: 502})
	p.Flush()
	errs, _ := p.Errors(context.Background(), 1)

	get := func(path string) string {
		req := httptest.NewRequest("GET", path, nil)
		req.SetBasicAuth("admin", "secret")
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if body := get("/_pulse/error?fp=" + errs[0].Fingerprint); !strings.Contains(body, "Stacks are captured for panics only") || !strings.Contains(body, "HTTP 502") {
		t.Error("error detail without a stack should explain why")
	}
	if body := get("/_pulse/error?fp=unknown"); !strings.Contains(body, "error not found") {
		t.Error("unknown fingerprint should be reported in place")
	}
	if body := get("/_pulse/route"); !strings.Contains(body, "no route selected") {
		t.Error("route page without a route should say so")
	}
}

func TestFingerprintStability(t *testing.T) {
	a := ErrorSample{Kind: "error", Method: "GET", Route: "/o/:id", Message: "order 12 failed for user 0a1b2c3d4e5f"}
	b := a
	b.Message = "order 98765 failed for user ffeeddccbbaa"
	if fingerprint(a) != fingerprint(b) {
		t.Error("ids inside a message should not split the group")
	}
	c := a
	c.Route = "/other"
	d := a
	d.Kind = "panic"
	if fingerprint(a) == fingerprint(c) || fingerprint(a) == fingerprint(d) {
		t.Error("route and kind should separate groups")
	}
}
