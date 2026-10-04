package pulse

import (
	"context"
	"sort"
	"sync"
	"time"
)

// AggKey addresses one aggregate: a metric, a key within it, and a time bucket.
type AggKey struct {
	Metric string
	Key    string
	Res    string
	Start  int64
}

// AggDelta is an increment to apply to a stored aggregate.
type AggDelta struct {
	AggKey
	Agg
}

// AggPoint is a stored aggregate returned by a read.
type AggPoint struct {
	Key   string
	Start time.Time
	Agg
}

// Entry is one item of a capped list. Data is JSON.
type Entry struct {
	Time time.Time
	Data []byte
}

// ErrorOccurrence is a single sighting of an error.
type ErrorOccurrence struct {
	Fingerprint string
	Time        time.Time
	Sample      []byte
}

// ErrorGroup is every sighting of one fingerprint, with the latest sample.
type ErrorGroup struct {
	Fingerprint string
	Count       int64
	FirstSeen   time.Time
	LastSeen    time.Time
	Sample      []byte
}

// Store persists what Pulse records. Implementations must be safe for
// concurrent use and must merge writes coming from several instances.
type Store interface {
	AddAggregates(ctx context.Context, deltas []AggDelta) error
	// Aggregates returns every key of metric with a bucket start in [from, to].
	Aggregates(ctx context.Context, metric string, res Resolution, from, to time.Time) ([]AggPoint, error)

	AppendEntries(ctx context.Context, list string, entries []Entry, max int) error
	// Entries returns the newest entries first.
	Entries(ctx context.Context, list string, limit int) ([]Entry, error)

	RecordErrors(ctx context.Context, occurrences []ErrorOccurrence, max int) error
	// Errors returns groups ordered by last sighting, newest first.
	Errors(ctx context.Context, limit int) ([]ErrorGroup, error)
	Error(ctx context.Context, fingerprint string) (ErrorGroup, bool, error)

	PutHost(ctx context.Context, instance string, data []byte, ttl time.Duration) error
	Hosts(ctx context.Context) (map[string][]byte, error)
}

type memSeriesKey struct {
	metric string
	res    string
}

type memHost struct {
	data    []byte
	expires time.Time
}

// MemoryStore keeps everything in process memory. Data is lost on restart and
// is not shared between instances.
type MemoryStore struct {
	mu      sync.RWMutex
	aggs    map[memSeriesKey]map[int64]map[string]*Agg
	entries map[string][]Entry
	errors  map[string]*ErrorGroup
	hosts   map[string]memHost
	now     func() time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		aggs:    map[memSeriesKey]map[int64]map[string]*Agg{},
		entries: map[string][]Entry{},
		errors:  map[string]*ErrorGroup{},
		hosts:   map[string]memHost{},
		now:     time.Now,
	}
}

func retentionOf(res string) time.Duration {
	for _, r := range Resolutions {
		if r.Name == res {
			return r.Retention
		}
	}
	return Res1h.Retention
}

func (s *MemoryStore) AddAggregates(_ context.Context, deltas []AggDelta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	touched := map[memSeriesKey]struct{}{}
	for _, d := range deltas {
		sk := memSeriesKey{d.Metric, d.Res}
		buckets := s.aggs[sk]
		if buckets == nil {
			buckets = map[int64]map[string]*Agg{}
			s.aggs[sk] = buckets
		}
		keys := buckets[d.Start]
		if keys == nil {
			keys = map[string]*Agg{}
			buckets[d.Start] = keys
		}
		a := keys[d.Key]
		if a == nil {
			a = &Agg{}
			keys[d.Key] = a
		}
		a.Merge(d.Agg)
		touched[sk] = struct{}{}
	}
	for sk := range touched {
		cutoff := s.now().Add(-retentionOf(sk.res)).Unix()
		for start := range s.aggs[sk] {
			if start < cutoff {
				delete(s.aggs[sk], start)
			}
		}
	}
	return nil
}

func (s *MemoryStore) Aggregates(_ context.Context, metric string, res Resolution, from, to time.Time) ([]AggPoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []AggPoint
	lo, hi := from.Unix(), to.Unix()
	for start, keys := range s.aggs[memSeriesKey{metric, res.Name}] {
		if start < lo || start > hi {
			continue
		}
		for k, a := range keys {
			out = append(out, AggPoint{Key: k, Start: time.Unix(start, 0), Agg: a.clone()})
		}
	}
	return out, nil
}

func (s *MemoryStore) AppendEntries(_ context.Context, list string, entries []Entry, max int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := append(s.entries[list], entries...)
	if max > 0 && len(l) > max {
		l = append([]Entry(nil), l[len(l)-max:]...)
	}
	s.entries[list] = l
	return nil
}

func (s *MemoryStore) Entries(_ context.Context, list string, limit int) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l := s.entries[list]
	out := make([]Entry, 0, min(limit, len(l)))
	for i := len(l) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, l[i])
	}
	return out, nil
}

func (s *MemoryStore) RecordErrors(_ context.Context, occurrences []ErrorOccurrence, max int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range occurrences {
		g := s.errors[o.Fingerprint]
		if g == nil {
			g = &ErrorGroup{Fingerprint: o.Fingerprint, FirstSeen: o.Time}
			s.errors[o.Fingerprint] = g
		}
		g.Count++
		if !o.Time.Before(g.LastSeen) {
			g.LastSeen = o.Time
			g.Sample = o.Sample
		}
	}
	if max > 0 && len(s.errors) > max {
		groups := make([]*ErrorGroup, 0, len(s.errors))
		for _, g := range s.errors {
			groups = append(groups, g)
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].LastSeen.After(groups[j].LastSeen) })
		for _, g := range groups[max:] {
			delete(s.errors, g.Fingerprint)
		}
	}
	return nil
}

func (s *MemoryStore) Errors(_ context.Context, limit int) ([]ErrorGroup, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ErrorGroup, 0, len(s.errors))
	for _, g := range s.errors {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) Error(_ context.Context, fingerprint string) (ErrorGroup, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.errors[fingerprint]
	if !ok {
		return ErrorGroup{}, false, nil
	}
	return *g, true, nil
}

func (s *MemoryStore) PutHost(_ context.Context, instance string, data []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[instance] = memHost{data: data, expires: s.now().Add(ttl)}
	return nil
}

func (s *MemoryStore) Hosts(_ context.Context) (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]byte{}
	for k, h := range s.hosts {
		if s.now().After(h.expires) {
			delete(s.hosts, k)
			continue
		}
		out[k] = h.data
	}
	return out, nil
}
