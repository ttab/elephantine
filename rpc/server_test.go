package rpc_test

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/internal/rpcmetrics"
	"github.com/ttab/elephantine/internal/testservice"
	"github.com/ttab/elephantine/internal/testservice/testserviceconnect"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/elephantine/test"
)

// echoMessage is the message the fixture calls echo.
const echoMessage = "hello"

// testIssuer is the token issuer the test stacks trust.
const testIssuer = "test"

// logKeyLevel is the key the recording log handler reports the level under.
const logKeyLevel = "level"

// testImpl implements the fixture service against the plain protobuf
// interface, which is the interface every elephant service implements.
type testImpl struct{}

func (testImpl) Echo(
	ctx context.Context, req *testservice.EchoRequest,
) (*testservice.EchoResponse, error) {
	res := testservice.EchoResponse{
		Message:     req.GetMessage(),
		LogMetadata: map[string]string{},
	}

	info, ok := rpc.GetAuthInfo(ctx)
	if ok {
		res.Subject = info.Claims.Subject
	}

	for key, value := range elephantine.GetLogMetadata(ctx) {
		res.LogMetadata[key] = fmt.Sprint(value)
	}

	return &res, nil
}

// codeBareCancellation is the Fail code the fixture answers with a bare error
// that wraps a context cancellation, the way connect-go's own handler wrapper
// does when the request context is already done. It is not an RPC code.
const codeBareCancellation = "bare-cancellation"

func (testImpl) Fail(
	_ context.Context, req *testservice.FailRequest,
) (*testservice.FailResponse, error) {
	if req.GetCode() == codeBareCancellation {
		return nil, fmt.Errorf("do the work: %w", context.Canceled)
	}

	var code connect.Code

	err := code.UnmarshalText([]byte(req.GetCode()))
	if err != nil {
		return nil, rpc.InvalidArgument("code", "is not an RPC code")
	}

	return nil, rpc.WithMeta(
		rpc.Errorf(code, "the call failed on purpose"),
		"reason", "the test asked for it")
}

// scopedImpl requires a scope before doing the work, so that the scope check
// can be exercised over both protocols.
type scopedImpl struct {
	testImpl
}

func (s scopedImpl) Echo(
	ctx context.Context, req *testservice.EchoRequest,
) (*testservice.EchoResponse, error) {
	_, err := rpc.RequireAnyScope(ctx, "test_write", "test_admin")
	if err != nil {
		return nil, err
	}

	return s.testImpl.Echo(ctx, req)
}

// stack is a dual-stack test server: one service implementation mounted on both
// protocols behind one set of service options.
type stack struct {
	Addr     string
	Registry *prometheus.Registry
	Records  *recordingHandler
	Token    string

	key    *ecdsa.PrivateKey
	client *http.Client
}

// newStack starts an API server serving the implementation over both Twirp and
// Connect, with the standard service options.
func newStack(
	t *testing.T,
	impl testservice.Test, requireAuth elephantine.ServiceAuth,
	opts ...elephantine.TwirpMetricOptionFunc,
) *stack {
	t.Helper()

	var (
		records = &recordingHandler{}
		logger  = slog.New(records)
		reg     = prometheus.NewRegistry()
		key     = test.NewSigningKey(t)
		parser  = elephantine.NewStaticAuthInfoParser(
			t.Context(), key.PublicKey,
			elephantine.JWTAuthInfoParserOptions{Issuer: testIssuer})
	)

	so := elephantine.ServiceOptions{
		JSONSkipDefaults: true,
	}

	so.SetAuthInfoValidation(parser, requireAuth)
	so.AddLoggingHooks(logger)

	err := so.AddMetricsHooks(reg, opts...)
	test.Mustf(t, err, "set up the RPC metrics")

	srv, client := elephantine.NewTestAPIServer(t, logger)

	srv.RegisterAPI(testservice.NewTestServer(impl, so.ServerOptions()), so)

	path, handler := testserviceconnect.NewTestServiceHandler(
		impl, so.HandlerOptions()...)

	srv.RegisterConnect(path, handler, so)

	srv.RegisterConnect(streamPath,
		newStreamHandler(so.HandlerOptions()...), so)

	err = srv.ListenAndServe(t.Context())
	test.Mustf(t, err, "start the test API server")

	return &stack{
		Addr:     srv.Addr(),
		Registry: reg,
		Records:  records,
		Token:    test.AccessKey(t, key, test.Claims(t, "hugo", "test_read")),
		key:      key,
		client:   client,
	}
}

// TokenFor signs a token for the claims, so that a test can decide what the
// call is authenticated as.
func (s *stack) TokenFor(t *testing.T, claims elephantine.JWTClaims) string {
	t.Helper()

	return test.AccessKey(t, s.key, claims)
}

// HTTPClient returns a client that sends the token as a bearer token, the way a
// token source in an oauth2 client does.
func (s *stack) HTTPClient(token string) *http.Client {
	return &http.Client{
		Transport: &bearerTransport{
			token: token,
			next:  s.client.Transport,
		},
	}
}

// StreamClient returns a client for the server-streaming fixture handler.
func (s *stack) StreamClient(
	token string,
) *connect.Client[testservice.EchoRequest, testservice.EchoResponse] {
	return newStreamClient(s.HTTPClient(token), "http://"+s.Addr)
}

// Clients returns one client per protocol, all of them the plain protobuf
// interface the service is implemented against.
func (s *stack) Clients(token string) map[string]testservice.Test {
	var (
		client = s.HTTPClient(token)
		base   = "http://" + s.Addr
	)

	return map[string]testservice.Test{
		"twirp": testservice.NewTestProtobufClient(base, client),
		"connect": testserviceconnect.NewTestServiceClient(
			client, base),
	}
}

// bearerTransport adds an authorization header to every request, the way a
// token source in an oauth2 client does.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.token)
	}

	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}

	res, err := next.RoundTrip(r)
	if err != nil {
		return nil, fmt.Errorf("round trip the request: %w", err)
	}

	return res, nil
}

// recordingHandler keeps the log records so that a test can compare what the
// two stacks logged.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, r.Clone())

	return nil
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(_ string) slog.Handler { return h }

// Attributes returns the attributes of the records with the given message, one
// map per record, rendered as strings so that they can be compared.
func (h *recordingHandler) Attributes(message string) []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()

	var out []map[string]string

	for _, r := range h.records {
		if r.Message != message {
			continue
		}

		attrs := map[string]string{
			logKeyLevel: r.Level.String(),
		}

		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = fmt.Sprint(a.Value.Any())

			return true
		})

		out = append(out, attrs)
	}

	return out
}

// TestDualStackAuthentication checks that the protocol neutral authentication
// middleware answers with the same codes on both stacks, and with the codes the
// Twirp hook answered with before it moved out of the hooks.
func TestDualStackAuthentication(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	t.Run("authenticated", func(t *testing.T) {
		for name, client := range s.Clients(s.Token) {
			t.Run(name, func(t *testing.T) {
				res, err := client.Echo(t.Context(),
					&testservice.EchoRequest{Message: echoMessage})
				test.Mustf(t, err, "call Echo")

				test.Equalf(t, echoMessage, res.GetMessage(),
					"echo the message")
				test.Equalf(t, "user://test/hugo", res.GetSubject(),
					"see the authenticated subject")
			})
		}
	})

	t.Run("no_authorization", func(t *testing.T) {
		for name, client := range s.Clients("") {
			t.Run(name, func(t *testing.T) {
				_, err := client.Echo(t.Context(),
					&testservice.EchoRequest{})

				test.IsRPCError(t, err, connect.CodeUnauthenticated)
			})
		}
	})

	// An invalid authorization is unauthenticated rather than permission
	// denied: the caller could not be identified, which is the same failure
	// as not presenting a token at all. Permission denied is for a caller
	// we did identify and that is not allowed to make the call.
	t.Run("invalid_authorization", func(t *testing.T) {
		for name, client := range s.Clients("not-a-token") {
			t.Run(name, func(t *testing.T) {
				_, err := client.Echo(t.Context(),
					&testservice.EchoRequest{})

				test.IsRPCError(t, err, connect.CodeUnauthenticated)
			})
		}
	})
}

// TestDualStackOptionalAuthentication checks that a service that allows
// anonymous callers still lets them through on both stacks, and still refuses
// an authorization it cannot validate.
func TestDualStackOptionalAuthentication(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthOptional)

	for name, client := range s.Clients("") {
		t.Run(name+"_anonymous", func(t *testing.T) {
			res, err := client.Echo(t.Context(),
				&testservice.EchoRequest{Message: echoMessage})
			test.Mustf(t, err, "call Echo anonymously")

			test.Equalf(t, "", res.GetSubject(),
				"have no subject for an anonymous caller")
		})
	}

	for name, client := range s.Clients("not-a-token") {
		t.Run(name+"_invalid", func(t *testing.T) {
			_, err := client.Echo(t.Context(),
				&testservice.EchoRequest{})

			test.IsRPCError(t, err, connect.CodeUnauthenticated)
		})
	}
}

// TestDualStackLogMetadata checks that a handler sees the same log metadata
// whichever protocol the caller used.
func TestDualStackLogMetadata(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	metadata := map[string]map[string]string{}

	for name, client := range s.Clients(s.Token) {
		res, err := client.Echo(t.Context(),
			&testservice.EchoRequest{Message: echoMessage})
		test.Mustf(t, err, "call Echo over %s", name)

		metadata[name] = res.GetLogMetadata()
	}

	test.EqualDiff(t, map[string]string{
		"service": "Test",
		"method":  "Echo",
		"sub":     "user://test/hugo",
	}, metadata["twirp"], "set the log metadata on the Twirp stack")

	test.EqualDiff(t, metadata["twirp"], metadata["connect"],
		"set the same log metadata on both stacks")
}

// TestDualStackErrorParity checks that a handler returning a Connect error with
// metadata reaches a Twirp caller as the Twirp error it always was.
func TestDualStackErrorParity(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	clients := s.Clients(s.Token)

	errs := map[string]error{}

	for name, client := range clients {
		_, err := client.Fail(t.Context(),
			&testservice.FailRequest{Code: codeNotFound})
		test.MustNotf(t, err, "get an error from Fail over %s", name)

		errs[name] = err
	}

	test.ErrorParity(t, errs["twirp"], errs["connect"])

	test.EqualDiff(t, map[string]string{
		"reason": "the test asked for it",
	}, rpc.Meta(errs["connect"]), "carry the metadata over Connect")

	test.EqualDiff(t, rpc.Meta(errs["twirp"]), rpc.Meta(errs["connect"]),
		"carry the same metadata on both stacks")

	// The two stacks log the error response the same way. not_found is one
	// of the codes the protocols agree on the status for, so even the
	// status code is identical.
	logged := s.Records.Attributes("error response")

	test.Equalf(t, 2, len(logged), "log one error response per stack")
	test.EqualDiff(t, logged[0], logged[1],
		"log the error the same way on both stacks")
	test.EqualDiff(t, map[string]string{
		logKeyLevel:   "INFO",
		"err_code":    "not_found",
		"err":         "the call failed on purpose",
		"status_code": "404",
		"err_meta":    "map[reason:the test asked for it]",
	}, logged[0], "log the error with the established keys")
}

// TestDualStackScopeCheck checks that the scope check produces the same error
// on both stacks, metadata included.
func TestDualStackScopeCheck(t *testing.T) {
	s := newStack(t, scopedImpl{}, elephantine.ServiceAuthRequired)

	errs := map[string]error{}

	for name, client := range s.Clients(s.Token) {
		_, err := client.Echo(t.Context(), &testservice.EchoRequest{})
		test.MustNotf(t, err, "get an error from Echo over %s", name)

		test.IsRPCError(t, err, connect.CodePermissionDenied)

		errs[name] = err
	}

	test.ErrorParity(t, errs["twirp"], errs["connect"])

	test.EqualDiff(t, map[string]string{
		"required_any_of_scopes": "test_write test_admin",
	}, rpc.Meta(errs["connect"]), "name the accepted scopes")
}

// TestDualStackMetrics checks that the two stacks report the same series with
// the same label values, and that the protocol counter tells them apart.
func TestDualStackMetrics(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired,
		elephantine.WithTwirpMetricsCustomerFunc(func(_ context.Context) string {
			return "acme"
		}),
	)

	// The client id label comes from the token, so that the counter names
	// the applications a method still has callers in.
	claims := test.Claims(t, "hugo", "test_read")
	claims.ClientID = "eltest"

	for name, client := range s.Clients(s.TokenFor(t, claims)) {
		_, err := client.Echo(t.Context(),
			&testservice.EchoRequest{Message: echoMessage})
		test.Mustf(t, err, "call Echo over %s", name)

		_, err = client.Fail(t.Context(),
			&testservice.FailRequest{Code: codeNotFound})
		test.MustNotf(t, err, "get an error from Fail over %s", name)
	}

	// A call that never reaches a handler is a response and not a request,
	// on both stacks: the authentication middleware answers it and counts
	// the response itself, and there is no client id to count it under.
	for name, client := range s.Clients("") {
		_, err := client.Echo(t.Context(),
			&testservice.EchoRequest{Message: echoMessage})
		test.MustNotf(t, err,
			"get an error from an unauthenticated call over %s", name)
	}

	err := testutil.GatherAndCompare(s.Registry, strings.NewReader(`
# HELP rpc_requests_total Number of RPC requests received.
# TYPE rpc_requests_total counter
rpc_requests_total{customer="acme",method="Echo",service="Test"} 2
rpc_requests_total{customer="acme",method="Fail",service="Test"} 2
`), "rpc_requests_total")
	test.Mustf(t, err, "count the requests with the same labels on both stacks")

	err = testutil.GatherAndCompare(s.Registry, strings.NewReader(`
# HELP rpc_responses_total Number of RPC responses sent.
# TYPE rpc_responses_total counter
rpc_responses_total{customer="acme",method="Echo",service="Test",status="200"} 2
rpc_responses_total{customer="acme",method="Echo",service="Test",status="401"} 2
rpc_responses_total{customer="acme",method="Fail",service="Test",status="404"} 2
`), "rpc_responses_total")
	test.Mustf(t, err, "count the responses with the same labels on both stacks")

	err = testutil.GatherAndCompare(s.Registry, strings.NewReader(`
# HELP rpc_protocol_responses_total `+rpcmetrics.ProtocolResponsesHelp+`
# TYPE rpc_protocol_responses_total counter
rpc_protocol_responses_total{client_id="",code="unauthenticated",method="Echo",protocol="connect",service="Test"} 1
rpc_protocol_responses_total{client_id="",code="unauthenticated",method="Echo",protocol="twirp",service="Test"} 1
rpc_protocol_responses_total{client_id="eltest",code="not_found",method="Fail",protocol="connect",service="Test"} 1
rpc_protocol_responses_total{client_id="eltest",code="not_found",method="Fail",protocol="twirp",service="Test"} 1
rpc_protocol_responses_total{client_id="eltest",code="ok",method="Echo",protocol="connect",service="Test"} 1
rpc_protocol_responses_total{client_id="eltest",code="ok",method="Echo",protocol="twirp",service="Test"} 1
`), "rpc_protocol_responses_total")
	test.Mustf(t, err, "count the responses per protocol and code")
}

// TestConnectGRPC checks that the plaintext listener really serves gRPC, which
// is only true because it enables HTTP/2 without TLS: Go negotiates HTTP/2
// through the TLS ALPN handshake and nowhere else, so a listener that does not
// say so speaks HTTP/1.1 and gRPC cannot be spoken to it.
func TestConnectGRPC(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	var transport http.Transport

	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetUnencryptedHTTP2(true)

	client := http.Client{
		Transport: &bearerTransport{
			token: s.Token,
			next:  &transport,
		},
	}

	grpc := testserviceconnect.NewTestServiceClient(
		&client, "http://"+s.Addr, connect.WithGRPC())

	res, err := grpc.Echo(t.Context(),
		&testservice.EchoRequest{Message: echoMessage})
	test.Mustf(t, err, "call Echo over gRPC")

	test.Equalf(t, echoMessage, res.GetMessage(), "echo the message")
	test.Equalf(t, "user://test/hugo", res.GetSubject(),
		"authenticate the caller over gRPC too")

	err = testutil.GatherAndCompare(s.Registry, strings.NewReader(`
# HELP rpc_protocol_responses_total `+rpcmetrics.ProtocolResponsesHelp+`
# TYPE rpc_protocol_responses_total counter
rpc_protocol_responses_total{client_id="",code="ok",method="Echo",protocol="grpc",service="Test"} 1
`), "rpc_protocol_responses_total")
	test.Mustf(t, err, "report the response under the gRPC protocol")
}

// TestDefaultServiceOptions checks that the standard options come with both
// stacks wired up.
func TestDefaultServiceOptions(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))
	key := test.NewSigningKey(t)

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(), key.PublicKey,
		elephantine.JWTAuthInfoParserOptions{Issuer: testIssuer})

	so, err := elephantine.NewDefaultServiceOptions(
		logger, parser, prometheus.NewRegistry(),
		elephantine.ServiceAuthRequired)
	test.Mustf(t, err, "create the default service options")

	test.NotNilf(t, so.Hooks, "install the Twirp hooks")

	test.Equalf(t, 3, len(so.Interceptors),
		"install the metrics, logging and authentication interceptors")

	test.Equalf(t, 1, len(so.HandlerOptions()),
		"turn the interceptors into a handler option")

	if so.AuthMiddleware == nil {
		t.Fatal("the authentication middleware should be installed")
	}
}

// TestPropagateHeaders checks that the headers a caller sets on the context
// reach the server.
func TestPropagateHeaders(t *testing.T) {
	var (
		mu   sync.Mutex
		seen http.Header
	)

	path, handler := testserviceconnect.NewTestServiceHandler(testImpl{})

	mux := http.NewServeMux()

	mux.Handle(path, http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		mu.Lock()
		seen = r.Header.Clone()
		mu.Unlock()

		handler.ServeHTTP(w, r)
	}))

	srv := httptest.NewServer(mux)

	t.Cleanup(srv.Close)

	client := testserviceconnect.NewTestServiceClient(
		srv.Client(), srv.URL,
		connect.WithInterceptors(rpc.PropagateHeaders()))

	ctx := rpc.WithOutgoingHeaders(t.Context(), http.Header{
		"X-Forwarded-For": []string{"192.0.2.1"},
	})

	_, err := client.Echo(ctx, &testservice.EchoRequest{Message: echoMessage})
	test.Mustf(t, err, "call Echo")

	mu.Lock()
	defer mu.Unlock()

	test.Equalf(t, "192.0.2.1", seen.Get("X-Forwarded-For"),
		"propagate the header set on the context")
}

// TestRootRequireAnyScopeParity checks that the deprecated
// elephantine.RequireAnyScope still produces exactly the Twirp error it always
// has, now that it delegates to rpc.RequireAnyScope.
func TestRootRequireAnyScopeParity(t *testing.T) {
	cases := map[string]context.Context{
		"anonymous": context.Background(),
		"unscoped": elephantine.SetAuthInfo(
			context.Background(), &elephantine.AuthInfo{
				Claims: elephantine.JWTClaims{
					Scope: "something_else",
				},
			}),
	}

	cases["unscoped"] = withSubject(cases["unscoped"])

	for name := range cases {
		ctx := cases[name]

		t.Run(name, func(t *testing.T) {
			//nolint:staticcheck // the deprecated function is what
			// this test is about.
			_, twirpErr := elephantine.RequireAnyScope(
				ctx, "test_read", "test_write")
			test.MustNotf(t, twirpErr, "get an error")

			_, connectErr := rpc.RequireAnyScope(
				ctx, "test_read", "test_write")
			test.MustNotf(t, connectErr, "get an error")

			test.ErrorParity(t, twirpErr, connectErr)
		})
	}
}

// withSubject gives the context's auth info a subject, since an empty subject
// is what makes a caller anonymous.
func withSubject(ctx context.Context) context.Context {
	info, _ := elephantine.GetAuthInfo(ctx)

	info.Claims.Subject = "user://test/hugo"

	return ctx
}

// The fixture service is unary only, since the plain interface every elephant
// service implements has no place for a stream, so the streaming tests build a
// server-streaming handler with connect-go's own constructors. It is mounted
// next to the fixture service on every test stack.
const (
	streamPath      = "/elephantine.testservice.v1.Streamer/"
	streamProcedure = streamPath + "Echo"
)

// newStreamHandler is a server-streaming handler that answers with one message.
func newStreamHandler(opts ...connect.HandlerOption) http.Handler {
	return connect.NewServerStreamHandler(streamProcedure,
		func(
			_ context.Context,
			req *connect.Request[testservice.EchoRequest],
			stream *connect.ServerStream[testservice.EchoResponse],
		) error {
			err := stream.Send(&testservice.EchoResponse{
				Message: req.Msg.GetMessage(),
			})
			if err != nil {
				return fmt.Errorf("send the message: %w", err)
			}

			return nil
		}, opts...)
}

// newStreamClient is a client for the server-streaming fixture handler.
func newStreamClient(
	httpClient connect.HTTPClient, baseURL string,
) *connect.Client[testservice.EchoRequest, testservice.EchoResponse] {
	return connect.NewClient[testservice.EchoRequest, testservice.EchoResponse](
		httpClient, baseURL+streamProcedure,
		connect.WithClientOptions())
}

// callStream makes the streaming call and returns the error the caller ends up
// with, which for a stream is the one the receive loop reports.
func callStream(
	ctx context.Context,
	client *connect.Client[testservice.EchoRequest, testservice.EchoResponse],
) (string, error) {
	stream, err := client.CallServerStream(ctx,
		connect.NewRequest(&testservice.EchoRequest{Message: echoMessage}))
	if err != nil {
		return "", err //nolint:wrapcheck // the test asserts on the code.
	}

	defer func() {
		_ = stream.Close()
	}()

	var message string

	for stream.Receive() {
		message = stream.Msg().GetMessage()
	}

	err = stream.Err()
	if err != nil {
		return "", err //nolint:wrapcheck // the test asserts on the code.
	}

	return message, nil
}

// TestStreamingAuthentication checks that a streaming call is authenticated
// like a unary one. connect.UnaryInterceptorFunc passes streaming calls
// through untouched, so an interceptor written with it would let an
// unauthenticated stream run: every interceptor in this package therefore
// implements the streaming wrappers, and the authentication middleware refuses
// the call before it reaches them.
func TestStreamingAuthentication(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	t.Run("authenticated", func(t *testing.T) {
		message, err := callStream(t.Context(), s.StreamClient(s.Token))
		test.Mustf(t, err, "call the streaming method")

		test.Equalf(t, echoMessage, message, "echo the message")
	})

	t.Run("no_authorization", func(t *testing.T) {
		_, err := callStream(t.Context(), s.StreamClient(""))

		test.IsRPCError(t, err, connect.CodeUnauthenticated)
	})

	t.Run("invalid_authorization", func(t *testing.T) {
		_, err := callStream(t.Context(), s.StreamClient("not-a-token"))

		test.IsRPCError(t, err, connect.CodeUnauthenticated)
	})
}

// TestStreamingWithoutAuthMiddleware checks the safety net: a handler mounted
// with the service's handler options but not behind the authentication
// middleware still refuses an unauthenticated call, unary or streaming, rather
// than running the handler with no caller.
func TestStreamingWithoutAuthMiddleware(t *testing.T) {
	var (
		logger = slog.New(test.NewLogHandler(t, slog.LevelDebug))
		key    = test.NewSigningKey(t)
		parser = elephantine.NewStaticAuthInfoParser(
			t.Context(), key.PublicKey,
			elephantine.JWTAuthInfoParserOptions{Issuer: testIssuer})
	)

	so, err := elephantine.NewDefaultServiceOptions(
		logger, parser, prometheus.NewRegistry(),
		elephantine.ServiceAuthRequired)
	test.Mustf(t, err, "create the default service options")

	mux := http.NewServeMux()

	// Mounted directly, so nothing runs opt.AuthMiddleware.
	mux.Handle(streamPath, newStreamHandler(so.HandlerOptions()...))

	path, handler := testserviceconnect.NewTestServiceHandler(
		testImpl{}, so.HandlerOptions()...)

	mux.Handle(path, handler)

	srv := httptest.NewServer(mux)

	t.Cleanup(srv.Close)

	_, err = callStream(t.Context(), newStreamClient(srv.Client(), srv.URL))
	test.IsRPCError(t, err, connect.CodeUnauthenticated)

	unary := testserviceconnect.NewTestServiceClient(srv.Client(), srv.URL)

	_, err = unary.Echo(t.Context(), &testservice.EchoRequest{})
	test.IsRPCError(t, err, connect.CodeUnauthenticated)
}

// TestBareContextErrorCode checks that a handler error that is not a
// *connect.Error but wraps a context cancellation is counted and logged as
// canceled, the way Connect answers it, and not as an unknown server fault. It
// is connect-go's own handler wrapper that returns such an error when the
// request context is already done, so counting it as unknown would make every
// disconnected client look like a server error.
func TestBareContextErrorCode(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	client := s.Clients(s.Token)["connect"]

	_, err := client.Fail(t.Context(),
		&testservice.FailRequest{Code: codeBareCancellation})
	test.MustNotf(t, err, "get an error from Fail")

	test.IsRPCError(t, err, connect.CodeCanceled)

	logged := s.Records.Attributes("error response")

	test.Equalf(t, 1, len(logged), "log one error response")
	test.EqualDiff(t, map[string]string{
		logKeyLevel:   "WARN",
		"err_code":    "canceled",
		"err":         "do the work: context canceled",
		"status_code": "499",
	}, logged[0], "log the cancellation as canceled")

	err = testutil.GatherAndCompare(s.Registry, strings.NewReader(`
# HELP rpc_protocol_responses_total `+rpcmetrics.ProtocolResponsesHelp+`
# TYPE rpc_protocol_responses_total counter
rpc_protocol_responses_total{client_id="",code="canceled",method="Fail",protocol="connect",service="Test"} 1
`), "rpc_protocol_responses_total")
	test.Mustf(t, err, "count the response under the canceled code")

	err = testutil.GatherAndCompare(s.Registry, strings.NewReader(`
# HELP rpc_responses_total Number of RPC responses sent.
# TYPE rpc_responses_total counter
rpc_responses_total{customer="",method="Fail",service="Test",status="499"} 1
`), "rpc_responses_total")
	test.Mustf(t, err, "report the status Connect answers a cancellation with")
}

// TestGRPCResponseStatus checks that a gRPC response is reported with status
// 200 whatever the error was, since gRPC carries the code in the trailers and
// answers every call with 200.
func TestGRPCResponseStatus(t *testing.T) {
	s := newStack(t, testImpl{}, elephantine.ServiceAuthRequired)

	var transport http.Transport

	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetUnencryptedHTTP2(true)

	client := http.Client{
		Transport: &bearerTransport{
			token: s.Token,
			next:  &transport,
		},
	}

	grpc := testserviceconnect.NewTestServiceClient(
		&client, "http://"+s.Addr, connect.WithGRPC())

	_, err := grpc.Fail(t.Context(),
		&testservice.FailRequest{Code: codeNotFound})
	test.MustNotf(t, err, "get an error from Fail over gRPC")

	err = testutil.GatherAndCompare(s.Registry, strings.NewReader(`
# HELP rpc_responses_total Number of RPC responses sent.
# TYPE rpc_responses_total counter
rpc_responses_total{customer="",method="Fail",service="Test",status="200"} 1
`), "rpc_responses_total")
	test.Mustf(t, err, "report the status gRPC actually answers with")
}
