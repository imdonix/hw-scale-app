// Package logx provides a compact human-readable slog handler.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type handler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Level
	attrs []slog.Attr
}

// New returns a logger writing "2006-01-02 15:04:05 LEVEL msg k=v" lines.
func New(w io.Writer, level string) *slog.Logger {
	return slog.New(&handler{mu: &sync.Mutex{}, w: w, level: ParseLevel(level)})
}

// ParseLevel maps debug/info/warn/error to a slog level.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug", "trace":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format(time.DateTime))
	b.WriteByte(' ')
	lvl := r.Level.String()
	b.WriteString(lvl)
	b.WriteString(strings.Repeat(" ", max(1, 6-len(lvl))))
	b.WriteString(r.Message)
	write := func(a slog.Attr) bool {
		v := a.Value.Resolve()
		s := v.String()
		if v.Kind() == slog.KindTime {
			s = v.Time().Format(time.DateTime)
		}
		if strings.ContainsAny(s, " \t\"") {
			s = fmt.Sprintf("%q", s)
		}
		b.WriteString(" " + a.Key + "=" + s)
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &n
}

func (h *handler) WithGroup(string) slog.Handler { return h }
