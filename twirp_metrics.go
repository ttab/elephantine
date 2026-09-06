package elephantine

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine/internal/auth"
	"github.com/ttab/elephantine/internal/rpcmetrics"
	"github.com/twitchtv/twirp"
)

// The code in this file is adapted from
// https://github.com/navigacontentlab/panurge/blob/main/twirp.go
//
// See twirp_metrics.LICENSE

// TwirpMetricsOptions holds the configuration for the Twirp metrics hooks
// created by NewTwirpMetricsHooks. Set it through TwirpMetricOptionFunc
// options.
type TwirpMetricsOptions struct {
	reg             prometheus.Registerer
	testLatency     time.Duration
	contextCustomer func(ctx context.Context) string
}

// TwirpMetricOptionFunc configures the metrics hooks created by
// NewTwirpMetricsHooks.
type TwirpMetricOptionFunc func(opts *TwirpMetricsOptions)

// WithTwirpMetricsRegisterer uses a custom registerer for Twirp metrics.
func WithTwirpMetricsRegisterer(reg prometheus.Registerer) TwirpMetricOptionFunc {
	return func(opts *TwirpMetricsOptions) {
		opts.reg = reg
	}
}

// WithTwirpMetricsStaticTestLatency configures the RPC metrics to report
// a static duration.
func WithTwirpMetricsStaticTestLatency(latency time.Duration) TwirpMetricOptionFunc {
	return func(opts *TwirpMetricsOptions) {
		opts.testLatency = latency
	}
}

// WithTwirpMetricsCustomerFunc sets a function that can be used to return the
// customer label value for a context.
func WithTwirpMetricsCustomerFunc(fn func(ctx context.Context) string) TwirpMetricOptionFunc {
	return func(opts *TwirpMetricsOptions) {
		opts.contextCustomer = fn
	}
}

// NewTwirpMetricsHooks creates new twirp hooks enabling prometheus metrics.
//
// Deprecated: a service that also serves Connect gets the same series on that
// stack from [github.com/ttab/elephantine/rpc.MetricsInterceptor], and
// [NewDefaultServiceOptions] installs both. The hooks go away with the last
// Twirp mount in the fleet.
func NewTwirpMetricsHooks(opts ...TwirpMetricOptionFunc) (*twirp.ServerHooks, error) {
	return newTwirpMetricsHooks(opts...)
}

// newTwirpMetricsHooks is the implementation of the deprecated
// NewTwirpMetricsHooks, called from the service options so that the deprecation
// does not have to be worked around inside the package.
func newTwirpMetricsHooks(
	opts ...TwirpMetricOptionFunc,
) (*twirp.ServerHooks, error) {
	opt := TwirpMetricsOptions{
		reg: prometheus.DefaultRegisterer,
		contextCustomer: func(_ context.Context) string {
			return ""
		},
	}

	for i := range opts {
		opts[i](&opt)
	}

	// The collectors are shared with rpc.MetricsInterceptor, so that a
	// service serving both protocols reports one set of series and
	// registers them once.
	metrics, err := rpcmetrics.New(opt.reg)
	if err != nil {
		return nil, err
	}

	var hooks twirp.ServerHooks

	stateKey := new(int)

	hooks.RequestReceived = func(ctx context.Context) (context.Context, error) {
		return context.WithValue(ctx, stateKey, &twirpCallState{
			start: time.Now(),
			code:  rpcmetrics.CodeOK,
		}), nil
	}

	// The error hook is where the RPC code is available. Twirp only hands
	// the response hook the HTTP status, which cannot tell a lock conflict
	// from a validation failure.
	hooks.Error = func(ctx context.Context, err twirp.Error) context.Context {
		state, ok := ctx.Value(stateKey).(*twirpCallState)
		if ok {
			state.code = string(err.Code())
		}

		return ctx
	}

	hooks.ResponseSent = func(ctx context.Context) {
		serviceName, sOk := twirp.ServiceName(ctx)
		method, mOk := twirp.MethodName(ctx)

		if !mOk || !sOk {
			return
		}

		customer := opt.contextCustomer(ctx)
		status, _ := twirp.StatusCode(ctx)

		metrics.Responses.WithLabelValues(
			serviceName, method, status, customer,
		).Inc()

		state, ok := ctx.Value(stateKey).(*twirpCallState)
		if !ok {
			return
		}

		metrics.ProtocolResponses.WithLabelValues(
			serviceName, method, rpcmetrics.ProtocolTwirp, state.code,
			auth.ClientIDFromContext(ctx),
		).Inc()

		dur := time.Since(state.start).Seconds() // 100ms = 0.1 sek

		if opt.testLatency != 0 {
			dur = opt.testLatency.Seconds()
		}

		metrics.Duration.WithLabelValues(
			serviceName, method, customer,
		).Observe(dur)
	}

	hooks.RequestRouted = func(ctx context.Context) (context.Context, error) {
		serviceName, sOk := twirp.ServiceName(ctx)
		method, mOk := twirp.MethodName(ctx)

		if !sOk || !mOk {
			return ctx, nil
		}

		customer := opt.contextCustomer(ctx)

		metrics.Requests.WithLabelValues(
			serviceName, method, customer,
		).Inc()

		return ctx, nil
	}

	return &hooks, nil
}

// twirpCallState is the per-request state the metrics hooks keep: when the
// request started, and the code the response ended up with.
type twirpCallState struct {
	start time.Time
	code  string
}
