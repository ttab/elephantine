package rpc

import (
	"context"
	"maps"
	"net/http"

	"connectrpc.com/connect"
)

type headerCtxKey int

const outgoingHeadersCtxKey headerCtxKey = 1

// WithOutgoingHeaders creates a child context carrying HTTP headers to set on
// the requests made through a client that has the PropagateHeaders interceptor.
// It replaces twirp.WithHTTPRequestHeaders, and like it, it replaces rather
// than merges: a second call decides the headers.
//
// The headers are copied, so a later change to h does not reach the requests.
func WithOutgoingHeaders(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, outgoingHeadersCtxKey, h.Clone())
}

// OutgoingHeaders returns the headers set on the context with
// WithOutgoingHeaders, or nil if it carries none.
func OutgoingHeaders(ctx context.Context) http.Header {
	h, _ := ctx.Value(outgoingHeadersCtxKey).(http.Header)

	return h
}

// PropagateHeaders is the client interceptor that applies the headers set with
// WithOutgoingHeaders to the outgoing request. Pass it to a generated client
// constructor as connect.WithInterceptors(rpc.PropagateHeaders()).
//
// It has to be an interceptor rather than something the generated client does,
// because the generated clients are compiled from elephant-api, which must not
// depend on elephantine.
func PropagateHeaders() connect.Interceptor {
	return interceptor{
		unary: func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(
				ctx context.Context, req connect.AnyRequest,
			) (connect.AnyResponse, error) {
				if req.Spec().IsClient {
					maps.Copy(req.Header(), OutgoingHeaders(ctx))
				}

				return next(ctx, req)
			}
		},
		streamingClient: func(
			next connect.StreamingClientFunc,
		) connect.StreamingClientFunc {
			return func(
				ctx context.Context, spec connect.Spec,
			) connect.StreamingClientConn {
				conn := next(ctx, spec)

				maps.Copy(conn.RequestHeader(), OutgoingHeaders(ctx))

				return conn
			}
		},
	}
}
