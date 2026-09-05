// Package logmeta holds the request-scoped log metadata map. It lives here
// rather than in the elephantine root package so that both the root package
// and the rpc package can write to the same map: the root package uses the rpc
// package's interceptors, so rpc cannot import it back.
package logmeta

import "context"

type ctxKey int

const logCtxKey ctxKey = 1

// With creates a child context with a log metadata map.
func With(ctx context.Context) context.Context {
	m := make(map[string]any)

	return context.WithValue(ctx, logCtxKey, m)
}

// Get returns the log metadata map for the context, or nil if it has none.
func Get(ctx context.Context) map[string]any {
	m, ok := ctx.Value(logCtxKey).(map[string]any)
	if !ok {
		return nil
	}

	return m
}

// Set sets a log metadata value on the context if it has a log metadata map.
func Set(ctx context.Context, key string, value any) {
	m, ok := ctx.Value(logCtxKey).(map[string]any)
	if !ok {
		return
	}

	m[key] = value
}
