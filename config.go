package pulse

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config controls recording, storage and dashboard access.
type Config struct {
	// App names the service. It namespaces stored data and is shown in the dashboard header.
	App string

	// Path is the URL prefix the dashboard is mounted at. Default "/_pulse".
	Path string

	// Username and Password protect the dashboard with HTTP basic auth.
	// An empty Password disables the dashboard unless Authorize is set.
	Username string
	Password string

	// Authorize, when set, replaces basic auth.
	Authorize func(*http.Request) bool

	// Store persists aggregates and entries. Default is an in-process MemoryStore.
	Store Store

	// SlowRequest and SlowQuery are the thresholds above which a single
	// request or query is kept as an individual entry.
	SlowRequest time.Duration
	SlowQuery   time.Duration

	// FlushInterval is how often buffered data is written to the Store.
	FlushInterval time.Duration

	// HostInterval is how often host statistics are sampled. Negative disables sampling.
	HostInterval time.Duration

	// DiskPaths lists the mount points reported on the Server page. Default "/".
	DiskPaths []string

	// BufferSize is the capacity of the event queue. Events are dropped when it is full.
	BufferSize int

	// MaxEntries caps each stored list (slow requests, slow queries, logs, errors).
	MaxEntries int

	// LogLevel is the minimum level captured by SlogHandler.
	LogLevel slog.Level

	// Instance identifies this process on the Server page. Default is the hostname.
	Instance string
}

func (c Config) withDefaults() Config {
	if c.App == "" {
		c.App = "app"
	}
	if c.Path == "" {
		c.Path = "/_pulse"
	}
	c.Path = "/" + strings.Trim(c.Path, "/")
	if c.Store == nil {
		c.Store = NewMemoryStore()
	}
	if c.SlowRequest == 0 {
		c.SlowRequest = 500 * time.Millisecond
	}
	if c.SlowQuery == 0 {
		c.SlowQuery = 100 * time.Millisecond
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = 5 * time.Second
	}
	if c.HostInterval == 0 {
		c.HostInterval = 15 * time.Second
	}
	if len(c.DiskPaths) == 0 {
		c.DiskPaths = []string{"/"}
	}
	if c.BufferSize == 0 {
		c.BufferSize = 4096
	}
	if c.MaxEntries == 0 {
		c.MaxEntries = 200
	}
	if c.Instance == "" {
		c.Instance, _ = os.Hostname()
		if c.Instance == "" {
			c.Instance = "unknown"
		}
	}
	return c
}
