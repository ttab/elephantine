package rpc

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine/internal/auth"
	"github.com/ttab/elephantine/internal/rpcmetrics"
)

// MetricsOptions holds the configuration for the Connect metrics interceptor
// created by MetricsInterceptor. Set it through MetricsOption functions.
type MetricsOptions struct {
	customer    func(ctx context.Context) string
	testLatency time.Duration
}

// MetricsOption configures the interceptor created by MetricsInterceptor.
type MetricsOption func(opts *MetricsOptions)

// WithMetricsCustomerFunc sets a function that returns the customer label value
// for a context. It is the Connect counterpart of
// elephantine.WithTwirpMetricsCustomerFunc, and a dual-stack service passes the
// same function to both so the label means the same thing on both stacks.
func WithMetricsCustomerFunc(fn func(ctx context.Context) string) MetricsOption {
	return func(opts *MetricsOptions) {
		opts.customer = fn
	}
}

// WithMetricsStaticTestLatency makes the interceptor report a static duration,
// so that a test can assert on the histogram.
func WithMetricsStaticTestLatency(latency time.Duration) MetricsOption {
	return func(opts *MetricsOptions) {
		opts.testLatency = latency
	}
}

// MetricsInterceptor observes rpc_requests_total, rpc_duration_seconds,
// rpc_responses_total and rpc_protocol_responses_total for the handlers it
// wraps. The first three are the series elephantine.NewTwirpMetricsHooks has
// always reported, with the same names, labels and label values, and the
// collectors are shared with the hooks, so a service serving both protocols
// registers each metric once and its dashboards keep working.
//
// The status label is the HTTP status Connect answers the code with, which for
// canceled, deadline_exceeded and failed_precondition is not the status Twirp
// answered with. It is 200 for a gRPC or a gRPC-Web response, since those
// protocols answer every call with 200 and carry the code in the trailers;
// rpc_protocol_responses_total is where their outcome is readable.
//
// A call that failed authentication is counted as a response but not as a
// request, which is also what the hooks report, so the difference between the
// two series means the same thing on both stacks. A request the authentication
// middleware refuses never reaches an interceptor at all; the middleware counts
// that response itself, the same way.
//
// Framework-level failures on the Connect stack are not counted here, because
// connect-go answers them before it calls an interceptor: a malformed body, an
// unsupported content type, an unknown method and an oversized body are all
// answered by the protocol handler. Twirp reports those through its hooks, so
// the two stacks differ for requests that never reach a method.
func MetricsInterceptor(
	reg prometheus.Registerer, opts ...MetricsOption,
) (connect.Interceptor, error) {
	opt := MetricsOptions{
		customer: func(_ context.Context) string {
			return ""
		},
	}

	for i := range opts {
		opts[i](&opt)
	}

	metrics, err := rpcmetrics.New(reg)
	if err != nil {
		return nil, err
	}

	observe := func(
		ctx context.Context, procedure string, protocol string,
		start time.Time, err error,
	) {
		var (
			service, method = splitProcedure(procedure)
			customer        = opt.customer(ctx)
			clientID        = auth.ClientIDFromContext(ctx)
		)

		duration := time.Since(start).Seconds()
		if opt.testLatency != 0 {
			duration = opt.testLatency.Seconds()
		}

		metrics.Duration.WithLabelValues(
			service, method, customer).Observe(duration)

		metrics.Responses.WithLabelValues(
			service, method,
			ResponseStatus(err, protocol), customer).Inc()

		metrics.ProtocolResponses.WithLabelValues(
			service, method, protocol, codeLabel(err), clientID,
		).Inc()
	}

	countRequest := func(ctx context.Context, procedure string) {
		service, method := splitProcedure(procedure)

		metrics.Requests.WithLabelValues(
			service, method, opt.customer(ctx)).Inc()
	}

	return interceptor{
		unary: func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(
				ctx context.Context, req connect.AnyRequest,
			) (connect.AnyResponse, error) {
				start := time.Now()

				countRequest(ctx, req.Spec().Procedure)

				res, err := next(ctx, req)

				observe(ctx, req.Spec().Procedure,
					ProtocolLabel(req.Peer().Protocol),
					start, err)

				return res, err
			}
		},
		streamingHandler: func(
			next connect.StreamingHandlerFunc,
		) connect.StreamingHandlerFunc {
			return func(
				ctx context.Context, conn connect.StreamingHandlerConn,
			) error {
				start := time.Now()

				countRequest(ctx, conn.Spec().Procedure)

				err := next(ctx, conn)

				observe(ctx, conn.Spec().Procedure,
					ProtocolLabel(conn.Peer().Protocol),
					start, err)

				return err
			}
		},
	}, nil
}

// ResponseStatus is the HTTP status a Connect response is sent with, as a
// string, which is the form twirp.StatusCode reports it in. gRPC and gRPC-Web
// answer every call with 200 and carry the code in the trailers, so a response
// on those protocols is reported as 200 whatever the error was.
//
// It is the Connect status mapping, which differs from Twirp's for canceled,
// deadline_exceeded and failed_precondition, so a caller reporting a Twirp
// response has to know that those three codes are the ones it cannot use it
// for. The authentication middleware can: the codes it answers with are
// answered with the same status on both stacks.
func ResponseStatus(err error, protocol string) string {
	switch protocol {
	case rpcmetrics.ProtocolGRPC, rpcmetrics.ProtocolGRPCWeb:
		return "200"
	}

	if err == nil {
		return "200"
	}

	return strconv.Itoa(HTTPStatus(ResponseCode(err)))
}

// codeLabel is the RPC code of the response, or "ok" for a response that
// carried no error.
func codeLabel(err error) string {
	if err == nil {
		return rpcmetrics.CodeOK
	}

	return ResponseCode(err).String()
}

// ResponseCode is the code Connect answers an error with. A *connect.Error
// carries its own code; an error that is not one but that wraps a context
// cancellation or a deadline is answered canceled or deadline_exceeded, the way
// connect-go itself codes it; anything else is unknown.
//
// The context codes matter because connect-go returns a bare context error from
// its own handler wrapper when the request context is already done, so a caller
// that disconnected or that ran out of its Connect-Timeout-Ms would otherwise
// be counted and logged as an unknown server fault.
func ResponseCode(err error) connect.Code {
	if err == nil {
		return connect.CodeUnknown
	}

	cErr, ok := errors.AsType[*connect.Error](err)
	if ok {
		return cErr.Code()
	}

	switch {
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, os.ErrDeadlineExceeded):
		// Some dial errors surface as os.ErrDeadlineExceeded rather than
		// context.DeadlineExceeded, which is why connect-go checks both.
		return connect.CodeDeadlineExceeded
	}

	return connect.CodeUnknown
}

// ProtocolLabel maps connect-go's protocol name to the protocol label value.
// Only the spelling of gRPC-Web differs; a protocol connect-go adds later is
// bucketed as "other" rather than put into the label space unannounced.
func ProtocolLabel(protocol string) string {
	switch protocol {
	case connect.ProtocolConnect:
		return rpcmetrics.ProtocolConnect
	case connect.ProtocolGRPC:
		return rpcmetrics.ProtocolGRPC
	case connect.ProtocolGRPCWeb:
		return rpcmetrics.ProtocolGRPCWeb
	default:
		return "other"
	}
}
