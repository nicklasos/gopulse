package pulsepgx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	pulse "github.com/nicklasos/gopulse"
)

type spyTracer struct{ starts, ends int }

type spyKey struct{}

func (s *spyTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	s.starts++
	return context.WithValue(ctx, spyKey{}, "kept")
}

func (s *spyTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(spyKey{}) == "kept" {
		s.ends++
	}
}

func TestTracerRecordsStatementWithoutArguments(t *testing.T) {
	p := pulse.New(pulse.Config{HostInterval: -1, FlushInterval: time.Hour, SlowQuery: time.Nanosecond})
	t.Cleanup(p.Close)
	spy := &spyTracer{}
	var tracer pgx.QueryTracer = Wrap(p, spy)

	reqCtx, span := p.Start(context.Background(), "GET", "/users/:id")
	ctx := tracer.TraceQueryStart(reqCtx, nil, pgx.TraceQueryStartData{
		SQL:  "-- name: GetUserByEmail :one\nSELECT * FROM users WHERE email = $1",
		Args: []any{"secret@example.com"},
	})
	time.Sleep(2 * time.Millisecond)
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("relation does not exist")})
	p.Flush()

	queries, err := p.SlowQueries(context.Background(), 10)
	if err != nil || len(queries) != 1 {
		t.Fatalf("queries = %+v, %v", queries, err)
	}
	q := queries[0]
	if q.Name != "GetUserByEmail" || q.Error != "relation does not exist" || q.RequestID != span.ID || q.DurationMS < 2 {
		t.Errorf("recorded query = %+v", q)
	}
	if q.SQL == "" || strings.Contains(q.SQL, "secret@example.com") {
		t.Errorf("stored SQL %q must be the statement text without argument values", q.SQL)
	}
	if spy.starts != 1 || spy.ends != 1 {
		t.Errorf("wrapped tracer saw %d starts and %d ends with its context, want 1 and 1", spy.starts, spy.ends)
	}
}
