// Package pulse records request, error, query, log and host statistics for a
// Go web service and serves them as a password-protected HTML dashboard.
package pulse

import (
	"context"
	"html/template"
	"sync"
	"sync/atomic"
	"time"
)

// Built-in metric and list names.
const (
	MetricHTTP           = "http"
	MetricHTTP4xx        = "http.4xx"
	MetricHTTP5xx        = "http.5xx"
	MetricHostCPU        = "host.cpu"
	MetricHostMem        = "host.mem"
	MetricHostLoad       = "host.load"
	MetricHostGoroutines = "host.goroutines"

	ListSlowRequests = "slow_requests"

	// Unmatched is the route recorded for requests no handler was registered for.
	Unmatched = "(unmatched)"
)

// Pulse is the recorder and dashboard. Create one with New and share it.
type Pulse struct {
	cfg   Config
	store Store

	events   chan any
	flushReq chan chan struct{}
	done     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once

	dropped  atomic.Int64
	storeErr atomic.Value

	started time.Time
	tmpl    *template.Template

	// queryKeys bounds how many distinct statements get their own aggregate.
	// It is only touched by the recorder goroutine.
	queryKeys map[string]struct{}

	mu    sync.RWMutex
	pages []*Page
}

// New starts the background recorder. Call Close on shutdown to flush.
func New(cfg Config) *Pulse {
	cfg = cfg.withDefaults()
	p := &Pulse{
		cfg:       cfg,
		store:     cfg.Store,
		events:    make(chan any, cfg.BufferSize),
		flushReq:  make(chan chan struct{}),
		done:      make(chan struct{}),
		started:   time.Now(),
		tmpl:      parseTemplates(),
		queryKeys: map[string]struct{}{},
	}
	p.storeErr.Store("")
	p.registerBuiltinPages()

	p.wg.Add(1)
	go p.run()
	if cfg.HostInterval > 0 {
		p.wg.Add(1)
		go p.sampleHost()
	}
	return p
}

// Path is the URL prefix the dashboard is served under.
func (p *Pulse) Path() string { return p.cfg.Path }

// App is the configured service name.
func (p *Pulse) App() string { return p.cfg.App }

// Store exposes the underlying store for custom cards.
func (p *Pulse) Store() Store { return p.store }

// Close flushes buffered data and stops background work.
func (p *Pulse) Close() {
	p.once.Do(func() {
		close(p.done)
		p.wg.Wait()
	})
}

// Flush writes everything recorded so far to the store and waits for it.
func (p *Pulse) Flush() {
	ack := make(chan struct{})
	select {
	case p.flushReq <- ack:
		<-ack
	case <-p.done:
	}
}

// Dropped reports how many events were discarded because the queue was full.
func (p *Pulse) Dropped() int64 { return p.dropped.Load() }

// StoreError returns the last store failure, or "" when the last write succeeded.
func (p *Pulse) StoreError() string { return p.storeErr.Load().(string) }

// Record adds value to a custom metric under key. Each call counts once, so
// both totals (Sum) and averages (Sum/Count) are available to cards.
func (p *Pulse) Record(metric, key string, value float64) {
	p.emit(customEvent{t: time.Now(), metric: metric, key: key, value: value})
}

// Report records an error that did not surface as a failed request. When ctx
// belongs to a tracked request the error is attributed to its route.
func (p *Pulse) Report(ctx context.Context, err error) {
	if err == nil {
		return
	}
	s := ErrorSample{Kind: "error", Message: err.Error(), Time: time.Now()}
	if span := SpanFromContext(ctx); span != nil {
		s.Method, s.Route, s.RequestID = span.Method, span.Route, span.ID
	}
	p.emit(errorEvent{s})
}

func (p *Pulse) emit(e any) {
	select {
	case p.events <- e:
	default:
		p.dropped.Add(1)
	}
}
