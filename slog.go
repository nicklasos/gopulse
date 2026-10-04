package pulse

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// ListLogs holds the most recent captured log records.
const ListLogs = "logs"

const maxLogAttrLen = 1500

// LogRecord is one captured log line.
type LogRecord struct {
	Time      time.Time `json:"time"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Attrs     string    `json:"attrs,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	Method    string    `json:"method,omitempty"`
	Route     string    `json:"route,omitempty"`
}

type slogHandler struct {
	p      *Pulse
	next   slog.Handler
	attrs  string
	groups string
}

// SlogHandler wraps a slog.Handler. Records at or above Config.LogLevel are
// kept for the Logs page; every record still reaches next unchanged.
func (p *Pulse) SlogHandler(next slog.Handler) slog.Handler {
	return &slogHandler{p: p, next: next}
}

func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.p.cfg.LogLevel || h.next.Enabled(ctx, level)
}

func (h *slogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= h.p.cfg.LogLevel {
		var b strings.Builder
		b.WriteString(h.attrs)
		r.Attrs(func(a slog.Attr) bool {
			appendAttr(&b, h.groups, a)
			return b.Len() < maxLogAttrLen
		})
		rec := LogRecord{
			Time:    r.Time,
			Level:   r.Level.String(),
			Message: r.Message,
			Attrs:   truncate(strings.TrimSpace(b.String()), maxLogAttrLen),
		}
		if rec.Time.IsZero() {
			rec.Time = time.Now()
		}
		if span := SpanFromContext(ctx); span != nil {
			rec.RequestID, rec.Method, rec.Route = span.ID, span.Method, span.Route
		}
		h.p.emit(entryEvent{list: ListLogs, t: rec.Time, data: rec})
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range attrs {
		appendAttr(&b, h.groups, a)
	}
	return &slogHandler{p: h.p, next: h.next.WithAttrs(attrs), attrs: b.String(), groups: h.groups}
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &slogHandler{p: h.p, next: h.next.WithGroup(name), attrs: h.attrs, groups: h.groups + name + "."}
}

func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, ga := range v.Group() {
			appendAttr(b, prefix, ga)
		}
		return
	}
	if a.Key == "" {
		return
	}
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	s := v.String()
	if strings.ContainsAny(s, " \t\n\"") {
		s = `"` + strings.NewReplacer("\n", `\n`, `"`, `\"`).Replace(s) + `"`
	}
	b.WriteString(s)
	b.WriteByte(' ')
}

// Logs returns the most recent captured log records, newest first.
func (p *Pulse) Logs(ctx context.Context, limit int) ([]LogRecord, error) {
	return readEntries[LogRecord](ctx, p, ListLogs, limit)
}
