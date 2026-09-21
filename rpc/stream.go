package rpc

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/ttab/elephantine/internal/auth"
)

// ErrDraining is the cause a streaming handler's context is cancelled with
// when the server has started shutting down. A stream that ends because of it
// is answered with connect.CodeUnavailable, so that the client reconnects
// rather than believing it cancelled the call itself.
var ErrDraining = errors.New("the server is shutting down")

// ErrTokenExpired is the cause a context from ContextWithTokenExpiry is
// cancelled with when the caller's token has expired. A stream that ends
// because of it is answered with connect.CodeUnauthenticated, so that the
// client reconnects with a fresh token rather than treating it as a server
// fault.
var ErrTokenExpired = errors.New("the caller's token has expired")

// DrainInterceptor ends the streaming calls it wraps when drain is closed, and
// answers them connect.CodeUnavailable.
//
// It exists because http.Server.Shutdown waits for in-flight requests without
// cancelling their contexts: a streaming handler blocked on a channel learns
// nothing about a shutdown, holds Shutdown open until its deadline, and then
// has the connection closed underneath it. The client sees a truncated stream
// with no code and the handler's cleanup never runs. The drain reaches the
// handler as the thing it already watches — its context, cancelled with a
// cause of ErrDraining.
//
// Unary handlers pass through untouched. They are short, and Shutdown waits
// for them correctly as it is.
//
// A handler still has to select on ctx.Done() between sends for any of this to
// do anything, which is the ordinary requirement for a streaming handler.
//
// The interceptor is also what turns the cancellation ContextWithTokenExpiry
// installs into connect.CodeUnauthenticated, so a service that wants that
// translation installs it whether or not it cares about the drain.
// elephantine.NewDefaultServiceOptions installs it, and the APIServer closes
// the channel before it shuts its listeners down.
func DrainInterceptor(drain <-chan struct{}) connect.Interceptor {
	return interceptor{
		streamingHandler: func(
			next connect.StreamingHandlerFunc,
		) connect.StreamingHandlerFunc {
			return func(
				ctx context.Context, conn connect.StreamingHandlerConn,
			) error {
				ctx, state := withStreamState(ctx)

				ctx, cancel := context.WithCancelCause(ctx)
				defer cancel(nil)

				go func() {
					select {
					case <-drain:
						state.draining()
						cancel(ErrDraining)
					case <-ctx.Done():
					}
				}()

				return endOfStreamError(state, next(ctx, conn))
			}
		},
	}
}

// ContextWithTokenExpiry derives a context that is cancelled when the caller's
// token expires, with a cause of ErrTokenExpired. A stream that ends because
// of it is answered connect.CodeUnauthenticated by DrainInterceptor, whichever
// of the cause and the bare cancellation the handler returns.
//
// It is a helper a streaming handler calls, not a default an interceptor
// imposes: a stream of public metadata and a stream that carries document
// content are not the same risk, so whether a stream may outlive the token
// that opened it is that stream's decision.
//
//	ctx, cancel := rpc.ContextWithTokenExpiry(ctx)
//	defer cancel()
//
// There is no grace period: exp is exp, and a client that cuts it fine
// reconnects. The guarantee is that the stream's exposure is bounded by the
// token lifetime; revocation before expiry is not covered.
//
// A context with no authenticated caller, or a token with no expiry claim, is
// returned uncancelled — the returned context is still cancellable through the
// returned function, so the deferred cancel is correct either way.
func ContextWithTokenExpiry(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	expiry, ok := tokenExpiry(ctx)
	if !ok {
		return context.WithCancel(ctx)
	}

	eCtx, cancel := context.WithDeadlineCause(ctx, expiry, ErrTokenExpired)

	// The interceptor only sees the context it created itself, so the
	// expiry is recorded where it can read it back when the handler
	// returns.
	state := streamStateFrom(ctx)
	if state != nil {
		state.expiresWith(func() bool {
			return errors.Is(context.Cause(eCtx), ErrTokenExpired)
		})
	}

	return eCtx, cancel
}

// tokenExpiry returns the expiry of the calling client's token. JWTClaims
// embeds jwt.RegisteredClaims, so the expiry is already on the context for
// every authenticated caller whose token carries one.
func tokenExpiry(ctx context.Context) (time.Time, bool) {
	info, ok := auth.GetInfo(ctx)
	if !ok {
		return time.Time{}, false
	}

	exp := info.Claims.ExpiresAt
	if exp == nil || exp.IsZero() {
		return time.Time{}, false
	}

	return exp.Time, true
}

// streamState records why a streaming call's context was cancelled, so that
// the client is told the reason rather than the bare cancellation a handler
// returns for it. DrainInterceptor puts it on the context and
// ContextWithTokenExpiry finds it there.
type streamState struct {
	mu      sync.Mutex
	drain   bool
	expired func() bool
}

// draining records that the server started draining.
func (s *streamState) draining() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.drain = true
}

// expiresWith records how to tell whether the caller's token expired during
// the call.
func (s *streamState) expiresWith(expired func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.expired = expired
}

// cause returns the reason the call's context was cancelled, or nil if this
// package did not cancel it.
func (s *streamState) cause() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.drain:
		return ErrDraining
	case s.expired != nil && s.expired():
		return ErrTokenExpired
	}

	return nil
}

type streamStateKey struct{}

// withStreamState returns the context with a stream state on it, and the
// state.
func withStreamState(ctx context.Context) (context.Context, *streamState) {
	state := streamState{}

	return context.WithValue(ctx, streamStateKey{}, &state), &state
}

// streamStateFrom returns the stream state of the call, or nil for a call that
// is not wrapped by DrainInterceptor.
func streamStateFrom(ctx context.Context) *streamState {
	state, _ := ctx.Value(streamStateKey{}).(*streamState)

	return state
}

// endOfStreamError maps the error a streaming handler returned to the error
// the client is told the stream ended with.
func endOfStreamError(state *streamState, err error) error {
	if err == nil {
		return nil
	}

	reason := err

	// A handler that returns ctx.Err() rather than context.Cause(ctx)
	// reports a bare cancellation, which says nothing about why the stream
	// ended, so the recorded reason is what answers that.
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		recorded := state.cause()
		if recorded != nil {
			reason = recorded
		}
	}

	switch {
	case errors.Is(reason, ErrDraining):
		return connect.NewError(connect.CodeUnavailable, ErrDraining)
	case errors.Is(reason, ErrTokenExpired):
		return connect.NewError(connect.CodeUnauthenticated, ErrTokenExpired)
	}

	return err
}
