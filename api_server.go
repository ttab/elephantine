package elephantine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine/internal/auth"
	"github.com/ttab/elephantine/rpc"
	"github.com/twitchtv/twirp"
	"golang.org/x/sync/errgroup"
)

// APIServerOption configures an APIServer when passed to NewAPIServer or
// NewTestAPIServer.
type APIServerOption func(s *APIServer)

func APIServerCORSHosts(hosts ...string) APIServerOption {
	return func(s *APIServer) {
		s.CORS.Hosts = hosts
	}
}

// APIServerPublicCORS marks request path prefixes as open to any origin:
// requests under them are answered with "Access-Control-Allow-Origin: *" and
// no "Vary: Origin", and their preflights succeed whatever the Origin header
// is. Paths outside the prefixes keep the origin allowlist from
// APIServerCORSHosts.
//
// Use it for anonymous read surfaces, typically served through a CDN, where
// the per-origin response and the Vary header only fragment the cache. See
// CORSOptions.PublicPrefixes for the full semantics and the caveat about paths
// that read credentials. Prefixes are matched literally, so pass "/public/"
// rather than "/public".
func APIServerPublicCORS(prefixes ...string) APIServerOption {
	return func(s *APIServer) {
		s.CORS.PublicPrefixes = append(s.CORS.PublicPrefixes, prefixes...)
	}
}

// DefaultMaxBodyBytes is the request body limit an APIServer applies when
// APIServerMaxBodyBytes isn't used.
//
// The number is picked from what our services actually send. The routine Twirp
// body is a document write: a news document with its blocks, metadata and
// links serialises to tens of kilobytes of JSON, and the largest we see stay
// well under a megabyte. The outliers are the RPCs that carry file content
// inline as a protobuf bytes field — elephant-hub's PublishVersion and
// BulkPublishVersion ship manifests and assets that way — where the JSON
// encoding adds a third on top for base64. Eight mebibytes leaves an order of
// magnitude of headroom over the first case and room for a multi-megabyte
// bundle in the second.
//
// It matters because Twirp buffers the whole request body in memory and then
// unmarshals it, so an unbounded body is an unbounded allocation per in-flight
// request: the limit is what one caller can make a replica hold.
//
// A service that serves real file uploads on the API listener — elephant-hub's
// CI publish endpoint takes multipart bodies up to 64 MiB — has to raise this
// with APIServerMaxBodyBytes.
const DefaultMaxBodyBytes int64 = 8 << 20

// APIServerMaxBodyBytes limits the size of a request body accepted by the API
// listener, overriding DefaultMaxBodyBytes. A request that declares a larger
// Content-Length is refused with 413 before it reaches a handler, and a
// request that streams past the limit fails on read.
//
// Pass a value of zero or less to turn the limit off. Do that only for a
// listener that has to accept genuinely large uploads, and prefer raising the
// limit to removing it.
func APIServerMaxBodyBytes(n int64) APIServerOption {
	return func(s *APIServer) {
		s.maxBodyBytes = n
	}
}

// MaxBodyBytesMiddleware caps the request bodies passed to the wrapped
// handler at n bytes. A request that declares a larger Content-Length is
// refused with 413 without being read; anything else gets a
// http.MaxBytesReader body, so a chunked or lying request fails when the
// handler reads past the limit. A limit of zero or less is no limit.
func MaxBodyBytesMiddleware(n int64, handler http.Handler) http.Handler {
	if n <= 0 {
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > n {
			http.Error(w,
				"request body too large",
				http.StatusRequestEntityTooLarge)

			return
		}

		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}

		handler.ServeHTTP(w, r)
	})
}

func APIServerTLS(addr string, certFile string, keyFile string) APIServerOption {
	return func(s *APIServer) {
		s.tlsAddr = addr
		s.certFile = certFile
		s.keyFile = keyFile
	}
}

// APIServerVersion sets the application version string reported by the
// /version endpoint. If not provided, the version falls back to
// debug.BuildInfo.Main.Version (which is "(devel)" for plain `go build`).
func APIServerVersion(version string) APIServerOption {
	return func(s *APIServer) {
		s.appVersion = version
	}
}

// APIServerModules adds module paths to the /version endpoint's module list.
// The defaults (github.com/ttab/elephantine, github.com/ttab/elephant-api,
// github.com/ttab/elephant-tt-api) are always included; modules passed here
// are appended.
func APIServerModules(modules ...string) APIServerOption {
	return func(s *APIServer) {
		s.modules = append(s.modules, modules...)
	}
}

func NewAPIServer(
	logger *slog.Logger,
	addr string, profileAddr string,
	opts ...APIServerOption,
) *APIServer {
	health := NewHealthServer(logger, profileAddr)

	return newAPIServer(
		logger,
		false,
		addr,
		profileAddr,
		&handlerWrapper{},
		health,
		opts...,
	)
}

// Cleaner is the subset of testing.TB used to register cleanup callbacks,
// satisfied by *testing.T. NewTestAPIServer uses it to tear down the test
// server when the test finishes.
type Cleaner interface {
	Cleanup(fn func())
}

func NewTestAPIServer(
	t Cleaner,
	logger *slog.Logger,
	opts ...APIServerOption,
) (*APIServer, *http.Client) {
	handler := handlerWrapper{
		// Will be replaced when the server starts up, only here to
		// answer any early requests with 404:s.
		Handler: http.NewServeMux(),
	}

	// Unstarted, so that the test server serves the protocols the real
	// plaintext listener serves. Without it a test cannot reach the Connect
	// handler over gRPC.
	testServer := httptest.NewUnstartedServer(&handler)
	testServer.Config.Protocols = plaintextProtocols()

	testServer.Start()

	healthServer := NewTestHealthServer(logger)

	t.Cleanup(func() {
		testServer.CloseClientConnections()
		testServer.Close()
		_ = healthServer.Close()
	})

	return newAPIServer(logger, true,
		testServer.Listener.Addr().String(),
		healthServer.Addr(), &handler, healthServer,
		opts...,
	), testServer.Client()
}

type handlerWrapper struct {
	Handler http.Handler
}

func (h *handlerWrapper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Handler.ServeHTTP(w, r)
}

func newAPIServer(
	logger *slog.Logger, testServer bool,
	addr string, profileAddr string,
	handler *handlerWrapper, health *HealthServer,
	opts ...APIServerOption,
) *APIServer {
	s := APIServer{
		testServer:   testServer,
		logger:       logger,
		addr:         addr,
		profileAddr:  profileAddr,
		handler:      handler,
		maxBodyBytes: DefaultMaxBodyBytes,
		Mux:          http.NewServeMux(),
		Health:       health,
		CORS: &CORSOptions{
			AllowInsecure:          false,
			AllowInsecureLocalhost: true,
			Hosts:                  []string{"localhost", "tt.se"},
			AllowedMethods:         []string{"GET", "POST"},
			AllowedHeaders: []string{
				"Authorization", "Content-Type",
				// Sent by every Connect client, and turned
				// into a handler deadline.
				"Connect-Protocol-Version",
				"Connect-Timeout-Ms",
			},
		},
	}

	for _, o := range opts {
		o(&s)
	}

	s.Mux.Handle("GET /health/alive", http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)

		_, _ = fmt.Fprintln(w, "I AM ALIVE!")
	}))

	s.Mux.Handle("GET /version", versionHandler(buildBuildInfo(
		s.appVersion,
		append(append([]string{}, defaultVersionModules...), s.modules...),
	)))

	s.Health.AddReadyFunction("api_liveness",
		LivenessReadyCheck(s.AliveEndpoint()))

	return &s
}

// APIServer is a HTTP server for our APIs that bundles a request mux, a health
// server, and CORS handling. Construct it with NewAPIServer (or
// NewTestAPIServer for tests), register services with RegisterAPI(s), and
// start it with ListenAndServe.
type APIServer struct {
	testServer bool

	logger      *slog.Logger
	addr        string
	profileAddr string
	tlsAddr     string
	certFile    string
	keyFile     string
	handler     *handlerWrapper

	appVersion   string
	modules      []string
	maxBodyBytes int64

	Mux    *http.ServeMux
	Health *HealthServer
	CORS   *CORSOptions
}

func (s *APIServer) Addr() string {
	addr := s.addr
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}

	return addr
}

func (s *APIServer) AliveEndpoint() string {
	return fmt.Sprintf(
		"http://%s/health/alive",
		s.Addr(),
	)
}

// APIServiceHandler is implemented by the generated Twirp service handlers. It
// is a http.Handler that also reports the path prefix the service should be
// mounted on.
type APIServiceHandler interface {
	http.Handler

	PathPrefix() string
}

func (s *APIServer) RegisterAPIs(
	opt ServiceOptions, apis ...APIServiceHandler,
) {
	for _, api := range apis {
		s.RegisterAPI(api, opt)
	}
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

// RegisterConnect mounts a Connect handler behind the same authentication
// middleware as the Twirp services. Pass it the path and handler a generated
// New<Service>ServiceHandler returns:
//
//	server.RegisterConnect(documentsv1connect.NewDocumentsServiceHandler(
//		svc, opt.HandlerOptions()...))
//
// The path is a subtree, and no method is bound, since Connect serves gRPC and
// gRPC-Web on the same path and may answer GET for the RPCs that declare
// themselves free of side effects.
func (s *APIServer) RegisterConnect(
	path string, h http.Handler, opt ServiceOptions,
) {
	s.Mux.Handle(path, HTTPErrorHandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) error {
		if opt.AuthMiddleware != nil {
			return opt.AuthMiddleware(w, r, h)
		}

		h.ServeHTTP(w, r)

		return nil
	}))
}

// plaintextProtocols is the protocol set the plaintext listener serves:
// HTTP/1.1 and HTTP/2 without TLS. Go only negotiates HTTP/2 through the TLS
// ALPN handshake, so a listener that does not say this serves HTTP/1.1 only,
// and gRPC — which Connect serves on the same path as everything else, and
// which requires HTTP/2 — cannot be spoken to it at all. The two are told
// apart by the HTTP/2 connection preface, so Twirp, SSE, the websocket
// upgrade and every other HTTP/1.1 caller are unaffected.
func plaintextProtocols() *http.Protocols {
	var p http.Protocols

	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)

	return &p
}

func (s *APIServer) ListenAndServe(ctx context.Context) error {
	var handler http.Handler = s.Mux

	if s.CORS != nil {
		handler = CORSMiddleware(*s.CORS, s.Mux)
	}

	// Outermost, so that everything in the chain sees a bounded body.
	handler = MaxBodyBytesMiddleware(s.maxBodyBytes, handler)

	var loggingHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		ctx := WithLogMetadata(r.Context())

		handler.ServeHTTP(w, r.WithContext(ctx))
	}

	// Test servers are started from the get-go.
	if s.testServer {
		s.handler.Handler = loggingHandler

		return nil
	}

	grp, gCtx := errgroup.WithContext(ctx)

	if s.profileAddr != "" {
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
	}

	grp.Go(func() error {
		s.logger.Info("starting API server",
			"addr", s.addr)

		server := http.Server{
			Addr:              s.addr,
			Handler:           loggingHandler,
			ReadHeaderTimeout: 5 * time.Second,
			Protocols:         plaintextProtocols(),
		}

		err := ListenAndServeContext(ctx, &server, 10*time.Second)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("API server error: %w", err)
		}

		s.logger.Info("stopped API server")

		return nil
	})

	grp.Go(func() error {
		if s.tlsAddr == "" || s.certFile == "" || s.keyFile == "" {
			return nil
		}

		s.logger.Info("starting TLS API server",
			"addr", s.tlsAddr)

		server := http.Server{
			Addr:              s.tlsAddr,
			Handler:           loggingHandler,
			ReadHeaderTimeout: 5 * time.Second,
		}

		err := ListenAndServeContext(
			ctx, &server,
			10*time.Second,
			ListenAndServeTLS(s.logger, s.certFile, s.keyFile),
		)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("TLS API server error: %w", err)
		}

		s.logger.Info("stopped TLS API server")

		return nil
	})

	return grp.Wait() //nolint: wrapcheck
}

// ServiceAuth is used to control behaviour when an unauthorized client makes a
// call to the service.
type ServiceAuth bool

const (
	// ServiceAuthRequired respond with an unauthenticated error for
	// unauthorized calls.
	ServiceAuthRequired ServiceAuth = true
	// ServiceAuthOptional allow unauthorized calls, invalid authorizations
	// will still result in an error, but calls missing authorization will
	// be let through to the service implementation.
	ServiceAuthOptional ServiceAuth = false
)

// NewDefaultServiceOptions sets up the standard options for our RPC services.
// This sets up authentication, logging and metrics for both stacks: apply the
// options to a Twirp server with the ServerOptions() method, and to a Connect
// handler with HandlerOptions().
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

// ServiceOptions carries the Twirp server hooks, the Connect interceptors and
// the authentication middleware applied to the API services registered with an
// APIServer. Use NewDefaultServiceOptions for the standard setup, or compose it
// manually with the Add*/Set* methods, and apply it to a Twirp server with
// ServerOptions and to a Connect handler with HandlerOptions.
type ServiceOptions struct {
	Hooks          *twirp.ServerHooks
	Interceptors   []connect.Interceptor
	AuthMiddleware func(
		w http.ResponseWriter, r *http.Request, next http.Handler,
	) error

	// JSONSkipDefaults configures JSON serialization to skip unpopulated or
	// default values in JSON responses, which results in smaller responses
	// that are easier to read if your messages contain lots of fields that
	// may have their default/zero value.
	//
	// It only affects the Twirp mount. Connect's JSON codec is protojson
	// with its default options, which already omit unpopulated fields, so
	// the two stacks produce the same JSON either way.
	JSONSkipDefaults bool
}

// ServerOptions returns a ServerOptions function that configures the twirp
// server according to the set service options.
//
// The Twirp error interceptor is always installed, so that a handler that has
// been moved to the Connect error vocabulary answers a Twirp caller with the
// code, message and meta it always has. It is a no-op for a handler that still
// returns Twirp errors.
func (so *ServiceOptions) ServerOptions() twirp.ServerOption {
	return func(opts *twirp.ServerOptions) {
		twirp.WithServerJSONSkipDefaults(so.JSONSkipDefaults)(opts)
		twirp.WithServerHooks(so.Hooks)(opts)
		twirp.WithServerInterceptors(rpc.TwirpInterceptor())(opts)
	}
}

// HandlerOptions returns the options that configure a Connect handler according
// to the set service options. It is the Connect counterpart of ServerOptions,
// and is passed to the generated New<Service>ServiceHandler constructor.
func (so *ServiceOptions) HandlerOptions() []connect.HandlerOption {
	if len(so.Interceptors) == 0 {
		return nil
	}

	return []connect.HandlerOption{
		connect.WithInterceptors(so.Interceptors...),
	}
}

func (so *ServiceOptions) AddLoggingHooks(
	logger *slog.Logger,
) {
	so.Hooks = twirp.ChainHooks(loggingHooks(logger), so.Hooks)

	so.addInterceptor(rpc.LoggingInterceptor(logger))
}

// AddMetricsHooks adds the RPC metrics to both stacks. The options are the ones
// that configure the Twirp hooks; the customer function and the test latency
// are passed on to the Connect interceptor so that the two report the same
// label values, and WithTwirpMetricsRegisterer overrides the registerer for
// both.
func (so *ServiceOptions) AddMetricsHooks(
	reg prometheus.Registerer, opts ...TwirpMetricOptionFunc,
) error {
	opt := TwirpMetricsOptions{
		reg: reg,
		contextCustomer: func(_ context.Context) string {
			return ""
		},
	}

	for i := range opts {
		opts[i](&opt)
	}

	hooks, err := newTwirpMetricsHooks(
		WithTwirpMetricsRegisterer(opt.reg),
		WithTwirpMetricsStaticTestLatency(opt.testLatency),
		WithTwirpMetricsCustomerFunc(opt.contextCustomer),
	)
	if err != nil {
		return err
	}

	interceptor, err := rpc.MetricsInterceptor(opt.reg,
		rpc.WithMetricsCustomerFunc(opt.contextCustomer),
		rpc.WithMetricsStaticTestLatency(opt.testLatency),
	)
	if err != nil {
		return fmt.Errorf("create the Connect metrics interceptor: %w", err)
	}

	so.Hooks = twirp.ChainHooks(so.Hooks, hooks)

	so.addInterceptor(interceptor)

	return nil
}

// addInterceptor puts an interceptor outermost in the chain, so that the
// interceptors added last wrap the ones added before them. That is what makes
// the metrics interceptor observe the error the authentication interceptor
// returns, the way the Twirp response hook observes it.
func (so *ServiceOptions) addInterceptor(i connect.Interceptor) {
	so.Interceptors = append([]connect.Interceptor{i}, so.Interceptors...)
}

// SetAuthInfoValidation makes the service authenticate its callers with the
// parser.
//
// Authentication is protocol neutral HTTP middleware: it parses the
// Authorization header, and puts the resulting AuthInfo on the request context,
// where both stacks read it with GetAuthInfo. The middleware cannot know which
// protocol the caller is speaking, and so cannot render an error body itself.
// A request it could not authenticate is therefore let through with the reason
// recorded on the context, and the Twirp hook and the Connect interceptor
// installed here turn that into a coded error before the handler runs: a
// missing authorization is unauthenticated, an invalid one is permission
// denied, exactly as before.
func (so *ServiceOptions) SetAuthInfoValidation(
	parser AuthInfoParser, requireAuth ServiceAuth,
) {
	so.AuthMiddleware = func(
		w http.ResponseWriter, r *http.Request, next http.Handler,
	) error {
		ctx := authenticateRequest(r.Context(), parser, requireAuth,
			r.Header.Get("Authorization"))

		next.ServeHTTP(w, r.WithContext(ctx))

		return nil
	}

	hooks := twirp.ServerHooks{
		RequestRouted: func(ctx context.Context) (context.Context, error) {
			return ctx, rpc.ToTwirp(auth.GetError(ctx))
		},
	}

	if so.Hooks != nil {
		so.Hooks = twirp.ChainHooks(so.Hooks, &hooks)
	} else {
		so.Hooks = &hooks
	}

	so.Interceptors = append(so.Interceptors, authErrorInterceptor())
}

// authenticateRequest parses the authorization header and returns a context
// that either carries the caller's AuthInfo or the reason it does not.
func authenticateRequest(
	ctx context.Context,
	parser AuthInfoParser, requireAuth ServiceAuth,
	authorization string,
) context.Context {
	info, err := parser.AuthInfoFromHeader(authorization)

	switch {
	case errors.Is(err, ErrNoAuthorization):
		if requireAuth {
			return auth.SetError(ctx,
				rpc.Unauthenticated("authentication required"))
		}

		return ctx
	case err != nil:
		return auth.SetError(ctx, rpc.Errorf(
			connect.CodePermissionDenied,
			"invalid authorization: %v", err))
	case info == nil:
		return auth.SetError(ctx, rpc.Internalf(
			"invalid auth info parser response"))
	}

	ctx = SetAuthInfo(ctx, info)

	SetLogMetadata(ctx, LogKeySubject, info.Claims.Subject)

	return ctx
}

// authErrorInterceptor fails a Connect call that the authentication middleware
// could not authenticate, with the error the middleware recorded.
func authErrorInterceptor() connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(
			ctx context.Context, req connect.AnyRequest,
		) (connect.AnyResponse, error) {
			err := auth.GetError(ctx)
			if err != nil {
				return nil, err
			}

			return next(ctx, req)
		}
	})
}
