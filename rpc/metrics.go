package rpc

import (
	"context"
	"errors"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
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
// answered with. rpc_protocol_responses_total is the series to read the error
// breakdown from instead, since it carries the RPC code itself.
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

	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(
			ctx context.Context, req connect.AnyRequest,
		) (connect.AnyResponse, error) {
			var (
				service, method = splitProcedure(req.Spec().Procedure)
				customer        = opt.customer(ctx)
				start           = time.Now()
			)

			metrics.Requests.WithLabelValues(
				service, method, customer).Inc()

			res, err := next(ctx, req)

			duration := time.Since(start).Seconds()
			if opt.testLatency != 0 {
				duration = opt.testLatency.Seconds()
			}

			metrics.Duration.WithLabelValues(
				service, method, customer).Observe(duration)

			metrics.Responses.WithLabelValues(
				service, method, responseStatus(err), customer).Inc()

			metrics.ProtocolResponses.WithLabelValues(
				service, method,
				protocolLabel(req.Peer().Protocol),
				responseCode(err),
			).Inc()

			return res, err
		}
	}), nil
}

// responseStatus is the HTTP status the response will be sent with, as a
// string, which is the form twirp.StatusCode reports it in.
func responseStatus(err error) string {
	if err == nil {
		return "200"
	}

	return strconv.Itoa(HTTPStatus(responseConnectCode(err)))
}

// responseCode is the RPC code of the response, or "ok" for a response that
// carried no error.
func responseCode(err error) string {
	if err == nil {
		return rpcmetrics.CodeOK
	}

	return responseConnectCode(err).String()
}

// responseConnectCode is the code Connect will answer the error with. An
// uncoded error is answered with unknown, which is what Connect itself does.
func responseConnectCode(err error) connect.Code {
	cErr, ok := errors.AsType[*connect.Error](err)
	if !ok {
		return connect.CodeUnknown
	}

	return cErr.Code()
}

// protocolLabel maps connect-go's protocol name to the protocol label value.
// Only the spelling of gRPC-Web differs; a protocol connect-go adds later is
// bucketed as "other" rather than put into the label space unannounced.
func protocolLabel(protocol string) string {
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
