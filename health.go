package elephantine

import (
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/pprof" //nolint:gosec
	"time"

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
	readyFunctions map[string]ReadyFunc
}

// NewHealthServer creates a new health server that will listen to the provided
// address.
func NewHealthServer(logger *slog.Logger, addr string) *HealthServer {
	s := HealthServer{
		logger:         logger,
		readyFunctions: make(map[string]ReadyFunc),
	}

	s.server = &http.Server{
		Addr:              addr,
		Handler:           s.setUpMux(),
		ReadHeaderTimeout: 1 * time.Second,
	}

	return &s
}

func NewTestHealthServer() *HealthServer {
	s := HealthServer{
		readyFunctions: make(map[string]ReadyFunc),
	}

	s.testServer = httptest.NewServer(s.setUpMux())

	return &s
}

func (s *HealthServer) setUpMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	mux.Handle("/debug/vars", expvar.Handler())
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/health/ready", http.HandlerFunc(s.readyHandler))

	return mux
}

type readyResult struct {
	Ok    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *HealthServer) readyHandler(
	w http.ResponseWriter, req *http.Request,
) {
	var failed bool

	result := make(map[string]readyResult)

	for name, fn := range s.readyFunctions {
		err := fn(req.Context())
		if err != nil {
			failed = true

			s.logger.Error("healthcheck failed",
				LogKeyName, name,
				LogKeyError, err,
			)

			result[name] = readyResult{
				Ok:    false,
				Error: err.Error(),
			}

			continue
		}

		result[name] = readyResult{Ok: true}
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
// "/health/ready".
func (s *HealthServer) AddReadyFunction(name string, fn ReadyFunc) {
	s.readyFunctions[name] = fn
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
		return ListenAndServeContext(ctx, s.server)
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
