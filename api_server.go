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
	"github.com/ttab/elephantine/internal/rpcmetrics"
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
	testServer.Config.Protocols = PlaintextProtocols()

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
		r = withRPCStack(r, rpcStackTwirp)

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
		r = withRPCStack(r, rpcStackConnect)

		if opt.AuthMiddleware != nil {
			return opt.AuthMiddleware(w, r, h)
		}

		h.ServeHTTP(w, r)

		return nil
	}))
}

// PlaintextProtocols is the protocol set the plaintext listener serves:
// HTTP/1.1 and HTTP/2 without TLS. Go only negotiates HTTP/2 through the TLS
// ALPN handshake, so a listener that does not say this serves HTTP/1.1 only,
// and gRPC — which Connect serves on the same path as everything else, and
// which requires HTTP/2 — cannot be spoken to it at all. The two are told
// apart by the HTTP/2 connection preface, so Twirp, SSE, the websocket
// upgrade and every other HTTP/1.1 caller are unaffected.
//
// APIServer sets it on its own listener. It is exported for the services that
// serve their RPCs from an http.Server of their own, which have to say the
// same thing or lose gRPC without any error to say so.
func PlaintextProtocols() *http.Protocols {
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
			Protocols:         PlaintextProtocols(),
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
	// with its default options, which also omits unpopulated fields.
	//
	// Note that the two stacks do not spell field names the same way:
	// Twirp's JSON uses the proto names (document_uuid) and Connect's the
	// protojson default of lowerCamelCase (documentUuid). Both accept
	// either spelling in a request.
	JSONSkipDefaults bool

	// refusalObserver logs and counts the requests the authentication
	// middleware answers itself. It is a pointer so that it is shared by
	// every copy of the options value, whichever order the Set and Add
	// methods were called in.
	refusalObserver *refusalObserver
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

	so.refusals().logger = logger
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

	// The authentication middleware answers a refused request before it
	// reaches either stack, so it reports that response itself.
	collectors, err := rpcmetrics.New(opt.reg)
	if err != nil {
		return fmt.Errorf("declare the RPC metrics: %w", err)
	}

	refusals := so.refusals()

	refusals.metrics = collectors
	refusals.customer = opt.contextCustomer

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
// Authorization header, puts the resulting AuthInfo on the request context,
// where both stacks read it with GetAuthInfo, and answers a request it could
// not authenticate itself, with an unauthenticated error rendered in the
// protocol the caller is speaking. Connect, gRPC and gRPC-Web errors are
// rendered by connect.ErrorWriter and Twirp errors by twirp.WriteError, so the
// caller sees the error body its own client parses.
//
// Both a missing and an invalid authorization are unauthenticated: the caller
// could not be identified either way. ServiceAuthOptional lets a request
// without an Authorization header through as an anonymous caller; an
// authorization the parser rejects always fails.
//
// The Twirp hook and the Connect interceptor installed here are the safety net
// for a mount that does not run the middleware: they refuse a call that reaches
// a handler with no authenticated caller on its context when the service
// requires authentication, rather than letting it run unauthenticated.
func (so *ServiceOptions) SetAuthInfoValidation(
	parser AuthInfoParser, requireAuth ServiceAuth,
) {
	var (
		refusals    = so.refusals()
		errorWriter = connect.NewErrorWriter()
	)

	so.AuthMiddleware = func(
		w http.ResponseWriter, r *http.Request, next http.Handler,
	) error {
		ctx, err := authenticateRequest(r.Context(), parser, requireAuth,
			r.Header.Get("Authorization"))
		if err != nil {
			refusals.refuse(errorWriter, w, r, err)

			return nil
		}

		next.ServeHTTP(w, r.WithContext(ctx))

		return nil
	}

	hooks := twirp.ServerHooks{
		RequestRouted: func(ctx context.Context) (context.Context, error) {
			return ctx, rpc.ToTwirp(missingAuthError(ctx, requireAuth))
		},
	}

	if so.Hooks != nil {
		so.Hooks = twirp.ChainHooks(so.Hooks, &hooks)
	} else {
		so.Hooks = &hooks
	}

	so.Interceptors = append(so.Interceptors,
		rpc.AuthInfoInterceptor(bool(requireAuth)))
}

// authenticateRequest parses the authorization header and returns a context
// carrying the caller's AuthInfo, or the error the caller is to be answered
// with. A service that allows anonymous callers gets the context unchanged and
// no error when the header is missing.
func authenticateRequest(
	ctx context.Context,
	parser AuthInfoParser, requireAuth ServiceAuth,
	authorization string,
) (context.Context, error) {
	info, err := parser.AuthInfoFromHeader(authorization)

	switch {
	case errors.Is(err, ErrNoAuthorization):
		if requireAuth {
			return ctx, rpc.Unauthenticated("authentication required")
		}

		return ctx, nil
	case err != nil:
		return ctx, rpc.Errorf(connect.CodeUnauthenticated,
			"invalid authorization: %v", err)
	case info == nil:
		return ctx, rpc.Internalf("invalid auth info parser response")
	}

	ctx = SetAuthInfo(ctx, info)

	SetLogMetadata(ctx, LogKeySubject, info.Claims.Subject)

	return ctx, nil
}

// missingAuthError is the error a call that reached a handler chain without an
// authenticated caller is refused with, for a service that requires
// authentication. The middleware answers such a request itself, so this only
// fires for a mount that does not run the middleware.
func missingAuthError(ctx context.Context, requireAuth ServiceAuth) error {
	if !requireAuth {
		return nil
	}

	_, ok := auth.GetInfo(ctx)
	if ok {
		return nil
	}

	return rpc.Unauthenticated("authentication required")
}

// refusals returns the observer for the requests the authentication middleware
// answers itself, creating it if the options do not have one yet. It is a
// pointer so that every copy of a ServiceOptions value shares it, and so that
// AddLoggingHooks and AddMetricsHooks can fill it in whichever order they are
// called in.
func (so *ServiceOptions) refusals() *refusalObserver {
	if so.refusalObserver == nil {
		so.refusalObserver = &refusalObserver{}
	}

	return so.refusalObserver
}

// refusalObserver logs and counts the requests the authentication middleware
// answers before they reach a handler. Those requests never reach a Twirp hook
// or a Connect interceptor, so this is what keeps a refused call visible in the
// logs and in rpc_responses_total and rpc_protocol_responses_total, exactly as
// it was when the stacks rendered the error themselves. It deliberately does
// not touch rpc_requests_total: a refused call has always been counted as a
// response and not as a request.
type refusalObserver struct {
	logger   *slog.Logger
	metrics  *rpcmetrics.Metrics
	customer func(ctx context.Context) string
}

// refuse answers the request with the error, in the protocol the caller is
// speaking, and observes the response.
func (o *refusalObserver) refuse(
	errorWriter *connect.ErrorWriter,
	w http.ResponseWriter, r *http.Request, err error,
) {
	protocol := requestProtocol(r)

	o.observe(r, protocol, err)

	var writeErr error

	if protocol == rpcmetrics.ProtocolTwirp {
		writeErr = twirp.WriteError(w, rpc.ToTwirp(err))
	} else {
		writeErr = errorWriter.Write(w, r, err)
	}

	if writeErr != nil && o.logger != nil {
		o.logger.ErrorContext(r.Context(),
			"write the RPC error response",
			LogKeyError, writeErr.Error())
	}
}

// observe logs and counts a refused request the way the logging and metrics
// interceptors log and count a refused call.
func (o *refusalObserver) observe(
	r *http.Request, protocol string, err error,
) {
	ctx := r.Context()

	service, method, named := rpcmetrics.SplitProcedure(r.URL.Path)
	if named {
		SetLogMetadata(ctx, LogKeyService, service)
		SetLogMetadata(ctx, LogKeyMethod, method)
	}

	if o.logger != nil {
		rpc.LogErrorResponse(ctx, o.logger, err)
	}

	if o.metrics == nil || !named {
		return
	}

	var customer string

	if o.customer != nil {
		customer = o.customer(ctx)
	}

	// rpc.ResponseStatus is the Connect status mapping, which the codes the
	// middleware refuses a request with — unauthenticated, and internal for
	// a broken parser — are answered with on both stacks.
	o.metrics.Responses.WithLabelValues(
		service, method, rpc.ResponseStatus(err, protocol), customer,
	).Inc()

	// The caller is by definition unauthenticated, so there is no client
	// id to report it under.
	o.metrics.ProtocolResponses.WithLabelValues(
		service, method, protocol, rpc.ResponseCode(err).String(), "",
	).Inc()
}

// requestProtocol reports the RPC protocol a request is speaking, which decides
// both how an error is rendered to it and what protocol label the response is
// counted under. gRPC and gRPC-Web are told by their content type; the rest is
// decided by the mount, since Twirp and Connect share the content types
// application/json and application/proto.
func requestProtocol(r *http.Request) string {
	contentType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	contentType = strings.ToLower(strings.TrimSpace(contentType))

	switch {
	case strings.HasPrefix(contentType, "application/grpc-web"):
		return rpcmetrics.ProtocolGRPCWeb
	case strings.HasPrefix(contentType, "application/grpc"):
		return rpcmetrics.ProtocolGRPC
	}

	switch rpcStackOf(r.Context()) {
	case rpcStackTwirp:
		return rpcmetrics.ProtocolTwirp
	case rpcStackConnect:
		return rpcmetrics.ProtocolConnect
	case rpcStackUnknown:
	}

	// A service that mounts its RPCs on its own router and calls the
	// middleware itself marks neither, so fall back to the path: a Connect
	// procedure is "/<pkg>.<Service>/<Method>" and nothing more, while a
	// Twirp path carries a prefix ahead of it.
	if strings.Count(strings.Trim(r.URL.Path, "/"), "/") > 1 {
		return rpcmetrics.ProtocolTwirp
	}

	return rpcmetrics.ProtocolConnect
}

// rpcStack names the protocol family a request was mounted under, so that the
// authentication middleware can answer a request it refuses in a protocol the
// caller understands. RegisterAPI marks its mounts as Twirp and RegisterConnect
// marks its own as Connect.
type rpcStack int

const (
	rpcStackUnknown rpcStack = iota
	rpcStackTwirp
	rpcStackConnect
)

type rpcStackCtxKey struct{}

// withRPCStack returns the request with the protocol family of its mount
// recorded on the context.
func withRPCStack(r *http.Request, stack rpcStack) *http.Request {
	return r.WithContext(context.WithValue(
		r.Context(), rpcStackCtxKey{}, stack))
}

// rpcStackOf returns the protocol family of the mount the request came in on,
// or rpcStackUnknown for a request the service mounted itself.
func rpcStackOf(ctx context.Context) rpcStack {
	stack, _ := ctx.Value(rpcStackCtxKey{}).(rpcStack)

	return stack
}
