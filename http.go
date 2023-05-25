package elephantine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type HTTPError struct {
	Status     string
	StatusCode int
	Header     http.Header
	Body       io.Reader
}

func (e *HTTPError) Error() string {
	return e.Status
}

func NewHTTPError(statusCode int, message string) *HTTPError {
	return &HTTPError{
		StatusCode: statusCode,
		Header: http.Header{
			"Content-Type": []string{"text/plain"},
		},
		Body: strings.NewReader(message),
	}
}

func HTTPErrorf(statusCode int, format string, a ...any) *HTTPError {
	return NewHTTPError(statusCode, fmt.Sprintf(format, a...))
}

func IsHTTPErrorWithStatus(err error, status int) bool {
	var httpErr *HTTPError

	if !errors.As(err, &httpErr) {
		return false
	}

	return httpErr.StatusCode == status
}

func HTTPErrorFromResponse(res *http.Response) *HTTPError {
	e := HTTPError{
		Status:     res.Status,
		StatusCode: res.StatusCode,
		Header:     res.Header,
	}

	var buf bytes.Buffer

	_, _ = io.Copy(&buf, res.Body)

	e.Body = &buf

	return &e
}

func ListenAndServeContext(ctx context.Context, server *http.Server) error {
	go func() {
		<-ctx.Done()

		_ = server.Close()
	}()

	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return err //nolint:wrapcheck
	} else if err != nil {
		return fmt.Errorf("failed to start listening: %w", err)
	}

	return nil
}

type HTTPClientInstrumentation struct {
	inFlight *prometheus.GaugeVec
	counter  *prometheus.CounterVec
	trace    *promhttp.InstrumentTrace
	histVec  *prometheus.HistogramVec
}

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
			Name: "client_api_requests_total",
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
			Name:    "request_duration_seconds",
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
