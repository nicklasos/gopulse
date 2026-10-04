package pulse

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"
)

type spanKey struct{}

// Span tracks one in-flight request. Framework adapters create it with Start
// and finish it with End.
type Span struct {
	ID     string
	Method string
	Route  string

	p        *Pulse
	start    time.Time
	ended    atomic.Bool
	panicked bool
}

// Result describes how a request finished.
type Result struct {
	// Route overrides the route given to Start, for routers that only know it after dispatch.
	Route  string
	Path   string
	Status int
	Errors []error
}

// Start begins tracking a request. The returned context carries the span so
// queries, logs and reported errors can be attributed to the request.
func (p *Pulse) Start(ctx context.Context, method, route string) (context.Context, *Span) {
	s := &Span{
		ID:     strconv.FormatUint(rand.Uint64(), 36),
		Method: method,
		Route:  route,
		p:      p,
		start:  time.Now(),
	}
	return context.WithValue(ctx, spanKey{}, s), s
}

// SpanFromContext returns the span stored by Start, or nil.
func SpanFromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(spanKey{}).(*Span)
	return s
}

// Panic records a recovered panic value with its stack. The adapter should
// call End afterwards and re-panic so the application's own recovery still runs.
func (s *Span) Panic(v any, stack []byte) {
	s.panicked = true
	s.p.emit(errorEvent{ErrorSample{
		Kind:      "panic",
		Message:   fmt.Sprint(v),
		Method:    s.Method,
		Route:     s.Route,
		Status:    500,
		Stack:     string(stack),
		RequestID: s.ID,
		Time:      time.Now(),
	}})
}

// End records the finished request. Only the first call has an effect.
func (s *Span) End(r Result) {
	if !s.ended.CompareAndSwap(false, true) {
		return
	}
	now := time.Now()
	if r.Route != "" {
		s.Route = r.Route
	}
	s.p.emit(requestEvent{
		t:      now,
		id:     s.ID,
		method: s.Method,
		route:  s.Route,
		path:   r.Path,
		status: r.Status,
		dur:    now.Sub(s.start),
	})

	sample := ErrorSample{
		Method: s.Method, Route: s.Route, Path: r.Path,
		Status: r.Status, RequestID: s.ID, Time: now,
	}
	for _, err := range r.Errors {
		if err == nil {
			continue
		}
		e := sample
		e.Kind, e.Message = "error", err.Error()
		s.p.emit(errorEvent{e})
	}
	if len(r.Errors) == 0 && r.Status >= 500 && !s.panicked {
		sample.Kind, sample.Message = "http", "HTTP "+strconv.Itoa(r.Status)
		s.p.emit(errorEvent{sample})
	}
}
