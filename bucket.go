package pulse

import "time"

// Resolution is one tier of time buckets.
type Resolution struct {
	Name      string
	Step      time.Duration
	Retention time.Duration
}

var (
	Res10s = Resolution{Name: "10s", Step: 10 * time.Second, Retention: time.Hour}
	Res1m  = Resolution{Name: "1m", Step: time.Minute, Retention: 24 * time.Hour}
	Res1h  = Resolution{Name: "1h", Step: time.Hour, Retention: 7 * 24 * time.Hour}

	// Resolutions lists every tier. Each recorded value is written to all of them.
	Resolutions = []Resolution{Res10s, Res1m, Res1h}
)

// Floor returns the start of the bucket containing t.
func (r Resolution) Floor(t time.Time) time.Time {
	step := int64(r.Step / time.Second)
	return time.Unix(t.Unix()/step*step, 0)
}

// ResolutionFor picks the finest tier whose retention covers the window.
func ResolutionFor(from, to time.Time) Resolution {
	span := to.Sub(from)
	for _, r := range Resolutions {
		if span <= r.Retention {
			return r
		}
	}
	return Res1h
}

// HistBounds are the upper bounds, in milliseconds, of the duration histogram.
// A final overflow bucket holds everything above the last bound.
var HistBounds = []float64{1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

func histIndex(ms float64) int {
	for i, b := range HistBounds {
		if ms <= b {
			return i
		}
	}
	return len(HistBounds)
}

// Agg is the aggregate of all values recorded for one key in one bucket.
type Agg struct {
	Count int64
	Sum   float64
	Hist  []int64
}

// Merge adds b into a.
func (a *Agg) Merge(b Agg) {
	a.Count += b.Count
	a.Sum += b.Sum
	if len(b.Hist) > 0 {
		if len(a.Hist) < len(b.Hist) {
			grown := make([]int64, len(b.Hist))
			copy(grown, a.Hist)
			a.Hist = grown
		}
		for i, v := range b.Hist {
			a.Hist[i] += v
		}
	}
}

func (a Agg) clone() Agg {
	if a.Hist != nil {
		a.Hist = append([]int64(nil), a.Hist...)
	}
	return a
}

// Avg returns Sum/Count, or 0 for an empty aggregate.
func (a Agg) Avg() float64 {
	if a.Count == 0 {
		return 0
	}
	return a.Sum / float64(a.Count)
}

// Percentile estimates the p-th percentile (0..1) in milliseconds from the
// histogram by linear interpolation inside the matching bucket. Values in the
// overflow bucket are reported as the last bound.
func (a Agg) Percentile(p float64) float64 {
	var total int64
	for _, v := range a.Hist {
		total += v
	}
	if total == 0 {
		return 0
	}
	rank := p * float64(total)
	var seen float64
	for i, v := range a.Hist {
		if v == 0 {
			continue
		}
		if seen+float64(v) >= rank {
			if i >= len(HistBounds) {
				return HistBounds[len(HistBounds)-1]
			}
			lower := 0.0
			if i > 0 {
				lower = HistBounds[i-1]
			}
			return lower + (HistBounds[i]-lower)*(rank-seen)/float64(v)
		}
		seen += float64(v)
	}
	return HistBounds[len(HistBounds)-1]
}
