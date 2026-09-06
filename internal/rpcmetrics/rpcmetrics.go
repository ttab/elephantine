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
	"strings"

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
	LabelClientID = "client_id"
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

// ProtocolResponsesHelp is the help text for rpc_protocol_responses_total. It
// is exported so that the tests can compare the exposition format against it
// rather than repeating it.
const ProtocolResponsesHelp = "Number of RPC responses sent, by protocol, RPC" +
	" code and calling client. Traffic on protocol=\"twirp\" is what still" +
	" keeps a service's Twirp mount alive, so a method whose Twirp share has" +
	" reached zero can have it removed, and client_id names the application" +
	" that has to move before it can; the code label separates errors that" +
	" rpc_responses_total reports under a single HTTP status, such as a lock" +
	" conflict from a validation failure."

// Metrics is the RPC server metric set.
type Metrics struct {
	// Requests counts received requests, before the handler runs.
	Requests *prometheus.CounterVec
	// Duration observes the handler runtime.
	Duration *prometheus.HistogramVec
	// Responses counts sent responses by HTTP status.
	Responses *prometheus.CounterVec
	// ProtocolResponses counts sent responses by protocol, RPC code and
	// calling client.
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
			Help: ProtocolResponsesHelp,
		},
		[]string{
			LabelService, LabelMethod, LabelProtocol, LabelCode,
			LabelClientID,
		},
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

// SplitProcedure splits an RPC procedure or request path into the short service
// name and the method name, which is how both stacks label a series. It takes
// the Connect procedure "/elephant.repository.Documents/Get" and the Twirp
// request path "/twirp/elephant.repository.Documents/Get" alike, since the two
// differ only in the prefix ahead of the fully qualified service name. ok is
// false for anything that does not name a service and a method.
func SplitProcedure(procedure string) (string, string, bool) {
	trimmed := strings.Trim(procedure, "/")

	i := strings.LastIndex(trimmed, "/")
	if i == -1 {
		return "", "", false
	}

	var (
		service = trimmed[:i]
		method  = trimmed[i+1:]
	)

	// Drop everything ahead of the fully qualified service name, which is a
	// Twirp path prefix, and then the package, which leaves the short
	// service name twirp.ServiceName reports.
	if j := strings.LastIndex(service, "/"); j != -1 {
		service = service[j+1:]
	}

	if k := strings.LastIndex(service, "."); k != -1 {
		service = service[k+1:]
	}

	if service == "" || method == "" {
		return "", "", false
	}

	return service, method, true
}
