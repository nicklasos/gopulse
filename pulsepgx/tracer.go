// Package pulsepgx reports pgx v5 queries to pulse.
package pulsepgx

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	pulse "github.com/nicklasos/gopulse"
)

type queryKey struct{}

type queryStart struct {
	sql string
	at  time.Time
}

// Tracer implements pgx.QueryTracer. Only the statement text is recorded,
// never its arguments.
type Tracer struct {
	p    *pulse.Pulse
	next pgx.QueryTracer
}

// New returns a tracer to set as ConnConfig.Tracer on a pgx or pgxpool config.
func New(p *pulse.Pulse) *Tracer { return &Tracer{p: p} }

// Wrap returns a tracer that also forwards every call to next, for
// applications that already have a tracer installed.
func Wrap(p *pulse.Pulse, next pgx.QueryTracer) *Tracer { return &Tracer{p: p, next: next} }

func (t *Tracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if t.next != nil {
		ctx = t.next.TraceQueryStart(ctx, conn, data)
	}
	return context.WithValue(ctx, queryKey{}, queryStart{sql: data.SQL, at: time.Now()})
}

func (t *Tracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if start, ok := ctx.Value(queryKey{}).(queryStart); ok {
		t.p.RecordQuery(ctx, start.sql, time.Since(start.at), data.Err)
	}
	if t.next != nil {
		t.next.TraceQueryEnd(ctx, conn, data)
	}
}
