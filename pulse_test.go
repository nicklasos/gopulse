package pulse

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestPulse(t *testing.T, mutate func(*Config)) *Pulse {
	t.Helper()
	cfg := Config{
		App:           "test",
		Username:      "admin",
		Password:      "secret",
		HostInterval:  -1,
		FlushInterval: time.Hour,
		SlowRequest:   time.Hour,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p := New(cfg)
	t.Cleanup(p.Close)
	return p
}

func TestPercentileInterpolates(t *testing.T) {
	a := Agg{Hist: make([]int64, len(HistBounds)+1)}
	for range 90 {
		a.Hist[histIndex(8)]++
	}
	for range 10 {
		a.Hist[histIndex(400)]++
	}
	if got := a.Percentile(0.5); got < 5 || got > 10 {
		t.Fatalf("p50 = %v, want within the 5-10ms bucket", got)
	}
	if got := a.Percentile(0.95); got < 250 || got > 500 {
		t.Fatalf("p95 = %v, want within the 250-500ms bucket", got)
	}
	if got := (Agg{}).Percentile(0.95); got != 0 {
		t.Fatalf("empty p95 = %v, want 0", got)
	}
}

func TestPercentileOverflowBucket(t *testing.T) {
	a := Agg{Hist: make([]int64, len(HistBounds)+1)}
	a.Hist[histIndex(60000)] = 3
	if got, want := a.Percentile(0.99), HistBounds[len(HistBounds)-1]; got != want {
		t.Fatalf("overflow p99 = %v, want %v", got, want)
	}
}

func TestResolutionFloorAndChoice(t *testing.T) {
	ts := time.Unix(1_700_000_047, 0)
	if got := Res10s.Floor(ts).Unix(); got != 1_700_000_040 {
		t.Fatalf("10s floor = %d", got)
	}
	if got := Res1h.Floor(ts).Unix() % 3600; got != 0 {
		t.Fatalf("1h floor not aligned: %d", got)
	}
	now := time.Now()
	for span, want := range map[time.Duration]string{time.Hour: "10s", 6 * time.Hour: "1m", 7 * 24 * time.Hour: "1h"} {
		if got := ResolutionFor(now.Add(-span), now).Name; got != want {
			t.Errorf("ResolutionFor(%s) = %s, want %s", span, got, want)
		}
	}
}

func TestMemoryStoreCapsAndOrders(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	base := time.Now()
	for i := range 5 {
		_ = s.AppendEntries(ctx, "l", []Entry{{Time: base.Add(time.Duration(i) * time.Second), Data: []byte{byte('0' + i)}}}, 3)
	}
	got, _ := s.Entries(ctx, "l", 10)
	if len(got) != 3 || string(got[0].Data) != "4" || string(got[2].Data) != "2" {
		t.Fatalf("entries = %v, want newest three, newest first", got)
	}

	for i := range 4 {
		_ = s.RecordErrors(ctx, []ErrorOccurrence{{Fingerprint: string(rune('a' + i)), Time: base.Add(time.Duration(i) * time.Second)}}, 2)
	}
	groups, _ := s.Errors(ctx, 10)
	if len(groups) != 2 || groups[0].Fingerprint != "d" || groups[1].Fingerprint != "c" {
		t.Fatalf("errors = %+v, want the two most recent", groups)
	}
}

func TestMemoryStorePrunesExpiredBuckets(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	old := time.Now().Add(-2 * time.Hour)
	_ = s.AddAggregates(ctx, []AggDelta{{AggKey: AggKey{Metric: "m", Key: "k", Res: "10s", Start: Res10s.Floor(old).Unix()}, Agg: Agg{Count: 1}}})
	_ = s.AddAggregates(ctx, []AggDelta{{AggKey: AggKey{Metric: "m", Key: "k", Res: "10s", Start: Res10s.Floor(time.Now()).Unix()}, Agg: Agg{Count: 1}}})
	pts, _ := s.Aggregates(ctx, "m", Res10s, old.Add(-time.Hour), time.Now())
	if len(pts) != 1 {
		t.Fatalf("got %d points, want only the fresh bucket", len(pts))
	}
}

func TestRequestsAggregateAcrossResolutions(t *testing.T) {
	p := newTestPulse(t, nil)
	ctx := context.Background()
	for _, status := range []int{200, 200, 404, 500} {
		_, span := p.Start(ctx, "GET", "/users/:id")
		span.End(Result{Path: "/users/1", Status: status})
	}
	p.Flush()

	now := time.Now()
	for _, span := range []time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour} {
		totals, err := p.Totals(ctx, MetricHTTP, now.Add(-span), now)
		if err != nil {
			t.Fatal(err)
		}
		if got := totals["GET /users/:id"].Count; got != 4 {
			t.Errorf("window %s: count = %d, want 4", span, got)
		}
	}
	t4, _ := p.Total(ctx, MetricHTTP4xx, now.Add(-time.Hour), now)
	t5, _ := p.Total(ctx, MetricHTTP5xx, now.Add(-time.Hour), now)
	if t4.Count != 1 || t5.Count != 1 {
		t.Fatalf("4xx = %d, 5xx = %d, want 1 and 1", t4.Count, t5.Count)
	}
}

func TestSeriesIsGapFreeAndBounded(t *testing.T) {
	p := newTestPulse(t, nil)
	ctx := context.Background()
	_, span := p.Start(ctx, "GET", "/a")
	span.End(Result{Status: 200})
	p.Flush()

	now := time.Now()
	s, err := p.Series(ctx, MetricHTTP, "", now.Add(-time.Hour), now, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Points) == 0 || len(s.Points) > 61 {
		t.Fatalf("got %d points, want 1..61", len(s.Points))
	}
	var total int64
	for i, pt := range s.Points {
		total += pt.Count
		if i > 0 && pt.Time.Sub(s.Points[i-1].Time) != s.Step {
			t.Fatalf("points %d and %d are not one step apart", i-1, i)
		}
	}
	if total != 1 {
		t.Fatalf("series total = %d, want 1", total)
	}
}

func TestErrorsGroupByFingerprint(t *testing.T) {
	p := newTestPulse(t, nil)
	ctx := context.Background()
	for _, id := range []string{"17", "9042"} {
		_, span := p.Start(ctx, "GET", "/orders/:id")
		span.End(Result{Status: 500, Errors: []error{errors.New("order " + id + " not loaded")}})
	}
	_, span := p.Start(ctx, "GET", "/other")
	span.End(Result{Status: 503})
	p.Flush()

	errs, err := p.Errors(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(errs), errs)
	}
	byKind := map[string]ErrorInfo{}
	for _, e := range errs {
		byKind[e.Kind] = e
	}
	if byKind["error"].Count != 2 {
		t.Errorf("messages differing only by numbers should share a group, count = %d", byKind["error"].Count)
	}
	if byKind["http"].Message != "HTTP 503" {
		t.Errorf("status-only failure message = %q", byKind["http"].Message)
	}
}

func TestPanicIsRecordedOnceWithStack(t *testing.T) {
	p := newTestPulse(t, nil)
	_, span := p.Start(context.Background(), "GET", "/boom")
	span.Panic("kaboom", []byte("stack line"))
	span.End(Result{Status: 500})
	p.Flush()

	errs, _ := p.Errors(context.Background(), 10)
	if len(errs) != 1 || errs[0].Kind != "panic" || errs[0].Stack != "stack line" || errs[0].Message != "kaboom" {
		t.Fatalf("errors = %+v, want a single panic with its stack", errs)
	}
}

func TestSlowRequestsKeptAboveThreshold(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.SlowRequest = 20 * time.Millisecond })
	ctx := context.Background()
	_, fast := p.Start(ctx, "GET", "/fast")
	fast.End(Result{Status: 200})
	_, slow := p.Start(ctx, "GET", "/slow")
	time.Sleep(30 * time.Millisecond)
	slow.End(Result{Path: "/slow", Status: 200})
	p.Flush()

	reqs, _ := p.SlowRequests(ctx, 10)
	if len(reqs) != 1 || reqs[0].Route != "/slow" || reqs[0].DurationMS < 20 {
		t.Fatalf("slow requests = %+v", reqs)
	}
}

func TestRecordCustomMetric(t *testing.T) {
	p := newTestPulse(t, nil)
	p.Record("orders", "created", 10)
	p.Record("orders", "created", 30)
	p.Flush()
	now := time.Now()
	totals, _ := p.Totals(context.Background(), "orders", now.Add(-time.Hour), now)
	if a := totals["created"]; a.Count != 2 || math.Abs(a.Avg()-20) > 1e-9 {
		t.Fatalf("custom metric = %+v, want count 2 avg 20", a)
	}
}

func TestEventsDropWhenQueueFull(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.BufferSize = 1 })
	p.Close()
	for range 5 {
		p.Record("m", "k", 1)
	}
	if p.Dropped() != 4 {
		t.Fatalf("dropped = %d, want 4", p.Dropped())
	}
}

type failingStore struct{ *MemoryStore }

func (failingStore) AddAggregates(context.Context, []AggDelta) error {
	return errors.New("redis down")
}

func TestStoreFailureIsReported(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.Store = failingStore{NewMemoryStore()} })
	p.Record("m", "k", 1)
	p.Flush()
	if p.StoreError() != "redis down" {
		t.Fatalf("store error = %q", p.StoreError())
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/_pulse", nil)
	req.SetBasicAuth("admin", "secret")
	p.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Store unavailable") {
		t.Fatal("dashboard does not show the store failure")
	}
}

func TestDashboardAuth(t *testing.T) {
	p := newTestPulse(t, nil)
	cases := []struct {
		name       string
		user, pass string
		want       int
	}{
		{"no credentials", "", "", http.StatusUnauthorized},
		{"wrong password", "admin", "nope", http.StatusUnauthorized},
		{"wrong user", "root", "secret", http.StatusUnauthorized},
		{"correct", "admin", "secret", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/_pulse", nil)
			if tc.user != "" {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			p.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestDashboardDisabledWithoutPassword(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.Password = "" })
	req := httptest.NewRequest("GET", "/_pulse", nil)
	req.SetBasicAuth("admin", "")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAuthorizeHookReplacesBasicAuth(t *testing.T) {
	allow := false
	p := newTestPulse(t, func(c *Config) { c.Authorize = func(*http.Request) bool { return allow } })
	get := func() int {
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/_pulse", nil))
		return rec.Code
	}
	if got := get(); got != http.StatusForbidden {
		t.Fatalf("denied status = %d, want 403", got)
	}
	allow = true
	if got := get(); got != http.StatusOK {
		t.Fatalf("allowed status = %d, want 200", got)
	}
}

func TestEveryBuiltinPageRenders(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.SlowRequest = time.Nanosecond })
	ctx := context.Background()
	_, span := p.Start(ctx, "GET", "/users/:id")
	span.Panic("<script>alert(1)</script>", []byte("goroutine 1"))
	span.End(Result{Path: "/users/1", Status: 500})
	p.emit(hostEvent{p.readHost()})
	p.Flush()
	errs, _ := p.Errors(ctx, 1)

	paths := []string{
		"/_pulse", "/_pulse/", "/_pulse/overview?period=7d", "/_pulse/routes?sort=avg",
		"/_pulse/route?route=GET+%2Fusers%2F%3Aid", "/_pulse/errors",
		"/_pulse/error?fp=" + errs[0].Fingerprint, "/_pulse/server", "/_pulse/overview?_fragment=1",
	}
	for _, path := range paths {
		req := httptest.NewRequest("GET", path, nil)
		req.SetBasicAuth("admin", "secret")
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
		}
		if strings.Contains(body, "Could not load") {
			t.Errorf("%s: a card failed to render", path)
		}
		if strings.Contains(body, "<script>alert(1)") {
			t.Errorf("%s: recorded text was not escaped", path)
		}
	}

	req := httptest.NewRequest("GET", "/_pulse/missing", nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown page status = %d, want 404", rec.Code)
	}
}

func TestCustomPageAndFailingCard(t *testing.T) {
	p := newTestPulse(t, nil)
	p.AddPage("Business KPIs",
		Card{Title: "Numbers", Render: func(context.Context, View) (Widget, error) {
			return Stats{{Label: "Orders", Value: "42"}}, nil
		}},
		Card{Title: "Broken", Render: func(context.Context, View) (Widget, error) { panic("nope") }},
	)
	req := httptest.NewRequest("GET", "/_pulse/business-kpis", nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "42") {
		t.Fatalf("custom page did not render: %d", rec.Code)
	}
	if !strings.Contains(body, "Could not load: card panicked: nope") {
		t.Fatal("a panicking card should be contained and reported in place")
	}
	if !strings.Contains(body, ">Business KPIs</a>") {
		t.Fatal("custom page missing from navigation")
	}
}

func TestHTTPMiddlewareUsesMuxPattern(t *testing.T) {
	p := newTestPulse(t, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		if SpanFromContext(r.Context()) == nil {
			t.Error("handler context carries no span")
		}
		w.WriteHeader(http.StatusTeapot)
	})
	mux.Handle("/_pulse/", p.Handler())
	h := p.Middleware(mux)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/items/7", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nowhere", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/_pulse/overview", nil))
	p.Flush()

	now := time.Now()
	totals, _ := p.Totals(context.Background(), MetricHTTP, now.Add(-time.Hour), now)
	if totals["GET /items/{id}"].Count != 1 || totals["GET "+Unmatched].Count != 1 || len(totals) != 2 {
		t.Fatalf("routes = %v, want the pattern, one unmatched, and no dashboard requests", totals)
	}
}
