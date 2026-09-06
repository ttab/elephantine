package rpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/ttab/elephantine/internal/auth"
)

// interceptor is a connect.Interceptor built from the three wrapper functions,
// so that an interceptor in this package covers streaming calls as well as
// unary ones. A nil function is a pass-through.
//
// connect.UnaryInterceptorFunc, which is the obvious way to write an
// interceptor, passes streaming calls straight through. For an authentication
// or an authorization interceptor that means a streaming handler running with
// no check at all, and for logging and metrics it means a streaming call
// leaving no trace, so nothing in this package uses it.
type interceptor struct {
	unary            func(connect.UnaryFunc) connect.UnaryFunc
	streamingClient  func(connect.StreamingClientFunc) connect.StreamingClientFunc
	streamingHandler func(connect.StreamingHandlerFunc) connect.StreamingHandlerFunc
}

// WrapUnary implements connect.Interceptor.
func (i interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	if i.unary == nil {
		return next
	}

	return i.unary(next)
}

// WrapStreamingClient implements connect.Interceptor.
func (i interceptor) WrapStreamingClient(
	next connect.StreamingClientFunc,
) connect.StreamingClientFunc {
	if i.streamingClient == nil {
		return next
	}

	return i.streamingClient(next)
}

// WrapStreamingHandler implements connect.Interceptor.
func (i interceptor) WrapStreamingHandler(
	next connect.StreamingHandlerFunc,
) connect.StreamingHandlerFunc {
	if i.streamingHandler == nil {
		return next
	}

	return i.streamingHandler(next)
}

// AuthInfoInterceptor refuses a call that reaches a handler with no
// authenticated caller on its context, for a service that requires
// authentication.
//
// The authentication middleware in elephantine.ServiceOptions answers such a
// request itself, so this is the safety net for a mount that does not run the
// middleware: without it a handler would run unauthenticated and have to
// discover that for itself. It covers streaming handlers as well as unary
// ones, which is why it is not a connect.UnaryInterceptorFunc.
//
// Pass required as false for a service that allows anonymous callers; the
// interceptor is then a pass-through, and it is the handler's scope check that
// decides what an anonymous caller may do.
func AuthInfoInterceptor(required bool) connect.Interceptor {
	check := func(ctx context.Context) error {
		if !required {
			return nil
		}

		_, ok := auth.GetInfo(ctx)
		if ok {
			return nil
		}

		return Unauthenticated("authentication required")
	}

	return interceptor{
		unary: func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(
				ctx context.Context, req connect.AnyRequest,
			) (connect.AnyResponse, error) {
				err := check(ctx)
				if err != nil {
					return nil, err
				}

				return next(ctx, req)
			}
		},
		streamingHandler: func(
			next connect.StreamingHandlerFunc,
		) connect.StreamingHandlerFunc {
			return func(
				ctx context.Context, conn connect.StreamingHandlerConn,
			) error {
				err := check(ctx)
				if err != nil {
					return err
				}

				return next(ctx, conn)
			}
		},
	}
}
