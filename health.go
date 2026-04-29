package elephantine

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/pprof" //nolint:gosec
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HealthServer exposes health endpoints, metrics, and PPROF endpoints.
//
// A HealthServer should never be publicly exposed, as that both could expose
// sensitive information and could be used to DDOS your application.
//
// Example output for a request to `GET /health/ready`:
//
//	{
//	  "api_liveness": {
//	    "ok": false,
//	    "error": "api liveness endpoint returned non-ok status: 404 Not Found"
//	  },
//	  "postgres": {
//	    "ok": true
//	  },
//	  "s3": {
//	    "ok": true
//	  }
//	}
type HealthServer struct {
	logger         *slog.Logger
	testServer     *httptest.Server
	server         *http.Server
	readyFunctions map[string]readyEntry
	upGauge        *prometheus.GaugeVec
}

type readyEntry struct {
	fn       ReadyFunc
	optional bool
}

type healthServerOptions struct {
	registerer prometheus.Registerer
}

// HealthServerOption configures a HealthServer.
type HealthServerOption func(*healthServerOptions)

// WithHealthServerRegisterer sets the prometheus registerer used for the
// readiness check gauge. Pass nil to disable metric registration.
func WithHealthServerRegisterer(reg prometheus.Registerer) HealthServerOption {
	return func(o *healthServerOptions) {
		o.registerer = reg
	}
}

// NewHealthServer creates a new health server that will listen to the provided
// address. Pass an empty addr to construct a no-op server: the readiness
// machinery still works (useful for tests and for processes that share a
// health endpoint with another listener), but ListenAndServe does not bind
// any socket.
func NewHealthServer(
	logger *slog.Logger, addr string, opts ...HealthServerOption,
) *HealthServer {
	s := newHealthServer(logger, healthServerOptions{
		registerer: prometheus.DefaultRegisterer,
	}, opts)

	if addr != "" {
		s.server = &http.Server{
			Addr:              addr,
			Handler:           s.setUpMux(),
			ReadHeaderTimeout: 1 * time.Second,
		}
	}

	return s
}

func NewTestHealthServer(
	logger *slog.Logger, opts ...HealthServerOption,
) *HealthServer {
	s := newHealthServer(logger, healthServerOptions{}, opts)

	s.testServer = httptest.NewServer(s.setUpMux())

	return s
}

func newHealthServer(
	logger *slog.Logger,
	defaults healthServerOptions,
	opts []HealthServerOption,
) *HealthServer {
	o := defaults

	for _, opt := range opts {
		opt(&o)
	}

	s := HealthServer{
		logger:         logger,
		readyFunctions: make(map[string]readyEntry),
		upGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "health_check_up",
				Help: "1 if the named readiness check is passing, 0 otherwise.",
			},
			[]string{"name"},
		),
	}

	if o.registerer != nil {
		err := o.registerer.Register(s.upGauge)

		var are prometheus.AlreadyRegisteredError
		switch {
		case errors.As(err, &are):
			if existing, ok := are.ExistingCollector.(*prometheus.GaugeVec); ok {
				s.upGauge = existing
			}
		case err != nil:
			logger.Warn("register health check gauge",
				LogKeyError, err)
		}
	}

	return &s
}

func (s *HealthServer) Addr() string {
	var addr string

	switch {
	case s.testServer != nil:
		addr = s.testServer.Listener.Addr().String()
	case s.server != nil:
		addr = s.server.Addr
	default:
		return ""
	}

	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}

	return addr
}

func (s *HealthServer) setUpMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	mux.Handle("/debug/vars", expvar.Handler())
	mux.Handle("/debug/bom", http.HandlerFunc(bomHandler))
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/health/ready", http.HandlerFunc(s.readyHandler))

	return mux
}

type readyResult struct {
	Ok       bool   `json:"ok"`
	Optional bool   `json:"optional,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (s *HealthServer) readyHandler(
	w http.ResponseWriter, req *http.Request,
) {
	var failed bool

	result := make(map[string]readyResult)

	for name, entry := range s.readyFunctions {
		err := entry.fn(req.Context())
		if err != nil {
			if !entry.optional {
				failed = true
			}

			s.logger.Error("healthcheck failed",
				LogKeyName, name,
				LogKeyError, err,
				"optional", entry.optional,
			)

			s.upGauge.WithLabelValues(name).Set(0)

			result[name] = readyResult{
				Ok:       false,
				Optional: entry.optional,
				Error:    err.Error(),
			}

			continue
		}

		s.upGauge.WithLabelValues(name).Set(1)

		result[name] = readyResult{Ok: true, Optional: entry.optional}
	}

	w.Header().Set("Content-Type", "application/json")

	if failed {
		w.WriteHeader(http.StatusInternalServerError)
	}

	enc := json.NewEncoder(w)

	// Making health endpoints human-readable is always a nice touch.
	enc.SetIndent("", "  ")

	_ = enc.Encode(result)
}

// ReadyFunc is a function that will be called to determine if a service is
// ready to recieve traffic. It should return a descriptive error that helps
// with debugging if the underlying check fails.
type ReadyFunc func(ctx context.Context) error

// AddReadyFunction adds a function that will be called when a client requests
// "/health/ready". A non-nil error from the function will cause "/health/ready"
// to respond with 500.
func (s *HealthServer) AddReadyFunction(name string, fn ReadyFunc) {
	s.readyFunctions[name] = readyEntry{fn: fn}
	s.upGauge.WithLabelValues(name).Set(0)
}

// AddOptionalReadyFunction adds a function that will be called when a client
// requests "/health/ready". A non-nil error from the function will be reported
// in the response body with "ok": false but will not cause "/health/ready" to
// respond with 500.
func (s *HealthServer) AddOptionalReadyFunction(name string, fn ReadyFunc) {
	s.readyFunctions[name] = readyEntry{fn: fn, optional: true}
	s.upGauge.WithLabelValues(name).Set(0)
}

// Close stops the health server.
func (s *HealthServer) Close() error {
	switch {
	case s.server != nil:
		err := s.server.Close()
		if err != nil {
			return fmt.Errorf("failed to close http server: %w", err)
		}
	case s.testServer != nil:
		s.testServer.Close()
	}

	return nil
}

// ListenAndServe starts the health server, shutting it down if the context gets
// cancelled.
func (s *HealthServer) ListenAndServe(ctx context.Context) error {
	if s.server != nil {
		return ListenAndServeContext(ctx, s.server, 5*time.Second)
	} else {
		<-ctx.Done()
	}

	return nil
}

// LivenessReadyCheck returns a ReadyFunc that verifies that an endpoint aswers
// to GET requests with 200 OK.
func LivenessReadyCheck(endpoint string) ReadyFunc {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(
			ctx, http.MethodGet, endpoint, nil,
		)
		if err != nil {
			return fmt.Errorf(
				"failed to create liveness check request: %w", err)
		}

		var client http.Client

		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf(
				"failed to perform liveness check request: %w", err)
		}

		_ = res.Body.Close()

		if res.StatusCode != http.StatusOK {
			return fmt.Errorf(
				"api liveness endpoint returned non-ok status: %s",
				res.Status)
		}

		return nil
	}
}
