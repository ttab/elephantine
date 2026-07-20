package test

import "context"

// Cleaner is the subset of testing.TB used to register cleanup callbacks,
// satisfied by *testing.T.
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
