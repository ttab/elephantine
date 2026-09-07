package test

import (
	"context"
	"log/slog"
	"sync"
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

// NewLogHandler returns a slog.Handler that logs through t. Once t's cleanup
// has run the handler silently drops further records: the cleanup and the
// write into t.Log are serialised, so a record either reaches t.Log before the
// cleanup returns, while logging is still legal, or is dropped.
func NewLogHandler(t TestingLogger, level slog.Level) slog.Handler {
	h := LogHandler{
		t: t,
	}

	h.handler = slog.NewTextHandler(&h, &slog.HandlerOptions{
		Level: level,
	})

	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()

		h.done = true
	})

	return &h
}

// LogHandler is a slog.Handler that writes log records through a
// TestingLogger's Log method, so log output is attributed to the running test.
// Once the test's cleanup has run it silently drops further records. Create it
// with NewLogHandler.
//
// Every record, including those from handlers derived with WithAttrs and
// WithGroup, is written through Write, which is where the guard lives.
type LogHandler struct {
	t       TestingLogger
	handler *slog.TextHandler

	// mu serialises Write against the cleanup that stops it, and guards
	// done. The cleanup cannot return while a write is in flight, so a
	// write that has passed the done check is guaranteed to reach t.Log
	// before the test is marked complete.
	mu   sync.Mutex
	done bool
}

func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.handler.Handle(ctx, r) //nolint:wrapcheck
}

func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.handler.WithAttrs(attrs)
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	return h.handler.WithGroup(name)
}

func (h *LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

// Write is the io.Writer the TextHandler and all its clones write to. It must
// not be called while holding h.mu.
func (h *LogHandler) Write(data []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.done {
		return len(data), nil
	}

	h.t.Log(string(data))

	return len(data), nil
}
