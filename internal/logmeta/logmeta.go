// Package logmeta holds the request-scoped log metadata map. It lives here
// rather than in the elephantine root package so that both the root package
// and the rpc package can write to the same map: the root package uses the rpc
// package's interceptors, so rpc cannot import it back.
package logmeta

import (
	"context"
	"maps"
	"sync"
)

type ctxKey int

const logCtxKey ctxKey = 1

// metadata is the request-scoped log metadata map and the mutex that guards
// it.
//
// The lock is what makes the map safe for a streaming handler, which is
// normally more than one goroutine: a producer goroutine and the one writing
// to the stream both calling Set is a concurrent map write, and that kills the
// process rather than logging something odd. A unary handler is one goroutine
// and never raced, but the map is written a handful of times per request and
// read once, so the lock costs it nothing.
type metadata struct {
	mu     sync.Mutex
	values map[string]any
}

// With creates a child context with a log metadata map.
func With(ctx context.Context) context.Context {
	m := &metadata{values: make(map[string]any)}

	return context.WithValue(ctx, logCtxKey, m)
}

// Get returns a copy of the log metadata map for the context, or nil if it has
// none. It is a copy so that a caller ranging over it cannot race a handler
// goroutine that is still setting values.
func Get(ctx context.Context) map[string]any {
	m, ok := ctx.Value(logCtxKey).(*metadata)
	if !ok {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return maps.Clone(m.values)
}

// Set sets a log metadata value on the context if it has a log metadata map.
func Set(ctx context.Context, key string, value any) {
	m, ok := ctx.Value(logCtxKey).(*metadata)
	if !ok {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.values[key] = value
}
