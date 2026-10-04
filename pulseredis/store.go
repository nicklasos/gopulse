// Package pulseredis stores pulse data in Redis, so history survives
// restarts and several instances of a service share one dashboard.
package pulseredis

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	pulse "github.com/nicklasos/gopulse"
)

const errorRetention = 7 * 24 * time.Hour

// Store implements pulse.Store on a go-redis client.
type Store struct {
	rdb    redis.UniversalClient
	prefix string
	now    func() time.Time
}

// New returns a store that keeps every key under "gopulse:<app>:". Use a
// different app name per service when they share one Redis.
func New(rdb redis.UniversalClient, app string) *Store {
	return &Store{rdb: rdb, prefix: "gopulse:" + app + ":", now: time.Now}
}

func (s *Store) aggKey(metric, res string, start int64) string {
	return s.prefix + "agg:" + metric + ":" + res + ":" + strconv.FormatInt(start, 10)
}

func resolution(name string) pulse.Resolution {
	for _, r := range pulse.Resolutions {
		if r.Name == name {
			return r
		}
	}
	return pulse.Res1h
}

func (s *Store) AddAggregates(ctx context.Context, deltas []pulse.AggDelta) error {
	pipe := s.rdb.Pipeline()
	expiries := map[string]time.Duration{}
	for _, d := range deltas {
		key := s.aggKey(d.Metric, d.Res, d.Start)
		if d.Count != 0 {
			pipe.HIncrBy(ctx, key, "c|"+d.Key, d.Count)
		}
		if d.Sum != 0 {
			pipe.HIncrByFloat(ctx, key, "s|"+d.Key, d.Sum)
		}
		for i, v := range d.Hist {
			if v != 0 {
				pipe.HIncrBy(ctx, key, "h"+strconv.Itoa(i)+"|"+d.Key, v)
			}
		}
		res := resolution(d.Res)
		expiries[key] = res.Retention + res.Step
	}
	for key, ttl := range expiries {
		pipe.Expire(ctx, key, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) Aggregates(ctx context.Context, metric string, res pulse.Resolution, from, to time.Time) ([]pulse.AggPoint, error) {
	step := int64(res.Step / time.Second)
	first := res.Floor(from).Unix()
	if first < from.Unix() {
		first += step
	}
	var starts []int64
	var cmds []*redis.MapStringStringCmd
	pipe := s.rdb.Pipeline()
	for start := first; start <= to.Unix(); start += step {
		starts = append(starts, start)
		cmds = append(cmds, pipe.HGetAll(ctx, s.aggKey(metric, res.Name, start)))
	}
	if len(cmds) == 0 {
		return nil, nil
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}

	var out []pulse.AggPoint
	for i, cmd := range cmds {
		fields := cmd.Val()
		if len(fields) == 0 {
			continue
		}
		byKey := map[string]*pulse.Agg{}
		for field, raw := range fields {
			kind, key, ok := strings.Cut(field, "|")
			if !ok {
				continue
			}
			a := byKey[key]
			if a == nil {
				a = &pulse.Agg{}
				byKey[key] = a
			}
			switch {
			case kind == "c":
				a.Count, _ = strconv.ParseInt(raw, 10, 64)
			case kind == "s":
				a.Sum, _ = strconv.ParseFloat(raw, 64)
			case strings.HasPrefix(kind, "h"):
				idx, err := strconv.Atoi(kind[1:])
				if err != nil || idx < 0 || idx > len(pulse.HistBounds) {
					continue
				}
				if a.Hist == nil {
					a.Hist = make([]int64, len(pulse.HistBounds)+1)
				}
				a.Hist[idx], _ = strconv.ParseInt(raw, 10, 64)
			}
		}
		at := time.Unix(starts[i], 0)
		for key, a := range byKey {
			out = append(out, pulse.AggPoint{Key: key, Start: at, Agg: *a})
		}
	}
	return out, nil
}

func (s *Store) AppendEntries(ctx context.Context, list string, entries []pulse.Entry, max int) error {
	pipe := s.rdb.Pipeline()
	for _, e := range entries {
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: s.prefix + "list:" + list,
			MaxLen: int64(max),
			Approx: true,
			Values: map[string]any{"t": e.Time.UnixNano(), "d": e.Data},
		})
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) Entries(ctx context.Context, list string, limit int) ([]pulse.Entry, error) {
	msgs, err := s.rdb.XRevRangeN(ctx, s.prefix+"list:"+list, "+", "-", int64(limit)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]pulse.Entry, 0, len(msgs))
	for _, m := range msgs {
		data, _ := m.Values["d"].(string)
		nanos, _ := strconv.ParseInt(asString(m.Values["t"]), 10, 64)
		out = append(out, pulse.Entry{Time: time.Unix(0, nanos), Data: []byte(data)})
	}
	return out, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func (s *Store) errorKey(fingerprint string) string { return s.prefix + "err:" + fingerprint }

func (s *Store) RecordErrors(ctx context.Context, occurrences []pulse.ErrorOccurrence, max int) error {
	index := s.prefix + "errs"
	pipe := s.rdb.Pipeline()
	for _, o := range occurrences {
		key := s.errorKey(o.Fingerprint)
		ms := o.Time.UnixMilli()
		pipe.HIncrBy(ctx, key, "count", 1)
		pipe.HSetNX(ctx, key, "first", ms)
		pipe.HSet(ctx, key, "last", ms, "sample", o.Sample)
		pipe.Expire(ctx, key, errorRetention)
		pipe.ZAdd(ctx, index, redis.Z{Score: float64(ms), Member: o.Fingerprint})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}

	cutoff := s.now().Add(-errorRetention).UnixMilli()
	if err := s.rdb.ZRemRangeByScore(ctx, index, "-inf", strconv.FormatInt(cutoff, 10)).Err(); err != nil {
		return err
	}
	if max <= 0 {
		return nil
	}
	stale, err := s.rdb.ZRevRange(ctx, index, int64(max), -1).Result()
	if err != nil || len(stale) == 0 {
		return err
	}
	pipe = s.rdb.Pipeline()
	for _, fp := range stale {
		pipe.Del(ctx, s.errorKey(fp))
		pipe.ZRem(ctx, index, fp)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *Store) loadErrors(ctx context.Context, fingerprints []string) ([]pulse.ErrorGroup, error) {
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(fingerprints))
	for i, fp := range fingerprints {
		cmds[i] = pipe.HGetAll(ctx, s.errorKey(fp))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make([]pulse.ErrorGroup, 0, len(fingerprints))
	for i, cmd := range cmds {
		f := cmd.Val()
		if len(f) == 0 {
			continue
		}
		count, _ := strconv.ParseInt(f["count"], 10, 64)
		first, _ := strconv.ParseInt(f["first"], 10, 64)
		last, _ := strconv.ParseInt(f["last"], 10, 64)
		out = append(out, pulse.ErrorGroup{
			Fingerprint: fingerprints[i],
			Count:       count,
			FirstSeen:   time.UnixMilli(first),
			LastSeen:    time.UnixMilli(last),
			Sample:      []byte(f["sample"]),
		})
	}
	return out, nil
}

func (s *Store) Errors(ctx context.Context, limit int) ([]pulse.ErrorGroup, error) {
	stop := int64(-1)
	if limit > 0 {
		stop = int64(limit - 1)
	}
	fps, err := s.rdb.ZRevRange(ctx, s.prefix+"errs", 0, stop).Result()
	if err != nil || len(fps) == 0 {
		return nil, err
	}
	return s.loadErrors(ctx, fps)
}

func (s *Store) Error(ctx context.Context, fingerprint string) (pulse.ErrorGroup, bool, error) {
	groups, err := s.loadErrors(ctx, []string{fingerprint})
	if err != nil || len(groups) == 0 {
		return pulse.ErrorGroup{}, false, err
	}
	return groups[0], true, nil
}

func (s *Store) PutHost(ctx context.Context, instance string, data []byte, ttl time.Duration) error {
	pipe := s.rdb.Pipeline()
	pipe.Set(ctx, s.prefix+"host:"+instance, data, ttl)
	pipe.ZAdd(ctx, s.prefix+"hosts", redis.Z{Score: float64(s.now().Add(ttl).UnixMilli()), Member: instance})
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) Hosts(ctx context.Context) (map[string][]byte, error) {
	index := s.prefix + "hosts"
	now := strconv.FormatInt(s.now().UnixMilli(), 10)
	if err := s.rdb.ZRemRangeByScore(ctx, index, "-inf", "("+now).Err(); err != nil {
		return nil, err
	}
	instances, err := s.rdb.ZRange(ctx, index, 0, -1).Result()
	if err != nil || len(instances) == 0 {
		return map[string][]byte{}, err
	}
	keys := make([]string, len(instances))
	for i, inst := range instances {
		keys[i] = s.prefix + "host:" + inst
	}
	values, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for i, v := range values {
		if str, ok := v.(string); ok {
			out[instances[i]] = []byte(str)
		}
	}
	return out, nil
}
