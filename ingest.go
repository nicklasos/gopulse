package pulse

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"
)

// ErrorSample is the stored detail of one error sighting.
type ErrorSample struct {
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	Method    string    `json:"method,omitempty"`
	Route     string    `json:"route,omitempty"`
	Path      string    `json:"path,omitempty"`
	Status    int       `json:"status,omitempty"`
	Stack     string    `json:"stack,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	Time      time.Time `json:"time"`
}

// SlowRequest is a single request that exceeded Config.SlowRequest.
type SlowRequest struct {
	ID         string    `json:"id"`
	Method     string    `json:"method"`
	Route      string    `json:"route"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMS float64   `json:"duration_ms"`
	Queries    int       `json:"queries,omitempty"`
	QueryMS    float64   `json:"query_ms,omitempty"`
	Time       time.Time `json:"time"`
}

type requestEvent struct {
	queries int
	queryMS float64
	t       time.Time
	id      string
	method  string
	route   string
	path    string
	status  int
	dur     time.Duration
}

type errorEvent struct{ ErrorSample }

type customEvent struct {
	t      time.Time
	metric string
	key    string
	value  float64
}

type hostEvent struct{ sample HostSample }

type entryEvent struct {
	list string
	t    time.Time
	data any
}

type pending struct {
	aggs    map[AggKey]*Agg
	entries map[string][]Entry
	errors  []ErrorOccurrence
	host    []byte
}

func newPending() *pending {
	return &pending{aggs: map[AggKey]*Agg{}, entries: map[string][]Entry{}}
}

func (b *pending) empty() bool {
	return len(b.aggs) == 0 && len(b.entries) == 0 && len(b.errors) == 0 && b.host == nil
}

func (b *pending) add(metric, key string, t time.Time, value float64, hist int) {
	for _, res := range Resolutions {
		k := AggKey{Metric: metric, Key: key, Res: res.Name, Start: res.Floor(t).Unix()}
		a := b.aggs[k]
		if a == nil {
			a = &Agg{}
			b.aggs[k] = a
		}
		a.Count++
		a.Sum += value
		if hist >= 0 {
			if a.Hist == nil {
				a.Hist = make([]int64, len(HistBounds)+1)
			}
			a.Hist[hist]++
		}
	}
}

func (b *pending) entry(list string, t time.Time, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.entries[list] = append(b.entries[list], Entry{Time: t, Data: data})
}

func (p *Pulse) run() {
	defer p.wg.Done()
	batch := newPending()
	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()

	drain := func() {
		for {
			select {
			case e := <-p.events:
				p.apply(batch, e)
			default:
				return
			}
		}
	}
	flush := func() {
		if batch.empty() {
			return
		}
		p.write(batch)
		batch = newPending()
	}

	for {
		select {
		case e := <-p.events:
			p.apply(batch, e)
		case <-ticker.C:
			flush()
		case ack := <-p.flushReq:
			drain()
			flush()
			close(ack)
		case <-p.done:
			drain()
			flush()
			return
		}
	}
}

func routeKey(method, route string) string {
	if method == "" {
		return route
	}
	return method + " " + route
}

func (p *Pulse) apply(b *pending, e any) {
	switch ev := e.(type) {
	case requestEvent:
		ms := float64(ev.dur) / float64(time.Millisecond)
		key := routeKey(ev.method, ev.route)
		b.add(MetricHTTP, key, ev.t, ms, histIndex(ms))
		switch {
		case ev.status >= 500:
			b.add(MetricHTTP5xx, key, ev.t, 1, -1)
		case ev.status >= 400:
			b.add(MetricHTTP4xx, key, ev.t, 1, -1)
		}
		if ev.dur >= p.cfg.SlowRequest {
			b.entry(ListSlowRequests, ev.t, SlowRequest{
				ID: ev.id, Method: ev.method, Route: ev.route, Path: ev.path,
				Status: ev.status, DurationMS: ms, Queries: ev.queries, QueryMS: ev.queryMS, Time: ev.t,
			})
		}
	case queryEvent:
		ms := float64(ev.dur) / float64(time.Millisecond)
		name := ev.name
		if _, known := p.queryKeys[name]; !known {
			if len(p.queryKeys) >= maxQueryKeys {
				name = otherQueries
			} else {
				p.queryKeys[name] = struct{}{}
			}
		}
		b.add(MetricQuery, name, ev.t, ms, histIndex(ms))
		if ev.err != "" {
			b.add(MetricQueryErrors, name, ev.t, 1, -1)
		}
		if ev.sql != "" {
			b.entry(ListSlowQueries, ev.t, SlowQuery{
				Name: ev.name, SQL: ev.sql, DurationMS: ms, Error: ev.err,
				RequestID: ev.id, Method: ev.meth, Route: ev.route, Time: ev.t,
			})
		}
	case errorEvent:
		data, err := json.Marshal(ev.ErrorSample)
		if err != nil {
			return
		}
		b.errors = append(b.errors, ErrorOccurrence{
			Fingerprint: fingerprint(ev.ErrorSample),
			Time:        ev.Time,
			Sample:      data,
		})
	case customEvent:
		b.add(ev.metric, ev.key, ev.t, ev.value, -1)
	case entryEvent:
		b.entry(ev.list, ev.t, ev.data)
	case hostEvent:
		s := ev.sample
		b.add(MetricHostCPU, s.Instance, s.Time, s.CPUPercent, -1)
		b.add(MetricHostMem, s.Instance, s.Time, s.MemPercent(), -1)
		b.add(MetricHostLoad, s.Instance, s.Time, s.Load1, -1)
		b.add(MetricHostGoroutines, s.Instance, s.Time, float64(s.Goroutines), -1)
		if data, err := json.Marshal(s); err == nil {
			b.host = data
		}
	}
}

func (p *Pulse) write(b *pending) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if len(b.aggs) > 0 {
		deltas := make([]AggDelta, 0, len(b.aggs))
		for k, a := range b.aggs {
			deltas = append(deltas, AggDelta{AggKey: k, Agg: *a})
		}
		keep(p.store.AddAggregates(ctx, deltas))
	}
	for list, entries := range b.entries {
		limit := p.cfg.MaxEntries
		if list == ListLogs {
			limit = p.cfg.MaxLogs
		}
		keep(p.store.AppendEntries(ctx, list, entries, limit))
	}
	if len(b.errors) > 0 {
		keep(p.store.RecordErrors(ctx, b.errors, p.cfg.MaxEntries))
	}
	if b.host != nil {
		ttl := 3*p.cfg.HostInterval + p.cfg.FlushInterval
		keep(p.store.PutHost(ctx, p.cfg.Instance, b.host, ttl))
	}

	if firstErr != nil {
		p.storeErr.Store(firstErr.Error())
		return
	}
	p.storeErr.Store("")
}

var (
	reDigits = regexp.MustCompile(`\d+`)
	reHex    = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
)

func normaliseMessage(msg string) string {
	if len(msg) > 300 {
		msg = msg[:300]
	}
	msg = reHex.ReplaceAllString(msg, "#")
	return reDigits.ReplaceAllString(msg, "#")
}

func fingerprint(s ErrorSample) string {
	sum := sha1.Sum([]byte(s.Kind + "|" + s.Method + "|" + s.Route + "|" + normaliseMessage(s.Message)))
	return hex.EncodeToString(sum[:8])
}
