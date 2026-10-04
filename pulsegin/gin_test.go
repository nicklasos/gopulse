package pulsegin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	pulse "github.com/nicklasos/gopulse"
)

func setup(t *testing.T) (*pulse.Pulse, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	p := pulse.New(pulse.Config{
		Username: "admin", Password: "secret",
		HostInterval: -1, FlushInterval: time.Hour,
	})
	t.Cleanup(p.Close)

	r := gin.New()
	r.RedirectTrailingSlash = false
	r.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "recovered by app"})
	}))
	r.Use(Middleware(p))
	Mount(r, p)
	return p, r
}

func do(r *gin.Engine, method, path string, auth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if auth {
		req.SetBasicAuth("admin", "secret")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func totals(t *testing.T, p *pulse.Pulse, metric string) map[string]pulse.Agg {
	t.Helper()
	p.Flush()
	now := time.Now()
	out, err := p.Totals(context.Background(), metric, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecordsRouteTemplateNotRawPath(t *testing.T) {
	p, r := setup(t)
	r.GET("/users/:id", func(c *gin.Context) { c.Status(http.StatusOK) })

	do(r, "GET", "/users/1", false)
	do(r, "GET", "/users/2?token=abc", false)
	do(r, "GET", "/wp-login.php", false)
	do(r, "GET", "/.env", false)

	got := totals(t, p, pulse.MetricHTTP)
	if got["GET /users/:id"].Count != 2 {
		t.Errorf("template route count = %d, want 2", got["GET /users/:id"].Count)
	}
	if got["GET "+pulse.Unmatched].Count != 2 || len(got) != 2 {
		t.Errorf("routes = %v, want unknown paths folded into one key", got)
	}
}

func TestHandlerContextCarriesSpan(t *testing.T) {
	_, r := setup(t)
	var route string
	r.GET("/ctx/:id", func(c *gin.Context) {
		if s := pulse.SpanFromContext(c.Request.Context()); s != nil {
			route = s.Route
		}
	})
	do(r, "GET", "/ctx/5", false)
	if route != "/ctx/:id" {
		t.Fatalf("span route = %q", route)
	}
}

func TestGinErrorsAndStatusOnlyFailures(t *testing.T) {
	p, r := setup(t)
	r.GET("/attached", func(c *gin.Context) {
		_ = c.Error(errors.New("db unavailable"))
		c.JSON(http.StatusInternalServerError, gin.H{})
	})
	r.GET("/bare", func(c *gin.Context) { c.JSON(http.StatusBadGateway, gin.H{}) })
	r.GET("/client", func(c *gin.Context) { c.JSON(http.StatusBadRequest, gin.H{}) })

	do(r, "GET", "/attached", false)
	do(r, "GET", "/bare", false)
	do(r, "GET", "/client", false)
	p.Flush()

	errs, _ := p.Errors(context.Background(), 10)
	messages := map[string]bool{}
	for _, e := range errs {
		messages[e.Message] = true
	}
	if len(errs) != 2 || !messages["db unavailable"] || !messages["HTTP 502"] {
		t.Fatalf("errors = %v, want the attached error and the bare 502 only", messages)
	}
}

func TestPanicIsRecordedAndAppRecoveryStillResponds(t *testing.T) {
	p, r := setup(t)
	r.GET("/panic", func(c *gin.Context) { panic("exploded") })

	rec := do(r, "GET", "/panic", false)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "recovered by app") {
		t.Fatalf("response = %d %s, want the app's own recovery response", rec.Code, rec.Body)
	}

	if got := totals(t, p, pulse.MetricHTTP5xx)["GET /panic"].Count; got != 1 {
		t.Errorf("5xx count = %d, want 1", got)
	}
	errs, _ := p.Errors(context.Background(), 10)
	if len(errs) != 1 || errs[0].Kind != "panic" || errs[0].Message != "exploded" {
		t.Fatalf("errors = %+v, want one panic", errs)
	}
	if !strings.Contains(errs[0].Stack, "gin_test.go") {
		t.Error("stack does not point at the panicking handler")
	}
}

func TestDashboardMountedProtectedAndNotRecorded(t *testing.T) {
	p, r := setup(t)

	for _, path := range []string{"/_pulse", "/_pulse/", "/_pulse/routes"} {
		if rec := do(r, "GET", path, false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials = %d, want 401", path, rec.Code)
		}
		if rec := do(r, "GET", path, true); rec.Code != http.StatusOK {
			t.Errorf("%s with credentials = %d, want 200", path, rec.Code)
		}
	}
	if rec := do(r, "GET", "/_pulse/_assets/app.css", true); rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Errorf("stylesheet = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if got := totals(t, p, pulse.MetricHTTP); len(got) != 0 {
		t.Errorf("dashboard requests were recorded: %v", got)
	}
}
