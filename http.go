package elephantine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HTTPError can be used to describe a non-OK response. Either as an error value
// in a client that got an error response from a server, or in a server
// implementation to communicate what the error response to a client should be.
type HTTPError struct {
	Status     string
	StatusCode int
	Header     http.Header
	Body       io.Reader
}

// Error implements the error interface.
func (e *HTTPError) Error() string {
	return e.Status
}

// NewHTTPError creates a new HTTPError with the given status code and response
// message.
func NewHTTPError(statusCode int, message string) *HTTPError {
	return &HTTPError{
		Status:     http.StatusText(statusCode),
		StatusCode: statusCode,
		Header: http.Header{
			"Content-Type": []string{"text/plain"},
		},
		Body: strings.NewReader(message),
	}
}

// HTTPErrorf creates a HTTPError using a format string.
func HTTPErrorf(statusCode int, format string, a ...any) *HTTPError {
	return NewHTTPError(statusCode, fmt.Sprintf(format, a...))
}

// IsHTTPErrorWithStatus checks if the error (or any error in its tree) is a
// HTTP error with the given status code.
func IsHTTPErrorWithStatus(err error, status int) bool {
	var httpErr *HTTPError

	if !errors.As(err, &httpErr) {
		return false
	}

	return httpErr.StatusCode == status
}

// HTTPErrorFromResponse creates a HTTPError from a response struct. This will
// consume and create a copy of the response body, so don't use it in a scenario
// where you expect really large error response bodies.
//
// If we fail to copy the response body the error will be joined with the
// HTTPError.
func HTTPErrorFromResponse(res *http.Response) error {
	e := HTTPError{
		Status:     res.Status,
		StatusCode: res.StatusCode,
		Header:     res.Header,
	}

	var buf bytes.Buffer

	e.Body = &buf

	_, err := io.Copy(&buf, res.Body)
	if err != nil {
		return errors.Join(&e,
			fmt.Errorf("failed to read response body: %w", err))
	}

	return &e
}

// ListenAndServeContext will call ListenAndServe() for the provided server and
// then Shutdown() if the context is cancelled.
//
// Check `errors.Is(err, http.ErrServerClosed)` to differentiate between a
// graceful server close and other errors.
func ListenAndServeContext(
	ctx context.Context, server *http.Server,
	shutdownTimeout time.Duration,
) error {
	closed := make(chan struct{})

	go func() {
		defer close(closed)

		<-ctx.Done()

		shtCtx, cancel := context.WithTimeout(
			context.Background(), shutdownTimeout)
		defer cancel()

		err := server.Shutdown(shtCtx)
		if err != nil {
			_ = server.Close()
		}
	}()

	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		// Listens and serve exits immediately when server.Shutdown() is
		// called, wait for it to actually be closed, gracefully or
		// otherwise.
		<-closed

		return err //nolint:wrapcheck
	} else if err != nil {
		return fmt.Errorf("failed to start listening: %w", err)
	}

	return nil
}

// HTTPClientInstrumentation provides a way to instrument HTTP clients.
type HTTPClientInstrumentation struct {
	inFlight *prometheus.GaugeVec
	counter  *prometheus.CounterVec
	trace    *promhttp.InstrumentTrace
	histVec  *prometheus.HistogramVec
}

// NewHTTPClientIntrumentation registers a set of HTTP client metrics with the
// provided registerer.
func NewHTTPClientIntrumentation(
	registerer prometheus.Registerer,
) (*HTTPClientInstrumentation, error) {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	inFlightGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "client_in_flight_requests",
			Help: "A gauge of in-flight requests for the wrapped client.",
		},
		[]string{"client"},
	)

	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "client_requests_total",
			Help: "A counter for requests from the wrapped client.",
		},
		[]string{"client", "code", "method"},
	)

	// dnsLatencyVec uses custom buckets based on expected dns durations.
	// It has an instance label "event", which is set in the
	// DNSStart and DNSDonehook functions defined in the
	// InstrumentTrace struct below.
	dnsLatencyVec := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "dns_duration_seconds",
			Help:    "Trace dns latency histogram.",
			Buckets: []float64{.005, .01, .025, .05},
		},
		[]string{"event"},
	)

	// tlsLatencyVec uses custom buckets based on expected tls durations.
	// It has an instance label "event", which is set in the
	// TLSHandshakeStart and TLSHandshakeDone hook functions defined in the
	// InstrumentTrace struct below.
	tlsLatencyVec := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "tls_duration_seconds",
			Help:    "Trace tls latency histogram.",
			Buckets: []float64{.05, .1, .25, .5},
		},
		[]string{"event"},
	)

	// histVec has no labels, making it a zero-dimensional ObserverVec.
	histVec := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "client_request_duration_seconds",
			Help:    "A histogram of request latencies.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"client"},
	)

	collectors := []prometheus.Collector{
		inFlightGauge, counter,
		tlsLatencyVec, dnsLatencyVec, histVec,
	}

	for i, c := range collectors {
		err := registerer.Register(c)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to register metrics collector %d: %w",
				i, err)
		}
	}

	// Define functions for the available httptrace.ClientTrace hook
	// functions that we want to instrument.
	trace := &promhttp.InstrumentTrace{
		DNSStart: func(t float64) {
			dnsLatencyVec.WithLabelValues("dns_start").Observe(t)
		},
		DNSDone: func(t float64) {
			dnsLatencyVec.WithLabelValues("dns_done").Observe(t)
		},
		TLSHandshakeStart: func(t float64) {
			tlsLatencyVec.WithLabelValues("tls_handshake_start").Observe(t)
		},
		TLSHandshakeDone: func(t float64) {
			tlsLatencyVec.WithLabelValues("tls_handshake_done").Observe(t)
		},
	}

	ci := HTTPClientInstrumentation{
		inFlight: inFlightGauge,
		counter:  counter,
		trace:    trace,
		histVec:  histVec,
	}

	return &ci, nil
}

// Client instruments the HTTP client transport with the standard promhttp
// metrics. The client_requests_total, client_in_flight_requests, and
// client_request_duration_seconds metrics will be labelled with the client
// name.
func (ci *HTTPClientInstrumentation) Client(name string, client *http.Client) error {
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	cCounter, err := ci.counter.CurryWith(prometheus.Labels{
		"client": name,
	})
	if err != nil {
		return fmt.Errorf("failed to curry request counter: %w", err)
	}

	cHistVec, err := ci.histVec.CurryWith(prometheus.Labels{
		"client": name,
	})
	if err != nil {
		return fmt.Errorf("failed to curry duration histogram: %w", err)
	}

	transport = promhttp.InstrumentRoundTripperDuration(cHistVec, transport)
	transport = promhttp.InstrumentRoundTripperTrace(ci.trace, transport)
	transport = promhttp.InstrumentRoundTripperCounter(cCounter, transport)
	transport = ci.instrumentInFlight(name, transport)

	client.Transport = transport

	return nil
}

func (ci *HTTPClientInstrumentation) instrumentInFlight(client string, next http.RoundTripper) promhttp.RoundTripperFunc {
	return func(r *http.Request) (*http.Response, error) {
		ci.inFlight.WithLabelValues(client).Inc()
		defer ci.inFlight.WithLabelValues(client).Dec()

		return next.RoundTrip(r)
	}
}

// NewHTTPClient returns a http.Client configured with timeouts and connection
// limits. The default request timeout, including time for response read is 10
// seconds. Use the option functions to customise.
func NewHTTPClient(
	timeout time.Duration,
	opts ...HTTPClientOption,
) *http.Client {
	o := HTTPClientOptions{
		client: &http.Client{
			Timeout: timeout,
		},
		transport: &http.Transport{
			TLSHandshakeTimeout:   min(3*time.Second, timeout/2),
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   6,
			IdleConnTimeout:       90 * time.Second,
			MaxConnsPerHost:       12,
			ResponseHeaderTimeout: min(5*time.Second, timeout/2),
		},
		dialer: &net.Dialer{
			Timeout: DialTimeoutExternal,
		},
	}

	o.transport.DialContext = o.dialer.DialContext
	o.client.Transport = o.transport

	for _, opt := range opts {
		opt(&o)
	}

	return o.client
}

type HTTPClientOption func(opts *HTTPClientOptions)

type HTTPClientOptions struct {
	client    *http.Client
	transport *http.Transport
	dialer    *net.Dialer
}

const (
	DialTimeoutInternal = 1 * time.Second
	DialTimeoutExternal = 5 * time.Second
	DialTimeoutSlow     = 10 * time.Second
)

func DialTimeout(d time.Duration) HTTPClientOption {
	return func(opts *HTTPClientOptions) {
		opts.dialer.Timeout = d
	}
}

func TLSHandshakeTimeout(d time.Duration) HTTPClientOption {
	return func(opts *HTTPClientOptions) {
		opts.transport.TLSHandshakeTimeout = d
	}
}

func ResponseHeaderTimeout(d time.Duration) HTTPClientOption {
	return func(opts *HTTPClientOptions) {
		opts.transport.ResponseHeaderTimeout = d
	}
}

// LongpollClient is syntactic sugar for setting the response header timeout to
// 0 (no timeout), can be used to communicate intent.
func LongpollClient() HTTPClientOption {
	return ResponseHeaderTimeout(0)
}

func IdleConnections(
	maxIdle int,
	maxIdlePerHost int,
	idleConnTimeout time.Duration,
) HTTPClientOption {
	return func(opts *HTTPClientOptions) {
		opts.transport.MaxIdleConns = maxIdle
		opts.transport.MaxConnsPerHost = maxIdlePerHost
		opts.transport.IdleConnTimeout = idleConnTimeout
	}
}

func MaxConnectionsPerHost(n int) HTTPClientOption {
	return func(opts *HTTPClientOptions) {
		opts.transport.MaxConnsPerHost = n
	}
}
