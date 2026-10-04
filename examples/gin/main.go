// Demo application: run it, then open http://localhost:8099/_pulse
// (login "admin", password "secret"). It generates its own traffic.
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	pulse "github.com/nicklasos/gopulse"
	"github.com/nicklasos/gopulse/pulsegin"
)

func main() {
	store := pulse.NewMemoryStore()
	seed(store)
	p := pulse.New(pulse.Config{
		App:         "demo",
		Instance:    "web-1",
		Username:    "admin",
		Password:    "secret",
		Store:       store,
		SlowRequest: 200 * time.Millisecond,
	})
	defer p.Close()

	p.AddPage("Business",
		pulse.Card{Title: "Orders", Render: func(ctx context.Context, v pulse.View) (pulse.Widget, error) {
			orders, err := p.Total(ctx, "orders.amount", v.From, v.To)
			if err != nil {
				return nil, err
			}
			return pulse.Stats{
				{Label: "Orders", Value: pulse.FormatCount(float64(orders.Count)), Hint: "last " + v.Period},
				{Label: "Revenue", Value: "$" + pulse.FormatCount(orders.Sum)},
				{Label: "Average order", Value: "$" + pulse.FormatCount(orders.Avg())},
			}, nil
		}},
		pulse.Card{Title: "Revenue per interval", Width: pulse.Half, Render: func(ctx context.Context, v pulse.View) (pulse.Widget, error) {
			series, err := p.Series(ctx, "orders.amount", "created", v.From, v.To, 60)
			if err != nil {
				return nil, err
			}
			line := pulse.Line{Name: "Revenue"}
			for _, pt := range series.Points {
				line.Points = append(line.Points, pulse.Point{T: pt.Time, V: pt.Sum})
			}
			return pulse.TimeSeries{Lines: []pulse.Line{line}, Unit: "USD", Bars: true}, nil
		}},
		pulse.Card{Title: "Queues", Width: pulse.Half, Render: func(context.Context, pulse.View) (pulse.Widget, error) {
			return pulse.Table{
				Columns: []string{"Queue", "Waiting", "Capacity used"},
				Rows: [][]any{
					{"emails", pulse.Num("12"), pulse.Cell{Text: "24%", Meter: pulse.Percent(24)}},
					{"exports", pulse.Num("3"), pulse.Cell{Text: "81%", Meter: pulse.Percent(81)}},
					{"webhooks", pulse.Num("0"), pulse.Cell{Text: "2%", Meter: pulse.Percent(2)}},
				},
			}, nil
		}},
	)

	logger := slog.New(p.SlogHandler(slog.NewTextHandler(io.Discard, nil)))
	// query stands in for a database call; a real application attaches
	// pulsepgx.New(p) to its pgx pool instead.
	query := func(c *gin.Context, sql string, base int) {
		d := time.Duration(base+rand.IntN(base)) * time.Millisecond
		time.Sleep(d)
		p.RecordQuery(c.Request.Context(), sql, d, nil)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(pulsegin.Middleware(p))
	pulsegin.Mount(r, p)

	r.GET("/users", func(c *gin.Context) {
		query(c, "-- name: ListUsers :many\nSELECT id, name FROM users ORDER BY id LIMIT $1", 12)
		c.JSON(http.StatusOK, gin.H{"users": []string{}})
	})
	r.GET("/users/:id", func(c *gin.Context) {
		query(c, "-- name: GetUser :one\nSELECT id, name FROM users WHERE id = $1", 3)
		if rand.IntN(10) == 0 {
			logger.WarnContext(c.Request.Context(), "user not found", "id", c.Param("id"))
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id")})
	})
	r.GET("/reports", func(c *gin.Context) {
		query(c, "-- name: ListOrders :many\nSELECT * FROM orders WHERE created_at > $1", 20)
		query(c, "SELECT date_trunc('day', created_at) AS day, sum(total) FROM orders WHERE created_at > $1 GROUP BY 1", 120)
		logger.InfoContext(c.Request.Context(), "report built", "rows", 100+rand.IntN(900))
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.POST("/orders", func(c *gin.Context) {
		query(c, "-- name: CreateOrder :one\nINSERT INTO orders (user_id, total) VALUES ($1, $2) RETURNING id", 15)
		p.Record("orders.amount", "created", float64(10+rand.IntN(90)))
		if rand.IntN(15) == 0 {
			logger.ErrorContext(c.Request.Context(), "payment failed", "gateway", "acme", "attempt", 3)
			_ = c.Error(errors.New("payment gateway timeout"))
			c.JSON(http.StatusBadGateway, gin.H{"error": "gateway"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"ok": true})
	})
	r.GET("/panic", func(c *gin.Context) {
		var m map[string]int
		m["boom"] = 1
	})

	go traffic("http://localhost:8099")
	log.Println("demo listening on http://localhost:8099 — dashboard at /_pulse (admin / secret)")
	log.Fatal(r.Run(":8099"))
}

func traffic(base string) {
	time.Sleep(500 * time.Millisecond)
	client := &http.Client{Timeout: 5 * time.Second}
	hit := func(method, path string) {
		req, _ := http.NewRequest(method, base+path, nil)
		if res, err := client.Do(req); err == nil {
			res.Body.Close()
		}
	}
	for {
		switch n := rand.IntN(100); {
		case n < 45:
			go hit("GET", "/users")
		case n < 75:
			go hit("GET", "/users/42")
		case n < 85:
			go hit("GET", "/reports")
		case n < 97:
			go hit("POST", "/orders")
		case n < 99:
			go hit("GET", "/nope")
		default:
			go hit("GET", "/panic")
		}
		time.Sleep(time.Duration(20+rand.IntN(120)) * time.Millisecond)
	}
}

// seed backfills an hour of synthetic history so the charts are not empty on
// first load.
func seed(store pulse.Store) {
	routes := []struct {
		key    string
		perMin float64
		ms     float64
	}{
		{"GET /users", 330, 25},
		{"GET /users/:id", 220, 9},
		{"GET /reports", 75, 340},
		{"POST /orders", 90, 55},
	}
	merged := map[pulse.AggKey]*pulse.Agg{}
	add := func(metric, key string, t time.Time, a pulse.Agg) {
		for _, res := range pulse.Resolutions {
			k := pulse.AggKey{Metric: metric, Key: key, Res: res.Name, Start: res.Floor(t).Unix()}
			if merged[k] == nil {
				merged[k] = &pulse.Agg{}
			}
			merged[k].Merge(a)
		}
	}
	bucketOf := func(ms float64) int {
		for i, b := range pulse.HistBounds {
			if ms <= b {
				return i
			}
		}
		return len(pulse.HistBounds)
	}

	now := time.Now()
	for t := now.Add(-time.Hour); t.Before(now.Add(-10 * time.Second)); t = t.Add(10 * time.Second) {
		wave := 1 + 0.35*float64(t.Unix()%900)/900
		for _, r := range routes {
			a := pulse.Agg{Hist: make([]int64, len(pulse.HistBounds)+1)}
			for range int(r.perMin / 6 * wave * (0.8 + 0.4*rand.Float64())) {
				ms := r.ms * (0.4 + 1.2*rand.Float64())
				if rand.IntN(25) == 0 {
					ms *= 3
				}
				a.Count++
				a.Sum += ms
				a.Hist[bucketOf(ms)]++
			}
			add(pulse.MetricHTTP, r.key, t, a)
		}
		for name, ms := range map[string]float64{"ListUsers": 18, "GetUser": 4.5, "ListOrders": 30, "CreateOrder": 22} {
			a := pulse.Agg{Hist: make([]int64, len(pulse.HistBounds)+1)}
			for range int(20 * wave) {
				v := ms * (0.5 + rand.Float64())
				a.Count++
				a.Sum += v
				a.Hist[bucketOf(v)]++
			}
			add(pulse.MetricQuery, name, t, a)
		}
		for range int(14 * wave) {
			add("orders.amount", "created", t, pulse.Agg{Count: 1, Sum: float64(10 + rand.IntN(90))})
		}
		if rand.IntN(4) == 0 {
			add(pulse.MetricHTTP4xx, "GET /users/:id", t, pulse.Agg{Count: 2, Sum: 2})
		}
		if rand.IntN(12) == 0 {
			add(pulse.MetricHTTP5xx, "POST /orders", t, pulse.Agg{Count: 1, Sum: 1})
		}
		gauges := map[string]float64{
			pulse.MetricHostCPU:        18 + 14*rand.Float64()*wave,
			pulse.MetricHostMem:        61 + 3*rand.Float64(),
			pulse.MetricHostLoad:       1.2 + 0.8*rand.Float64()*wave,
			pulse.MetricHostGoroutines: float64(24 + rand.IntN(14)),
		}
		for metric, v := range gauges {
			add(metric, "web-1", t, pulse.Agg{Count: 1, Sum: v})
		}
	}

	deltas := make([]pulse.AggDelta, 0, len(merged))
	for k, a := range merged {
		deltas = append(deltas, pulse.AggDelta{AggKey: k, Agg: *a})
	}
	_ = store.AddAggregates(context.Background(), deltas)
}
