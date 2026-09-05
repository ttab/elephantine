// Package rpcmetrics declares the RPC server metrics. The Twirp hooks in the
// elephantine root package and the Connect interceptor in the rpc package
// report the same series, so the collectors are declared once here and both
// constructors reuse an already registered collector rather than failing. A
// dual-stack service therefore registers each metric once, whichever stack is
// wired up first.
package rpcmetrics

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric label names.
const (
	LabelService  = "service"
	LabelMethod   = "method"
	LabelStatus   = "status"
	LabelCustomer = "customer"
	LabelProtocol = "protocol"
	LabelCode     = "code"
)

// Protocol label values. They are the RPC protocols a dual-stack service
// serves; the Connect ones are connect-go's own protocol names, with gRPC-Web
// spelled the way the specification does.
const (
	ProtocolTwirp   = "twirp"
	ProtocolConnect = "connect"
	ProtocolGRPC    = "grpc"
	ProtocolGRPCWeb = "grpc-web"
)

// CodeOK is the code label value used for a response that carried no error.
const CodeOK = "ok"

// protocolResponsesHelp is the help text for rpc_protocol_responses_total.
const protocolResponsesHelp = "Number of RPC responses sent, by protocol and" +
	" RPC code. Traffic on protocol=\"twirp\" is what still keeps a" +
	" service's Twirp mount alive, so a method whose Twirp share has" +
	" reached zero can have it removed; the code label separates errors" +
	" that rpc_responses_total reports under a single HTTP status, such" +
	" as a lock conflict from a validation failure."

// Metrics is the RPC server metric set.
type Metrics struct {
	// Requests counts received requests, before the handler runs.
	Requests *prometheus.CounterVec
	// Duration observes the handler runtime.
	Duration *prometheus.HistogramVec
	// Responses counts sent responses by HTTP status.
	Responses *prometheus.CounterVec
	// ProtocolResponses counts sent responses by protocol and RPC code.
	ProtocolResponses *prometheus.CounterVec
}

// New declares and registers the RPC metrics. A collector that is already
// registered with the registerer is reused, so that a service serving both
// Twirp and Connect reports one set of series.
func New(reg prometheus.Registerer) (*Metrics, error) {
	var m Metrics

	requests, err := registerOrReuse(reg, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rpc_requests_total",
			Help: "Number of RPC requests received.",
		},
		[]string{LabelService, LabelMethod, LabelCustomer},
	))
	if err != nil {
		return nil, err
	}

	duration, err := registerOrReuse(reg, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rpc_duration_seconds",
			Help:    "Duration for a rpc call.",
			Buckets: prometheus.ExponentialBuckets(0.005, 1.75, 15),
		},
		[]string{LabelService, LabelMethod, LabelCustomer},
	))
	if err != nil {
		return nil, err
	}

	responses, err := registerOrReuse(reg, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rpc_responses_total",
			Help: "Number of RPC responses sent.",
		},
		[]string{LabelService, LabelMethod, LabelStatus, LabelCustomer},
	))
	if err != nil {
		return nil, err
	}

	protocolResponses, err := registerOrReuse(reg, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rpc_protocol_responses_total",
			Help: protocolResponsesHelp,
		},
		[]string{LabelService, LabelMethod, LabelProtocol, LabelCode},
	))
	if err != nil {
		return nil, err
	}

	m.Requests = requests
	m.Duration = duration
	m.Responses = responses
	m.ProtocolResponses = protocolResponses

	return &m, nil
}

// registerOrReuse registers the collector, returning an already registered
// collector with the same descriptors instead of an error.
func registerOrReuse[C prometheus.Collector](
	reg prometheus.Registerer, c C,
) (C, error) {
	var zero C

	err := reg.Register(c)

	var are prometheus.AlreadyRegisteredError

	switch {
	case errors.As(err, &are):
		existing, ok := are.ExistingCollector.(C)
		if !ok {
			return zero, fmt.Errorf(
				"the registered collector %T does not match %T",
				are.ExistingCollector, c)
		}

		return existing, nil
	case err != nil:
		return zero, fmt.Errorf("register collector: %w", err)
	}

	return c, nil
}
