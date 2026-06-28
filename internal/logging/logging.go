package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// F is a single structured log field. Construct fields with Field(); every field
// passes through the redaction guard before it is emitted.
type F struct {
	Key   string
	Value any
}

// Field constructs a structured field. The value is scrubbed at emit time if the
// key indicates content/PII (see redact.go).
func Field(key string, value any) F { return F{Key: key, Value: value} }

// Logger emits structured observability events. It wraps log/slog and routes
// every field through the redaction guard. The zero value is not usable; build
// one with New().
type Logger struct {
	sl        *slog.Logger
	component string
}

// Options configures a Logger.
type Options struct {
	// Component is a short process/subsystem tag (e.g. "core", "media", "firewall").
	Component string
	// Writer is the log sink; defaults to os.Stderr.
	Writer io.Writer
	// Level is the minimum level; defaults to Info.
	Level slog.Level
	// JSON selects JSON output (default) vs text. JSON is the appliance default.
	JSON bool
}

// New builds a Logger. By default it writes JSON to stderr at Info level.
func New(opts Options) *Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	handlerOpts := &slog.HandlerOptions{Level: opts.Level}
	var h slog.Handler
	if opts.JSON || opts.Writer == nil {
		h = slog.NewJSONHandler(w, handlerOpts)
	} else {
		h = slog.NewTextHandler(w, handlerOpts)
	}
	comp := opts.Component
	if comp == "" {
		comp = "captionbridge"
	}
	return &Logger{sl: slog.New(h), component: comp}
}

// Nop returns a Logger that discards everything; useful in tests.
func Nop() *Logger { return New(Options{Writer: io.Discard, Component: "test"}) }

// emit writes an event at the given level with scrubbed fields.
func (l *Logger) emit(level slog.Level, ev Event, fields ...F) {
	attrs := make([]any, 0, len(fields)*2+2)
	attrs = append(attrs, slog.String("event", string(ev)), slog.String("component", l.component))
	for _, f := range fields {
		attrs = append(attrs, slog.Any(f.Key, scrub(f.Key, f.Value)))
	}
	l.sl.Log(context.Background(), level, string(ev), attrs...)
}

// Info emits an informational event.
func (l *Logger) Info(ev Event, fields ...F) { l.emit(slog.LevelInfo, ev, fields...) }

// Warn emits a warning event (e.g. lease expiring, clipping).
func (l *Logger) Warn(ev Event, fields ...F) { l.emit(slog.LevelWarn, ev, fields...) }

// Error emits an error-class event (state, never content).
func (l *Logger) Error(ev Event, fields ...F) { l.emit(slog.LevelError, ev, fields...) }

// Debug emits a debug event.
func (l *Logger) Debug(ev Event, fields ...F) { l.emit(slog.LevelDebug, ev, fields...) }

// With returns a child logger that tags a different component.
func (l *Logger) With(component string) *Logger {
	return &Logger{sl: l.sl, component: component}
}
