package elephantine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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
	}

	s.Mux.Handle("GET /health/alive", http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)

		_, _ = fmt.Fprintln(w, "I AM ALIVE!")
	}))

	healthServer := NewHealthServer(logger, profileAddr)

	healthServer.AddReadyFunction("api_liveness",
		LivenessReadyCheck(s.AliveEndpoint()))

	return &s
}

type APIServer struct {
	logger      *slog.Logger
	addr        string
	profileAddr string
	Mux         *http.ServeMux
	Health      *HealthServer
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
	var handler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		ctx := WithLogMetadata(r.Context())

		s.Mux.ServeHTTP(w, r.WithContext(ctx))
	}

	server := http.Server{
		Addr:              s.addr,
		Handler:           handler,
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

func NewDefaultServiceOptions(
	logger *slog.Logger, parser *AuthInfoParser,
) ServiceOptions {
	var so ServiceOptions

	so.SetJWTValidation(parser, true)
	so.AddLoggingHooks(logger, nil)

	return so
}

type ServiceOptions struct {
	Hooks          *twirp.ServerHooks
	AuthMiddleware func(
		w http.ResponseWriter, r *http.Request, next http.Handler,
	) error
}

func (so *ServiceOptions) AddLoggingHooks(
	logger *slog.Logger, scopesFunc func(context.Context) string,
) {
	if scopesFunc == nil {
		scopesFunc = func(ctx context.Context) string {
			auth, ok := GetAuthInfo(ctx)
			if !ok {
				return ""
			}

			return auth.Claims.Scope
		}
	}

	so.Hooks = twirp.ChainHooks(LoggingHooks(logger, scopesFunc), so.Hooks)
}

func (so *ServiceOptions) SetJWTValidation(
	parser *AuthInfoParser, requireAuth bool,
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
