// Package pulsegin connects pulse to the Gin web framework.
package pulsegin

import (
	"runtime/debug"

	"github.com/gin-gonic/gin"

	pulse "github.com/nicklasos/gopulse"
)

// Middleware records every request handled by the engine.
//
// Register it after the application's recovery middleware: a panic is
// recorded with its stack and then re-raised so that recovery still writes
// the response.
func Middleware(p *pulse.Pulse) gin.HandlerFunc {
	return func(c *gin.Context) {
		if p.OwnsPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		route := c.FullPath()
		if route == "" {
			route = pulse.Unmatched
		}
		ctx, span := p.Start(c.Request.Context(), c.Request.Method, route)
		c.Request = c.Request.WithContext(ctx)

		defer func() {
			if v := recover(); v != nil {
				span.Panic(v, debug.Stack())
				span.End(pulse.Result{Path: c.Request.URL.Path, Status: 500})
				panic(v)
			}
		}()

		c.Next()

		errs := make([]error, 0, len(c.Errors))
		for _, e := range c.Errors {
			errs = append(errs, e.Err)
		}
		span.End(pulse.Result{Path: c.Request.URL.Path, Status: c.Writer.Status(), Errors: errs})
	}
}

// Mount serves the dashboard under p.Path() on the given router.
func Mount(r gin.IRoutes, p *pulse.Pulse) {
	h := gin.WrapH(p.Handler())
	r.Any(p.Path(), h)
	r.Any(p.Path()+"/*any", h)
}
