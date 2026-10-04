package pulseredis

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	pulse "github.com/nicklasos/gopulse"
)

func setup(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, "test"), mr
}

func delta(metric, key string, res pulse.Resolution, at time.Time, a pulse.Agg) pulse.AggDelta {
	return pulse.AggDelta{
		AggKey: pulse.AggKey{Metric: metric, Key: key, Res: res.Name, Start: res.Floor(at).Unix()},
		Agg:    a,
	}
}

func TestAggregatesMergeAcrossWriters(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	now := time.Now()
	hist := make([]int64, len(pulse.HistBounds)+1)
	hist[3], hist[len(hist)-1] = 2, 1

	for range 2 {
		err := s.AddAggregates(ctx, []pulse.AggDelta{
			delta("http", "GET /a|b", pulse.Res10s, now, pulse.Agg{Count: 3, Sum: 12.5, Hist: hist}),
			delta("http", "GET /other", pulse.Res10s, now, pulse.Agg{Count: 1, Sum: 1}),
			delta("http", "GET /a|b", pulse.Res1m, now, pulse.Agg{Count: 3, Sum: 12.5}),
			delta("unrelated", "GET /a|b", pulse.Res10s, now, pulse.Agg{Count: 99}),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	pts, err := s.Aggregates(ctx, "http", pulse.Res10s, pulse.Res10s.Floor(now.Add(-time.Minute)), now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]pulse.Agg{}
	for _, p := range pts {
		got[p.Key] = p.Agg
	}
	a := got["GET /a|b"]
	if len(got) != 2 || a.Count != 6 || a.Sum != 25 {
		t.Fatalf("aggregates = %+v, want two keys with the first summed to count 6 sum 25", got)
	}
	if a.Hist[3] != 4 || a.Hist[len(a.Hist)-1] != 2 {
		t.Fatalf("histogram = %v, want buckets summed across writes", a.Hist)
	}
}

func TestAggregatesRespectWindow(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	now := time.Now()
	_ = s.AddAggregates(ctx, []pulse.AggDelta{
		delta("m", "k", pulse.Res1m, now.Add(-30*time.Minute), pulse.Agg{Count: 1}),
		delta("m", "k", pulse.Res1m, now, pulse.Agg{Count: 1}),
	})
	pts, _ := s.Aggregates(ctx, "m", pulse.Res1m, pulse.Res1m.Floor(now.Add(-5*time.Minute)), now)
	if len(pts) != 1 {
		t.Fatalf("got %d points, want only the one inside the window", len(pts))
	}
}

func TestAggregateKeysExpireAfterRetention(t *testing.T) {
	s, mr := setup(t)
	now := time.Now()
	_ = s.AddAggregates(context.Background(), []pulse.AggDelta{
		delta("m", "k", pulse.Res10s, now, pulse.Agg{Count: 1}),
		delta("m", "k", pulse.Res1h, now, pulse.Agg{Count: 1}),
	})
	for _, key := range mr.Keys() {
		ttl := mr.TTL(key)
		switch {
		case strings.Contains(key, ":10s:"):
			if ttl < time.Hour || ttl > time.Hour+time.Minute {
				t.Errorf("%s ttl = %s, want about an hour", key, ttl)
			}
		case strings.Contains(key, ":1h:"):
			if ttl < 7*24*time.Hour || ttl > 8*24*time.Hour {
				t.Errorf("%s ttl = %s, want about a week", key, ttl)
			}
		}
		if !strings.HasPrefix(key, "gopulse:test:") {
			t.Errorf("key %q is outside the store prefix", key)
		}
	}
	mr.FastForward(2 * time.Hour)
	if left := len(mr.Keys()); left != 1 {
		t.Fatalf("%d keys left after two hours, want only the hourly bucket", left)
	}
}

func TestEntriesNewestFirstAndCapped(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	base := time.Now().Truncate(time.Millisecond)
	var entries []pulse.Entry
	for i := range 300 {
		entries = append(entries, pulse.Entry{Time: base.Add(time.Duration(i) * time.Millisecond), Data: []byte(fmt.Sprintf(`{"n":%d}`, i))})
	}
	if err := s.AppendEntries(ctx, "slow", entries, 100); err != nil {
		t.Fatal(err)
	}
	got, err := s.Entries(ctx, "slow", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || string(got[0].Data) != `{"n":299}` || string(got[4].Data) != `{"n":295}` {
		t.Fatalf("entries = %s ... %s, want the five newest, newest first", got[0].Data, got[len(got)-1].Data)
	}
	if !got[0].Time.Equal(base.Add(299 * time.Millisecond)) {
		t.Fatalf("entry time = %s, want the recorded time", got[0].Time)
	}
	all, _ := s.Entries(ctx, "slow", 1000)
	if len(all) > 200 {
		t.Fatalf("stream holds %d entries, want it trimmed near the cap of 100", len(all))
	}
	if empty, err := s.Entries(ctx, "never-written", 10); err != nil || len(empty) != 0 {
		t.Fatalf("missing list = %v, %v; want empty and no error", empty, err)
	}
}

func TestErrorsGroupOrderAndTrim(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	occ := func(fp string, minute int, sample string) pulse.ErrorOccurrence {
		return pulse.ErrorOccurrence{Fingerprint: fp, Time: base.Add(time.Duration(minute) * time.Minute), Sample: []byte(sample)}
	}
	err := s.RecordErrors(ctx, []pulse.ErrorOccurrence{
		occ("a", 1, "a1"), occ("b", 2, "b1"), occ("a", 3, "a2"), occ("c", 4, "c1"),
	}, 2)
	if err != nil {
		t.Fatal(err)
	}

	groups, err := s.Errors(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Fingerprint != "c" || groups[1].Fingerprint != "a" {
		t.Fatalf("groups = %+v, want c then a, with b trimmed", groups)
	}
	a := groups[1]
	if a.Count != 2 || string(a.Sample) != "a2" || !a.FirstSeen.Equal(base.Add(time.Minute)) || !a.LastSeen.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("group a = %+v, want count 2, latest sample, first and last sighting kept", a)
	}
	if _, ok, _ := s.Error(ctx, "b"); ok {
		t.Fatal("trimmed group b is still readable")
	}
	if g, ok, _ := s.Error(ctx, "c"); !ok || g.Count != 1 {
		t.Fatalf("Error(c) = %+v, %v", g, ok)
	}
}

func TestHostsDisappearAfterTTL(t *testing.T) {
	s, mr := setup(t)
	ctx := context.Background()
	clock := time.Now()
	s.now = func() time.Time { return clock }

	_ = s.PutHost(ctx, "web-1", []byte("one"), time.Minute)
	_ = s.PutHost(ctx, "web-2", []byte("two"), time.Hour)
	hosts, err := s.Hosts(ctx)
	if err != nil || len(hosts) != 2 || string(hosts["web-1"]) != "one" {
		t.Fatalf("hosts = %v, %v", hosts, err)
	}

	clock = clock.Add(2 * time.Minute)
	mr.FastForward(2 * time.Minute)
	hosts, _ = s.Hosts(ctx)
	if len(hosts) != 1 || string(hosts["web-2"]) != "two" {
		t.Fatalf("hosts after expiry = %v, want only web-2", hosts)
	}
}

func TestTwoInstancesShareOneDashboard(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	var instances []*pulse.Pulse
	for _, name := range []string{"web-1", "web-2"} {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		p := pulse.New(pulse.Config{
			App: "shop", Instance: name, Store: New(rdb, "shop"),
			HostInterval: -1, FlushInterval: time.Hour, SlowRequest: time.Nanosecond,
		})
		t.Cleanup(p.Close)
		instances = append(instances, p)
	}

	for i, p := range instances {
		for range i + 1 {
			_, span := p.Start(ctx, "GET", "/cart")
			span.End(pulse.Result{Path: "/cart", Status: 500})
		}
		p.Flush()
		if e := p.StoreError(); e != "" {
			t.Fatalf("store error: %s", e)
		}
	}

	reader := instances[0]
	now := time.Now()
	totals, err := reader.Totals(ctx, pulse.MetricHTTP, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if got := totals["GET /cart"].Count; got != 3 {
		t.Fatalf("requests seen through one instance = %d, want 3 from both", got)
	}
	errs, _ := reader.Errors(ctx, 10)
	if len(errs) != 1 || errs[0].Count != 3 || errs[0].Message != "HTTP 500" {
		t.Fatalf("errors = %+v, want one group counted across instances", errs)
	}
	slow, _ := reader.SlowRequests(ctx, 10)
	if len(slow) != 3 {
		t.Fatalf("slow requests = %d, want 3", len(slow))
	}
}

func TestUnreachableRedisIsReportedNotFatal(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	p := pulse.New(pulse.Config{Store: New(rdb, "down"), HostInterval: -1, FlushInterval: time.Hour})
	t.Cleanup(p.Close)

	mr.Close()
	p.Record("m", "k", 1)
	p.Flush()
	if p.StoreError() == "" {
		t.Fatal("a failed write should be surfaced through StoreError")
	}
}
