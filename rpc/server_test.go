package rpc_test

import (
	"context"
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
	"github.com/ttab/elephantine/internal/testservice"
	"github.com/ttab/elephantine/internal/testservice/testserviceconnect"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/elephantine/test"
)

// echoMessage is the message the fixture calls echo.
const echoMessage = "hello"

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

func (testImpl) Fail(
	_ context.Context, req *testservice.FailRequest,
) (*testservice.FailResponse, error) {
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
			elephantine.JWTAuthInfoParserOptions{Issuer: "test"})
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

	err = srv.ListenAndServe(t.Context())
	test.Mustf(t, err, "start the test API server")

	return &stack{
		Addr:     srv.Addr(),
		Registry: reg,
		Records:  records,
		Token:    test.AccessKey(t, key, test.Claims(t, "hugo", "test_read")),
		client:   client,
	}
}

// Clients returns one client per protocol, all of them the plain protobuf
// interface the service is implemented against.
func (s *stack) Clients(token string) map[string]testservice.Test {
	client := http.Client{
		Transport: &bearerTransport{
			token: token,
			next:  s.client.Transport,
		},
	}

	base := "http://" + s.Addr

	return map[string]testservice.Test{
		"twirp": testservice.NewTestProtobufClient(base, &client),
		"connect": testserviceconnect.NewTestServiceClient(
			&client, base),
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
			"level": r.Level.String(),
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

	t.Run("invalid_authorization", func(t *testing.T) {
		for name, client := range s.Clients("not-a-token") {
			t.Run(name, func(t *testing.T) {
				_, err := client.Echo(t.Context(),
					&testservice.EchoRequest{})

				test.IsRPCError(t, err, connect.CodePermissionDenied)
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

			test.IsRPCError(t, err, connect.CodePermissionDenied)
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
		"level":       "INFO",
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

	for name, client := range s.Clients(s.Token) {
		_, err := client.Echo(t.Context(),
			&testservice.EchoRequest{Message: echoMessage})
		test.Mustf(t, err, "call Echo over %s", name)

		_, err = client.Fail(t.Context(),
			&testservice.FailRequest{Code: codeNotFound})
		test.MustNotf(t, err, "get an error from Fail over %s", name)
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
rpc_responses_total{customer="acme",method="Fail",service="Test",status="404"} 2
`), "rpc_responses_total")
	test.Mustf(t, err, "count the responses with the same labels on both stacks")

	err = testutil.GatherAndCompare(s.Registry, strings.NewReader(`
# HELP rpc_protocol_responses_total `+protocolResponsesHelp+`
# TYPE rpc_protocol_responses_total counter
rpc_protocol_responses_total{code="not_found",method="Fail",protocol="connect",service="Test"} 1
rpc_protocol_responses_total{code="not_found",method="Fail",protocol="twirp",service="Test"} 1
rpc_protocol_responses_total{code="ok",method="Echo",protocol="connect",service="Test"} 1
rpc_protocol_responses_total{code="ok",method="Echo",protocol="twirp",service="Test"} 1
`), "rpc_protocol_responses_total")
	test.Mustf(t, err, "count the responses per protocol and code")
}

// protocolResponsesHelp is the help text of rpc_protocol_responses_total, which
// the exposition format comparison needs verbatim.
const protocolResponsesHelp = "Number of RPC responses sent, by protocol and" +
	" RPC code. Traffic on protocol=\"twirp\" is what still keeps a" +
	" service's Twirp mount alive, so a method whose Twirp share has" +
	" reached zero can have it removed; the code label separates errors" +
	" that rpc_responses_total reports under a single HTTP status, such" +
	" as a lock conflict from a validation failure."

// TestDefaultServiceOptions checks that the standard options come with both
// stacks wired up.
func TestDefaultServiceOptions(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))
	key := test.NewSigningKey(t)

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(), key.PublicKey,
		elephantine.JWTAuthInfoParserOptions{Issuer: "test"})

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
