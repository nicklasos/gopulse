package pulse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

//go:embed ui/*.html ui/app.css ui/app.js
var uiFS embed.FS

var (
	assetCSS, _  = uiFS.ReadFile("ui/app.css")
	assetJS, _   = uiFS.ReadFile("ui/app.js")
	assetVersion = func() string {
		sum := sha256.Sum256(append(append([]byte{}, assetCSS...), assetJS...))
		return hex.EncodeToString(sum[:6])
	}()
)

func parseTemplates() *template.Template {
	funcs := template.FuncMap{
		"meterValue": func(v *float64) float64 { return min(max(*v, 0), 100) },
		"meterClass": func(v *float64) string {
			switch {
			case *v >= 90:
				return "bad"
			case *v >= 75:
				return "warn"
			default:
				return ""
			}
		},
	}
	return template.Must(template.New("ui").Funcs(funcs).ParseFS(uiFS, "ui/*.html"))
}

type period struct {
	Name string
	Span time.Duration
}

var periods = []period{
	{"1h", time.Hour},
	{"6h", 6 * time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
}

type navItem struct {
	Title  string
	URL    string
	Active bool
}

type pageData struct {
	App          string
	Title        string
	Base         string
	AssetVersion string
	Pages        []navItem
	Periods      []navItem
	Cards        []renderedCard
	StoreError   string
	Dropped      int64
}

// Handler serves the dashboard. Mount it so that it receives every request
// under Path, including Path itself.
func (p *Pulse) Handler() http.Handler {
	return http.HandlerFunc(p.serveDashboard)
}

func (p *Pulse) serveDashboard(w http.ResponseWriter, r *http.Request) {
	if !p.authorize(w, r) {
		return
	}
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, p.cfg.Path), "/")
	switch rel {
	case "_assets/app.css":
		serveAsset(w, "text/css; charset=utf-8", assetCSS)
		return
	case "_assets/app.js":
		serveAsset(w, "text/javascript; charset=utf-8", assetJS)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	page := p.page(rel)
	if page == nil {
		http.NotFound(w, r)
		return
	}

	query := r.URL.Query()
	sel := periods[0]
	for _, c := range periods {
		if c.Name == query.Get("period") {
			sel = c
		}
	}
	now := time.Now()
	view := View{From: now.Add(-sel.Span), To: now, Period: sel.Name, Params: query, base: p.cfg.Path}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ctx = withReadCache(ctx)

	data := pageData{
		App:          p.cfg.App,
		Title:        page.Title,
		Base:         p.cfg.Path,
		AssetVersion: assetVersion,
		Cards:        p.renderCards(ctx, page, view),
		StoreError:   p.StoreError(),
		Dropped:      p.Dropped(),
	}

	active := page.Slug
	if page.Hidden && page.Parent != "" {
		active = page.Parent
	}
	p.mu.RLock()
	for _, pg := range p.pages {
		if pg.Hidden {
			continue
		}
		data.Pages = append(data.Pages, navItem{Title: pg.Title, URL: view.PageURL(pg.Slug), Active: pg.Slug == active})
	}
	p.mu.RUnlock()
	for _, c := range periods {
		q := url.Values{}
		for k, v := range query {
			if k != "_fragment" {
				q[k] = v
			}
		}
		q.Set("period", c.Name)
		data.Periods = append(data.Periods, navItem{
			Title:  c.Name,
			URL:    p.cfg.Path + "/" + page.Slug + "?" + q.Encode(),
			Active: c.Name == sel.Name,
		})
	}

	name := "layout"
	if query.Get("_fragment") != "" {
		name = "cards"
	}
	var buf bytes.Buffer
	if err := p.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "render failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	_, _ = w.Write(buf.Bytes())
}

func serveAsset(w http.ResponseWriter, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	_, _ = w.Write(body)
}

func (p *Pulse) authorize(w http.ResponseWriter, r *http.Request) bool {
	if p.cfg.Authorize != nil {
		if p.cfg.Authorize(r) {
			return true
		}
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	if p.cfg.Password == "" {
		http.NotFound(w, r)
		return false
	}
	user, pass, ok := r.BasicAuth()
	if ok && secureEqual(user, p.cfg.Username) && secureEqual(pass, p.cfg.Password) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="gopulse", charset="UTF-8"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func secureEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// OwnsPath reports whether a URL path belongs to the dashboard. Adapters use
// it to keep the dashboard's own requests out of the statistics.
func (p *Pulse) OwnsPath(path string) bool {
	return path == p.cfg.Path || strings.HasPrefix(path, p.cfg.Path+"/")
}

// Middleware records requests for net/http routers. The route is taken from
// the pattern matched by http.ServeMux.
func (p *Pulse) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.OwnsPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, span := p.Start(r.Context(), r.Method, Unmatched)
		r = r.WithContext(ctx)
		sw := &statusWriter{ResponseWriter: w}
		result := func(status int) Result {
			route := Unmatched
			if _, pattern, ok := strings.Cut(r.Pattern, " "); ok {
				route = pattern
			} else if r.Pattern != "" {
				route = r.Pattern
			}
			return Result{Route: route, Path: r.URL.Path, Status: status, ClientGone: errors.Is(ctx.Err(), context.Canceled)}
		}
		defer func() {
			if v := recover(); v != nil {
				res := result(http.StatusInternalServerError)
				span.Route = res.Route
				span.Panic(v, debug.Stack())
				span.End(res)
				panic(v)
			}
		}()
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		span.End(result(sw.status))
	})
}
