package pulse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueryName(t *testing.T) {
	cases := map[string]string{
		"-- name: GetUserByID :one\nSELECT * FROM users WHERE id = $1": "GetUserByID",
		"SELECT *\n  FROM users\n WHERE id = 42":                       "SELECT * FROM users WHERE id = ?",
		"SELECT * FROM users WHERE email = 'a@b.c' AND note = 'it''s'": "SELECT * FROM users WHERE email = ? AND note = ?",
		"SELECT * FROM t WHERE id IN (1, 2, 3) -- trailing":            "SELECT * FROM t WHERE id IN (?)",
		"SELECT * FROM t WHERE a = $1 AND b > 10 LIMIT $2":             "SELECT * FROM t WHERE a = $1 AND b > ? LIMIT $2",
		"   ": "(empty)",
	}
	for sql, want := range cases {
		if got := QueryName(sql); got != want {
			t.Errorf("QueryName(%q) = %q, want %q", sql, got, want)
		}
	}
	if got := QueryName("SELECT " + strings.Repeat("col, ", 100) + "x FROM t"); len([]rune(got)) > maxQueryKeyLen+1 {
		t.Errorf("long statement label has %d runes, want it capped", len([]rune(got)))
	}
}

func TestQueriesAggregateAndSlowOnesAreKept(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.SlowQuery = 50 * time.Millisecond })
	ctx, span := p.Start(context.Background(), "GET", "/users/:id")

	p.RecordQuery(ctx, "-- name: GetUser :one\nSELECT * FROM users WHERE id = $1", 2*time.Millisecond, nil)
	p.RecordQuery(ctx, "-- name: GetUser :one\nSELECT * FROM users WHERE id = $1", 4*time.Millisecond, nil)
	p.RecordQuery(ctx, "SELECT pg_sleep(1)", 80*time.Millisecond, nil)
	p.RecordQuery(context.Background(), "UPDATE t SET a = 1", time.Millisecond, errors.New("deadlock detected"))
	span.End(Result{Path: "/users/1", Status: 200})
	p.Flush()

	now := time.Now()
	totals, _ := p.Totals(ctx, MetricQuery, now.Add(-time.Hour), now)
	if a := totals["GetUser"]; a.Count != 2 || a.Avg() != 3 {
		t.Errorf("GetUser aggregate = %+v, want count 2 avg 3ms", a)
	}
	failed, _ := p.Totals(ctx, MetricQueryErrors, now.Add(-time.Hour), now)
	if failed["UPDATE t SET a = ?"].Count != 1 || len(failed) != 1 {
		t.Errorf("failed queries = %v, want only the UPDATE", failed)
	}

	slow, _ := p.SlowQueries(ctx, 10)
	if len(slow) != 2 {
		t.Fatalf("kept %d queries, want the slow one and the failed one", len(slow))
	}
	for _, q := range slow {
		switch {
		case q.Error != "":
			if q.RequestID != "" || q.SQL != "UPDATE t SET a = 1" {
				t.Errorf("failed query entry = %+v", q)
			}
		default:
			if q.RequestID != span.ID || q.Route != "/users/:id" || q.DurationMS != 80 {
				t.Errorf("slow query entry = %+v, want it linked to the request", q)
			}
		}
	}
}

func TestRequestCarriesItsQueryTotals(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.SlowRequest = time.Nanosecond })
	ctx, span := p.Start(context.Background(), "GET", "/report")
	p.RecordQuery(ctx, "SELECT 1", 3*time.Millisecond, nil)
	p.RecordQuery(ctx, "SELECT 2", 7*time.Millisecond, nil)
	span.End(Result{Status: 200})
	p.Flush()

	reqs, _ := p.SlowRequests(ctx, 1)
	if len(reqs) != 1 || reqs[0].Queries != 2 || reqs[0].QueryMS != 10 {
		t.Fatalf("request = %+v, want 2 queries totalling 10ms", reqs)
	}
}

func TestDistinctStatementsAreCapped(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.BufferSize = maxQueryKeys + 100 })
	for i := range maxQueryKeys + 20 {
		p.RecordQuery(context.Background(), fmt.Sprintf("SELECT * FROM table_%c%c%c", 'a'+i%26, 'a'+i/26%26, 'a'+i/676), time.Millisecond, nil)
	}
	p.Flush()
	now := time.Now()
	totals, _ := p.Totals(context.Background(), MetricQuery, now.Add(-time.Hour), now)
	if len(totals) != maxQueryKeys+1 || totals[otherQueries].Count != 20 {
		t.Fatalf("%d statement keys, %d folded; want the cap plus one overflow key holding 20", len(totals), totals[otherQueries].Count)
	}
}

func TestSlogHandlerCapturesAndPassesThrough(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.LogLevel = slog.LevelInfo })
	var out bytes.Buffer
	next := slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})
	log := slog.New(p.SlogHandler(next))

	ctx, span := p.Start(context.Background(), "POST", "/orders")
	log.Debug("too quiet to keep")
	log.With("service", "billing").WithGroup("order").InfoContext(ctx, "order created", "id", 7, "note", "two words")
	log.Error("payment failed", slog.Group("card", "brand", "visa"))
	p.Flush()

	if got := strings.Count(out.String(), "\n"); got != 3 {
		t.Fatalf("next handler received %d records, want all 3", got)
	}
	logs, _ := p.Logs(ctx, 10)
	if len(logs) != 2 {
		t.Fatalf("captured %d records, want 2 at info and above", len(logs))
	}
	errRec, infoRec := logs[0], logs[1]
	if infoRec.Message != "order created" || infoRec.Attrs != `service=billing order.id=7 order.note="two words"` {
		t.Errorf("info record = %+v", infoRec)
	}
	if infoRec.RequestID != span.ID || infoRec.Route != "/orders" {
		t.Errorf("info record not linked to its request: %+v", infoRec)
	}
	if errRec.Level != "ERROR" || errRec.Attrs != "card.brand=visa" || errRec.RequestID != "" {
		t.Errorf("error record = %+v", errRec)
	}
}

func TestSlogHandlerCapturesEvenWhenNextIsQuieter(t *testing.T) {
	p := newTestPulse(t, nil)
	var out bytes.Buffer
	log := slog.New(p.SlogHandler(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelError})))
	log.Info("kept by pulse only")
	p.Flush()
	logs, _ := p.Logs(context.Background(), 10)
	if len(logs) != 1 || out.Len() != 0 {
		t.Fatalf("captured %d, wrote %d bytes downstream; want 1 and 0", len(logs), out.Len())
	}
}

func TestClientDisconnectIsNotAServerError(t *testing.T) {
	p := newTestPulse(t, nil)
	_, span := p.Start(context.Background(), "GET", "/slow")
	span.End(Result{Status: 500, Errors: []error{context.Canceled}, ClientGone: true})
	_, ok := p.Start(context.Background(), "GET", "/fine")
	ok.End(Result{Status: 200, ClientGone: true})
	p.Flush()

	now := time.Now()
	ctx := context.Background()
	t5, _ := p.Total(ctx, MetricHTTP5xx, now.Add(-time.Hour), now)
	t4, _ := p.Total(ctx, MetricHTTP4xx, now.Add(-time.Hour), now)
	errs, _ := p.Errors(ctx, 10)
	if t5.Count != 0 || t4.Count != 1 || len(errs) != 0 {
		t.Fatalf("5xx=%d 4xx=%d errors=%d, want the abandoned request counted as 499 with no error", t5.Count, t4.Count, len(errs))
	}
}

func TestQueryLogAndRequestPagesRender(t *testing.T) {
	p := newTestPulse(t, func(c *Config) { c.SlowRequest, c.SlowQuery = time.Nanosecond, time.Nanosecond })
	log := slog.New(p.SlogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	ctx, span := p.Start(context.Background(), "GET", "/users/:id")
	p.RecordQuery(ctx, "SELECT '<b>x</b>' FROM users", time.Millisecond, errors.New("boom"))
	log.WarnContext(ctx, "cache miss", "key", "user:1")
	span.End(Result{Path: "/users/1", Status: 200})
	p.Flush()

	for path, want := range map[string]string{
		"/_pulse/queries":               "SELECT ? FROM users",
		"/_pulse/logs":                  "cache miss",
		"/_pulse/logs?level=ERROR":      "No ERROR records",
		"/_pulse/request?id=" + span.ID: "cache miss",
		"/_pulse/request?id=missing":    "request not found",
		"/_pulse/routes":                "/_pulse/request?id=" + span.ID,
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.SetBasicAuth("admin", "secret")
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: status %d, missing %q", path, rec.Code, want)
		}
		if strings.Contains(body, "<b>x</b>") {
			t.Errorf("%s: SQL text was not escaped", path)
		}
	}
}
