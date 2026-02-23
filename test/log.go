package test

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// TestingLogger is satisfied by *testing.T and *testing.B.
type TestingLogger interface {
	Log(args ...any)
	Cleanup(fn func())
}

// Logger is the previous interface for NewLogHandler. Use TestingLogger
// instead.
//
// Deprecated: use TestingLogger.
type Logger = TestingLogger

func NewLogHandler(t TestingLogger, level slog.Level) slog.Handler {
	h := LogHandler{
		t: t,
	}

	h.handler = slog.NewTextHandler(&h, &slog.HandlerOptions{
		Level: level,
	})

	t.Cleanup(func() {
		h.done.Store(true)
	})

	return &h
}

type LogHandler struct {
	t       TestingLogger
	handler *slog.TextHandler
	done    atomic.Bool
}

func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.done.Load() {
		return nil
	}

	return h.handler.Handle(ctx, r) //nolint:wrapcheck
}

func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.handler.WithAttrs(attrs)
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	return h.handler.WithGroup(name)
}

func (h *LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.done.Load() {
		return false
	}

	return h.handler.Enabled(ctx, level)
}

func (h *LogHandler) Write(data []byte) (int, error) {
	if h.done.Load() {
		return len(data), nil
	}

	h.t.Log(string(data))

	return len(data), nil
}
