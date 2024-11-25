package elephantine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twitchtv/twirp"
	"golang.org/x/sync/errgroup"
)

func NewAPIServer(
	logger *slog.Logger,
	addr string, profileAddr string,
) *APIServer {
	s := APIServer{
		logger:      logger,
		addr:        addr,
		profileAddr: profileAddr,
		Mux:         http.NewServeMux(),
		Health:      NewHealthServer(logger, profileAddr),
		CORS: &CORSOptions{
			AllowInsecure:          false,
			AllowInsecureLocalhost: true,
			Hosts:                  []string{"localhost", "tt.se"},
			AllowedMethods:         []string{"GET", "POST"},
			AllowedHeaders:         []string{"Authorization", "Content-Type"},
		},
	}

	s.Mux.Handle("GET /health/alive", http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)

		_, _ = fmt.Fprintln(w, "I AM ALIVE!")
	}))

	s.Health.AddReadyFunction("api_liveness",
		LivenessReadyCheck(s.AliveEndpoint()))

	return &s
}

type APIServer struct {
	logger      *slog.Logger
	addr        string
	profileAddr string
	Mux         *http.ServeMux
	Health      *HealthServer
	CORS        *CORSOptions
}

func (s *APIServer) AliveEndpoint() string {
	return fmt.Sprintf(
		"http://localhost%s/health/alive",
		s.addr,
	)
}

type APIServiceHandler interface {
	http.Handler

	PathPrefix() string
}

func (s *APIServer) RegisterAPI(
	api APIServiceHandler, opt ServiceOptions,
) {
	s.Mux.Handle("POST "+api.PathPrefix(), HTTPErrorHandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) error {
		if opt.AuthMiddleware != nil {
			return opt.AuthMiddleware(w, r, api)
		}

		api.ServeHTTP(w, r)

		return nil
	}))
}

func (s *APIServer) ListenAndServe(ctx context.Context) error {
	var handler http.Handler = s.Mux

	if s.CORS != nil {
		handler = CORSMiddleware(*s.CORS, s.Mux)
	}

	var loggingHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		ctx := WithLogMetadata(r.Context())

		handler.ServeHTTP(w, r.WithContext(ctx))
	}

	server := http.Server{
		Addr:              s.addr,
		Handler:           loggingHandler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	grp, gCtx := errgroup.WithContext(ctx)

	grp.Go(func() error {
		s.logger.Info("starting health server",
			"addr", s.profileAddr)

		err := s.Health.ListenAndServe(gCtx)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("health server error: %w", err)
		}

		s.logger.Info("stopped health server")

		return nil
	})

	grp.Go(func() error {
		s.logger.Info("starting API server",
			"addr", s.addr)

		err := ListenAndServeContext(ctx, &server, 10*time.Second)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("API server error: %w", err)
		}

		s.logger.Info("stopped API server")

		return nil
	})

	return grp.Wait() //nolint: wrapcheck
}

// ServiceAuth is used to control behaviour when an unauthorized client makes a
// call to the service.
type ServiceAuth bool

const (
	// ServiceAuthRequired respond with a Twirp Unauthenticated error for
	// unauthorized calls.
	ServiceAuthRequired ServiceAuth = true
	// ServiceAuthOptional allow unauthorized calls, invalid authorizations
	// will still result in an error, but calls missing authorization will
	// be let through to the service implementation.
	ServiceAuthOptional ServiceAuth = false
)

// NewDefaultServiceOptions sets up the standard options for our Twirp
// services. This sets up authentication, logging and metrics. Apply the options
// to your Twirp servers using the ServerOptions() method.
func NewDefaultServiceOptions(
	logger *slog.Logger,
	parser AuthInfoParser,
	reg prometheus.Registerer,
	requireAuth ServiceAuth,
) (ServiceOptions, error) {
	so := ServiceOptions{
		JSONSkipDefaults: true,
	}

	so.SetAuthInfoValidation(parser, requireAuth)
	so.AddLoggingHooks(logger)

	err := so.AddMetricsHooks(reg)
	if err != nil {
		return ServiceOptions{}, fmt.Errorf("set up metrics: %w", err)
	}

	return so, nil
}

type ServiceOptions struct {
	Hooks          *twirp.ServerHooks
	AuthMiddleware func(
		w http.ResponseWriter, r *http.Request, next http.Handler,
	) error

	// JSONSkipDefaults configures JSON serialization to skip unpopulated or
	// default values in JSON responses, which results in smaller responses
	// that are easier to read if your messages contain lots of fields that
	// may have their default/zero value.
	JSONSkipDefaults bool
}

// ServerOptions returns a ServerOptions function that configures the twirp
// server according to the set service options.
func (so *ServiceOptions) ServerOptions() twirp.ServerOption {
	return func(opts *twirp.ServerOptions) {
		twirp.WithServerJSONSkipDefaults(so.JSONSkipDefaults)(opts)
		twirp.WithServerHooks(so.Hooks)(opts)
	}
}

func (so *ServiceOptions) AddLoggingHooks(
	logger *slog.Logger,
) {
	so.Hooks = twirp.ChainHooks(LoggingHooks(logger), so.Hooks)
}

func (so *ServiceOptions) AddMetricsHooks(reg prometheus.Registerer) error {
	hooks, err := NewTwirpMetricsHooks(WithTwirpMetricsRegisterer(reg))
	if err != nil {
		return err
	}

	so.Hooks = twirp.ChainHooks(so.Hooks, hooks)

	return nil
}

func (so *ServiceOptions) SetAuthInfoValidation(
	parser AuthInfoParser, requireAuth ServiceAuth,
) {
	so.AuthMiddleware = func(
		w http.ResponseWriter, r *http.Request, next http.Handler,
	) error {
		ctx, _ := twirp.WithHTTPRequestHeaders(
			r.Context(),
			http.Header{
				"Authorization": r.Header["Authorization"],
			},
		)

		next.ServeHTTP(w, r.WithContext(ctx))

		return nil
	}

	hooks := twirp.ServerHooks{
		RequestRouted: func(ctx context.Context) (context.Context, error) {
			headers, ok := twirp.HTTPRequestHeaders(ctx)
			if !ok {
				return ctx, twirp.InternalError(
					"missing HTTP header context information")
			}

			auth, err := parser.AuthInfoFromHeader(headers.Get("Authorization"))
			if errors.Is(err, ErrNoAuthorization) {
				if requireAuth {
					return ctx, twirp.Unauthenticated.Error(
						"authentication required")
				}
			} else if err != nil {
				return ctx, twirp.PermissionDenied.Errorf(
					"invalid authorization: %v", err)
			} else if auth == nil {
				return ctx, twirp.InternalError(
					"invalid auth info parser response")
			}

			if auth != nil {
				ctx = SetAuthInfo(ctx, auth)

				SetLogMetadata(ctx,
					LogKeySubject, auth.Claims.Subject,
				)
			}

			return ctx, nil
		},
	}

	if so.Hooks != nil {
		so.Hooks = twirp.ChainHooks(so.Hooks, &hooks)
	} else {
		so.Hooks = &hooks
	}
}
