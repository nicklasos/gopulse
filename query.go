package pulse

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// Query metrics and lists.
const (
	MetricQuery       = "query"
	MetricQueryErrors = "query.errors"

	ListSlowQueries = "slow_queries"

	otherQueries    = "(other queries)"
	maxQueryKeys    = 500
	maxQueryKeyLen  = 160
	maxStoredSQLLen = 4000
)

// SlowQuery is a single database query that exceeded Config.SlowQuery.
type SlowQuery struct {
	Name       string    `json:"name"`
	SQL        string    `json:"sql"`
	DurationMS float64   `json:"duration_ms"`
	Error      string    `json:"error,omitempty"`
	RequestID  string    `json:"request_id,omitempty"`
	Method     string    `json:"method,omitempty"`
	Route      string    `json:"route,omitempty"`
	Time       time.Time `json:"time"`
}

type queryEvent struct {
	t     time.Time
	name  string
	sql   string
	dur   time.Duration
	err   string
	id    string
	meth  string
	route string
}

// RecordQuery records one database query. Database adapters such as pulsepgx
// call it; pass only the statement text, never its arguments.
func (p *Pulse) RecordQuery(ctx context.Context, sql string, d time.Duration, err error) {
	ev := queryEvent{t: time.Now(), name: QueryName(sql), dur: d}
	if err != nil {
		ev.err = err.Error()
	}
	if span := SpanFromContext(ctx); span != nil {
		ev.id, ev.meth, ev.route = span.ID, span.Method, span.Route
		span.queries.Add(1)
		span.queryNanos.Add(int64(d))
	}
	if d >= p.cfg.SlowQuery || err != nil {
		ev.sql = sql
		if len(ev.sql) > maxStoredSQLLen {
			ev.sql = ev.sql[:maxStoredSQLLen] + "…"
		}
	}
	p.emit(ev)
}

var (
	reSQLCName  = regexp.MustCompile(`^\s*--\s*name:\s*(\w+)`)
	reSQLString = regexp.MustCompile(`'(?:[^']|'')*'`)
	reSQLNumber = regexp.MustCompile(`\$?\b\d+(?:\.\d+)?\b`)
	reSQLList   = regexp.MustCompile(`\(\s*\?(?:\s*,\s*\?)+\s*\)`)
	reSpace     = regexp.MustCompile(`\s+`)
	reSQLLine   = regexp.MustCompile(`--[^\n]*`)
)

// QueryName returns a stable, low-cardinality label for a statement: the
// sqlc query name when the text carries one, otherwise the statement with
// literals replaced by placeholders.
func QueryName(sql string) string {
	if m := reSQLCName.FindStringSubmatch(sql); m != nil {
		return m[1]
	}
	s := reSQLLine.ReplaceAllString(sql, " ")
	s = reSQLString.ReplaceAllString(s, "?")
	s = reSQLNumber.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "$") {
			return m
		}
		return "?"
	})
	s = reSQLList.ReplaceAllString(s, "(?)")
	s = strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > maxQueryKeyLen {
		s = string(r[:maxQueryKeyLen]) + "…"
	}
	if s == "" {
		return "(empty)"
	}
	return s
}

// SlowQueries returns the most recent queries above the slow threshold.
func (p *Pulse) SlowQueries(ctx context.Context, limit int) ([]SlowQuery, error) {
	return readEntries[SlowQuery](ctx, p, ListSlowQueries, limit)
}
