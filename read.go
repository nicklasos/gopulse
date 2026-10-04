package pulse

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type readCacheKey struct{}

// readCache lets the cards of one page render share store reads.
type readCache struct {
	mu sync.Mutex
	m  map[string][]AggPoint
}

func withReadCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, readCacheKey{}, &readCache{m: map[string][]AggPoint{}})
}

func (p *Pulse) aggregates(ctx context.Context, metric string, res Resolution, from, to time.Time) ([]AggPoint, error) {
	cache, _ := ctx.Value(readCacheKey{}).(*readCache)
	if cache == nil {
		return p.store.Aggregates(ctx, metric, res, from, to)
	}
	key := fmt.Sprintf("%s|%s|%d|%d", metric, res.Name, from.Unix(), to.Unix())
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if pts, ok := cache.m[key]; ok {
		return pts, nil
	}
	pts, err := p.store.Aggregates(ctx, metric, res, from, to)
	if err != nil {
		return nil, err
	}
	cache.m[key] = pts
	return pts, nil
}

// Totals sums a metric per key over [from, to].
func (p *Pulse) Totals(ctx context.Context, metric string, from, to time.Time) (map[string]Agg, error) {
	res := ResolutionFor(from, to)
	pts, err := p.aggregates(ctx, metric, res, res.Floor(from), to)
	if err != nil {
		return nil, err
	}
	out := map[string]Agg{}
	for _, pt := range pts {
		a := out[pt.Key]
		a.Merge(pt.Agg)
		out[pt.Key] = a
	}
	return out, nil
}

// Total sums a metric across every key over [from, to].
func (p *Pulse) Total(ctx context.Context, metric string, from, to time.Time) (Agg, error) {
	totals, err := p.Totals(ctx, metric, from, to)
	var sum Agg
	for _, a := range totals {
		sum.Merge(a)
	}
	return sum, err
}

// SeriesPoint is one step of a time series.
type SeriesPoint struct {
	Time time.Time
	Agg
}

// Series is a gap-free time series: every step in the window has a point.
type Series struct {
	Step   time.Duration
	Points []SeriesPoint
}

// Series returns a metric over time. An empty key sums all keys. Buckets are
// merged so that at most maxPoints points are returned.
func (p *Pulse) Series(ctx context.Context, metric, key string, from, to time.Time, maxPoints int) (Series, error) {
	res := ResolutionFor(from, to)
	start := res.Floor(from)
	pts, err := p.aggregates(ctx, metric, res, start, to)
	if err != nil {
		return Series{}, err
	}
	if maxPoints <= 0 {
		maxPoints = 120
	}
	n := int(res.Floor(to).Sub(start)/res.Step) + 1
	group := (n + maxPoints - 1) / maxPoints
	step := res.Step * time.Duration(group)
	out := Series{Step: step, Points: make([]SeriesPoint, (n+group-1)/group)}
	for i := range out.Points {
		out.Points[i].Time = start.Add(time.Duration(i) * step)
	}
	for _, pt := range pts {
		if key != "" && pt.Key != key {
			continue
		}
		i := int(pt.Start.Sub(start) / step)
		if i < 0 || i >= len(out.Points) {
			continue
		}
		out.Points[i].Merge(pt.Agg)
	}
	return out, nil
}

// SlowRequests returns the most recent requests above the slow threshold.
func (p *Pulse) SlowRequests(ctx context.Context, limit int) ([]SlowRequest, error) {
	return readEntries[SlowRequest](ctx, p, ListSlowRequests, limit)
}

func readEntries[T any](ctx context.Context, p *Pulse, list string, limit int) ([]T, error) {
	entries, err := p.store.Entries(ctx, list, limit)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(entries))
	for _, e := range entries {
		var v T
		if json.Unmarshal(e.Data, &v) == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// ErrorInfo is an error group with its decoded sample.
type ErrorInfo struct {
	ErrorGroup
	ErrorSample
}

func decodeError(g ErrorGroup) ErrorInfo {
	info := ErrorInfo{ErrorGroup: g}
	_ = json.Unmarshal(g.Sample, &info.ErrorSample)
	return info
}

// Errors returns error groups, most recently seen first.
func (p *Pulse) Errors(ctx context.Context, limit int) ([]ErrorInfo, error) {
	groups, err := p.store.Errors(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ErrorInfo, len(groups))
	for i, g := range groups {
		out[i] = decodeError(g)
	}
	return out, nil
}

// Hosts returns the latest sample of every live instance.
func (p *Pulse) Hosts(ctx context.Context) ([]HostSample, error) {
	raw, err := p.store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]HostSample, 0, len(raw))
	for _, data := range raw {
		var s HostSample
		if json.Unmarshal(data, &s) == nil {
			out = append(out, s)
		}
	}
	return out, nil
}
