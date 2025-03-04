package test

import "context"

type Cleaner interface {
	Cleanup(fn func())
}

// Deprecated: Use testing.T.Context() instead.
func Context(c Cleaner) context.Context {
	ctx, cancel := context.WithCancel(context.Background())

	c.Cleanup(func() {
		cancel()
	})

	return ctx
}
