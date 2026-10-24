package rpc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/internal/testservice"
	"github.com/ttab/elephantine/internal/testservice/testserviceconnect"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/elephantine/test"
)

const (
	// delegateIssuer is the iss of the delegate tokens the tests sign.
	delegateIssuer = "elephant-delegate"
	// serviceAudience is the aud the service under test accepts.
	serviceAudience = "elephant-stream"
	// publicURL is the URL the service is reached at from the outside,
	// which is what the proofs name rather than the test listener.
	publicURL = "https://stream.example.com"
)

// delegate is an elephant-delegate stand-in: the signing key of the tokens,
// and a device key they can be bound to.
type delegate struct {
	key    ed25519.PrivateKey
	device *ecdsa.PrivateKey
	jkt    string
}

func newDelegate(t *testing.T) *delegate {
	t.Helper()

	_, key, err := ed25519.GenerateKey(rand.Reader)
	test.Mustf(t, err, "generate the delegate key")

	device, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	test.Mustf(t, err, "generate the device key")

	jkt, err := rpc.DPoPThumbprint(&device.PublicKey)
	test.Mustf(t, err, "compute the device key thumbprint")

	return &delegate{key: key, device: device, jkt: jkt}
}

// Issuer is the trusted issuer configuration for the delegate.
func (d *delegate) Issuer() elephantine.TrustedIssuer {
	pub, _ := d.key.Public().(ed25519.PublicKey)

	return elephantine.TrustedIssuer{
		Issuer: delegateIssuer,
		Keyfunc: func(_ *jwt.Token) (any, error) {
			return pub, nil
		},
	}
}

// Claims are the claims of a document token bound to the device key, as
// elephant-delegate mints them.
func (d *delegate) Claims(t *testing.T) elephantine.JWTClaims {
	t.Helper()

	return elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    delegateIssuer,
			Subject:   "core://user/" + strings.ToLower(t.Name()),
			Audience:  jwt.ClaimStrings{serviceAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        uuid.NewString(),
		},
		Scope:        "test_write",
		Confirmation: &elephantine.Confirmation{JKT: d.jkt},
		DocumentUUID: uuid.NewString(),
	}
}

// Sign signs a token with the delegate key.
func (d *delegate) Sign(t *testing.T, claims elephantine.JWTClaims) string {
	t.Helper()

	token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).
		SignedString(d.key)
	test.Mustf(t, err, "sign the delegate token")

	return token
}

// proof describes the DPoP proof a dpopTransport makes. The zero value is a
// correct proof for the request.
type proof struct {
	// Key signs the proof instead of the device key.
	Key *ecdsa.PrivateKey
	// Method and URL replace the htm and htu.
	Method string
	URL    string
	// Token replaces the token the ath is computed from.
	Token string
	// Age backdates the iat.
	Age time.Duration
	// JTI replaces the generated jti.
	JTI string
	// Skip sends no proof at all.
	Skip bool
}

// dpopTransport authorizes every request with a bound token and a DPoP proof
// for it, the way the shell does.
type dpopTransport struct {
	t      *testing.T
	token  string
	device *ecdsa.PrivateKey
	proof  proof
	next   http.RoundTripper
}

func (tr *dpopTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "DPoP "+tr.token)

	if !tr.proof.Skip {
		r.Header.Set(rpc.DPoPHeader, tr.sign(r))
	}

	next := tr.next
	if next == nil {
		next = http.DefaultTransport
	}

	res, err := next.RoundTrip(r)
	if err != nil {
		return nil, fmt.Errorf("round trip the request: %w", err)
	}

	return res, nil
}

func (tr *dpopTransport) sign(r *http.Request) string {
	p := tr.proof

	key := tr.device
	if p.Key != nil {
		key = p.Key
	}

	method := r.Method
	if p.Method != "" {
		method = p.Method
	}

	htu := publicURL + r.URL.Path
	if p.URL != "" {
		htu = p.URL
	}

	token := tr.token
	if p.Token != "" {
		token = p.Token
	}

	jti := p.JTI
	if jti == "" {
		jti = uuid.NewString()
	}

	raw, err := key.PublicKey.Bytes()
	test.Mustf(tr.t, err, "encode the proof key")

	proofToken := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"jti": jti,
		"htm": method,
		"htu": htu,
		"iat": time.Now().Add(-p.Age).Unix(),
		"ath": rpc.AccessTokenHash(token),
	})

	proofToken.Header["typ"] = "dpop+jwt"
	proofToken.Header["jwk"] = map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(raw[1:33]),
		"y":   base64.RawURLEncoding.EncodeToString(raw[33:]),
	}

	signed, err := proofToken.SignedString(key)
	test.Mustf(tr.t, err, "sign the proof")

	return signed
}

// boundClients returns one client per protocol that sends the token with a
// proof described by p.
func boundClients(
	t *testing.T, s *stack, d *delegate, token string, p proof,
) map[string]testservice.TestService {
	t.Helper()

	return s.clientsWith(&http.Client{Transport: &dpopTransport{
		t: t, token: token, device: d.device, proof: p,
		next: s.client.Transport,
	}})
}

// newDelegateStack starts a dual-stack service that accepts the delegate's
// tokens for serviceAudience, verifying their proofs with the options.
func newDelegateStack(
	t *testing.T, d *delegate, dpop *rpc.DPoPOptions,
) *stack {
	t.Helper()

	opts := []stackOption{
		withParserOptions(elephantine.JWTAuthInfoParserOptions{
			Audience:       serviceAudience,
			TrustedIssuers: []elephantine.TrustedIssuer{d.Issuer()},
		}),
	}

	if dpop != nil {
		opts = append(opts, withDPoP(*dpop))
	}

	return newStack(t, scopedImpl{}, elephantine.ServiceAuthRequired,
		opts...)
}

// TestDPoPBoundToken checks that a service accepts a delegate token bound to a
// device key when, and only when, the call carries a valid proof made with
// that key, on both stacks.
func TestDPoPBoundToken(t *testing.T) {
	var (
		d      = newDelegate(t)
		s      = newDelegateStack(t, d, &rpc.DPoPOptions{PublicURL: publicURL})
		claims = d.Claims(t)
		token  = d.Sign(t, claims)
	)

	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	test.Mustf(t, err, "generate another device key")

	t.Run("valid_proof", func(t *testing.T) {
		for name, client := range boundClients(t, s, d, token, proof{}) {
			t.Run(name, func(t *testing.T) {
				res, err := client.Echo(t.Context(),
					&testservice.EchoRequest{Message: echoMessage})
				test.Mustf(t, err, "call with a valid proof")

				test.Equalf(t, claims.Subject, res.GetSubject(),
					"see the delegated user")
			})
		}
	})

	// Each refusal is checked for its reason too, so that a case cannot
	// pass by failing an earlier check than the one it is about.
	refused := map[string]struct {
		proof  proof
		reason string
	}{
		"no_proof":        {proof{Skip: true}, "no proof was given"},
		"other_key":       {proof{Key: otherKey}, "not signed with the key"},
		"wrong_method":    {proof{Method: http.MethodPut}, "wrong htm"},
		"wrong_url":       {proof{URL: publicURL + "/other.Service/Echo"}, "wrong htu"},
		"wrong_host":      {proof{URL: "https://elsewhere.example.com/x"}, "wrong htu"},
		"other_token":     {proof{Token: "another-token"}, "wrong ath"},
		"stale":           {proof{Age: 10 * time.Minute}, "iat outside"},
		"from_the_future": {proof{Age: -time.Minute}, "iat outside"},
	}

	for name, c := range refused {
		t.Run(name, func(t *testing.T) {
			for proto, client := range boundClients(t, s, d, token, c.proof) {
				t.Run(proto, func(t *testing.T) {
					_, err := client.Echo(t.Context(),
						&testservice.EchoRequest{Message: echoMessage})

					test.IsRPCError(t, err, connect.CodeUnauthenticated)

					if !strings.Contains(err.Error(), c.reason) {
						t.Fatalf("expected %q in the error, got %v",
							c.reason, err)
					}
				})
			}
		})
	}

	t.Run("bearer_without_proof", func(t *testing.T) {
		// Sending the bound token as a bearer token is no way around
		// the proof.
		for name, client := range s.Clients(token) {
			t.Run(name, func(t *testing.T) {
				_, err := client.Echo(t.Context(),
					&testservice.EchoRequest{Message: echoMessage})

				test.IsRPCError(t, err, connect.CodeUnauthenticated)
			})
		}
	})

	t.Run("streaming", func(t *testing.T) {
		stream := func(p proof) testserviceconnect.StreamServiceClient {
			return testserviceconnect.NewStreamServiceClient(
				&http.Client{Transport: &dpopTransport{
					t: t, token: token, device: d.device, proof: p,
					next: s.client.Transport,
				}},
				"http://"+s.Addr)
		}

		emitted, err := emit(t.Context(), stream(proof{}),
			&testservice.EmitRequest{Count: 1})
		test.Mustf(t, err, "open a stream with a valid proof")

		test.Equalf(t, 1, len(emitted), "receive one message")

		_, err = emit(t.Context(), stream(proof{Skip: true}),
			&testservice.EmitRequest{Count: 1})
		test.IsRPCError(t, err, connect.CodeUnauthenticated)

		_, err = emit(t.Context(), stream(proof{Key: otherKey}),
			&testservice.EmitRequest{Count: 1})
		test.IsRPCError(t, err, connect.CodeUnauthenticated)
	})

	t.Run("session_token", func(t *testing.T) {
		// The identity provider's tokens are unaffected by the
		// delegate issuer.
		claims := test.Claims(t, "hugo", "test_write")
		claims.Audience = jwt.ClaimStrings{serviceAudience}

		for name, client := range s.Clients(s.TokenFor(t, claims)) {
			t.Run(name, func(t *testing.T) {
				_, err := client.Echo(t.Context(),
					&testservice.EchoRequest{Message: echoMessage})
				test.Mustf(t, err, "call with a session token")
			})
		}
	})

	problems, err := testutil.GatherAndLint(s.Registry)
	test.Mustf(t, err, "lint the metrics")

	if len(problems) > 0 {
		t.Fatalf("metric lint problems: %v", problems)
	}

	count := dpopResults(t, s.Registry, rpc.DPoPResultValid)
	if count < 3 {
		t.Fatalf("expected at least three valid proofs, got %v", count)
	}

	count = dpopResults(t, s.Registry, rpc.DPoPResultMissing)
	if count < 4 {
		t.Fatalf("expected at least four missing proofs, got %v", count)
	}
}

// dpopResults returns the value of the rpc_dpop_proofs_total sample for the
// result.
func dpopResults(
	t *testing.T, reg *prometheus.Registry, result string,
) float64 {
	t.Helper()

	families, err := reg.Gather()
	test.Mustf(t, err, "gather the metrics")

	for _, f := range families {
		if f.GetName() != "rpc_dpop_proofs_total" {
			continue
		}

		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					return m.GetCounter().GetValue()
				}
			}
		}
	}

	t.Fatalf("no rpc_dpop_proofs_total sample for %q", result)

	return 0
}

// TestDPoPAudienceAndIssuer checks that a delegate token is refused when it is
// for another service, or claims an issuer whose keys did not sign it.
func TestDPoPAudienceAndIssuer(t *testing.T) {
	var (
		d = newDelegate(t)
		s = newDelegateStack(t, d, &rpc.DPoPOptions{PublicURL: publicURL})
	)

	cases := map[string]func(c *elephantine.JWTClaims){
		"other_audience": func(c *elephantine.JWTClaims) {
			c.Audience = jwt.ClaimStrings{"elephant-push"}
		},
		"no_audience": func(c *elephantine.JWTClaims) {
			c.Audience = nil
		},
		"other_issuer": func(c *elephantine.JWTClaims) {
			c.Issuer = "someone-else"
		},
		"primary_issuer": func(c *elephantine.JWTClaims) {
			// Signed by the delegate but claiming to be the
			// identity provider: checked against the identity
			// provider's keys.
			c.Issuer = testIssuer
		},
		"no_expiry": func(c *elephantine.JWTClaims) {
			c.ExpiresAt = nil
		},
		"expired": func(c *elephantine.JWTClaims) {
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := d.Claims(t)
			mutate(&claims)

			token := d.Sign(t, claims)

			for proto, client := range boundClients(t, s, d, token, proof{}) {
				t.Run(proto, func(t *testing.T) {
					_, err := client.Echo(t.Context(),
						&testservice.EchoRequest{Message: echoMessage})

					test.IsRPCError(t, err, connect.CodeUnauthenticated)
				})
			}
		})
	}

	t.Run("forged_signature", func(t *testing.T) {
		forger := newDelegate(t)
		forger.device = d.device
		forger.jkt = d.jkt

		token := forger.Sign(t, d.Claims(t))

		for proto, client := range boundClients(t, s, d, token, proof{}) {
			t.Run(proto, func(t *testing.T) {
				_, err := client.Echo(t.Context(),
					&testservice.EchoRequest{Message: echoMessage})

				test.IsRPCError(t, err, connect.CodeUnauthenticated)
			})
		}
	})
}

// TestDPoPNotConfigured checks that a service that trusts the delegate issuer
// but does not verify proofs refuses a bound token even with a valid proof,
// rather than accepting it as a bearer token, and still accepts an unbound
// delegate token.
func TestDPoPNotConfigured(t *testing.T) {
	var (
		d = newDelegate(t)
		s = newDelegateStack(t, d, nil)
	)

	bound := d.Sign(t, d.Claims(t))

	for proto, client := range boundClients(t, s, d, bound, proof{}) {
		t.Run("bound_"+proto, func(t *testing.T) {
			_, err := client.Echo(t.Context(),
				&testservice.EchoRequest{Message: echoMessage})

			test.IsRPCError(t, err, connect.CodeUnauthenticated)
		})
	}

	claims := d.Claims(t)
	claims.Confirmation = nil

	unbound := d.Sign(t, claims)

	for proto, client := range s.Clients(unbound) {
		t.Run("unbound_"+proto, func(t *testing.T) {
			_, err := client.Echo(t.Context(),
				&testservice.EchoRequest{Message: echoMessage})
			test.Mustf(t, err, "call with an unbound delegate token")
		})
	}
}

// TestDPoPProofAge checks that a service can accept proofs older than the
// default window, which is what a background upload queued for hours needs.
func TestDPoPProofAge(t *testing.T) {
	var (
		d = newDelegate(t)
		s = newDelegateStack(t, d, &rpc.DPoPOptions{
			PublicURL:   publicURL,
			MaxProofAge: 12 * time.Hour,
		})
		token = d.Sign(t, d.Claims(t))
	)

	for proto, client := range boundClients(t, s, d, token,
		proof{Age: 3 * time.Hour}) {
		t.Run("three_hours_"+proto, func(t *testing.T) {
			_, err := client.Echo(t.Context(),
				&testservice.EchoRequest{Message: echoMessage})
			test.Mustf(t, err, "call with a proof signed at enqueue")
		})
	}

	for proto, client := range boundClients(t, s, d, token,
		proof{Age: 13 * time.Hour}) {
		t.Run("too_old_"+proto, func(t *testing.T) {
			_, err := client.Echo(t.Context(),
				&testservice.EchoRequest{Message: echoMessage})

			test.IsRPCError(t, err, connect.CodeUnauthenticated)
		})
	}
}

// memoryReplayStore is a DPoPReplayStore for one process.
type memoryReplayStore struct {
	mu   sync.Mutex
	used map[string]bool
	fail bool
}

func (m *memoryReplayStore) UseProof(
	_ context.Context, jkt string, jti string, _ time.Time,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.fail {
		return false, fmt.Errorf("the store is down")
	}

	if m.used == nil {
		m.used = make(map[string]bool)
	}

	key := jkt + " " + jti
	if m.used[key] {
		return false, nil
	}

	m.used[key] = true

	return true, nil
}

// TestDPoPReplay checks that a replay store makes a proof single use, and that
// a store that fails refuses the call as an internal error rather than letting
// it through.
func TestDPoPReplay(t *testing.T) {
	var (
		d     = newDelegate(t)
		store = &memoryReplayStore{}
		s     = newDelegateStack(t, d, &rpc.DPoPOptions{
			PublicURL: publicURL,
			Replay:    store,
		})
		token = d.Sign(t, d.Claims(t))
	)

	for proto := range s.Clients("") {
		t.Run("replayed_"+proto, func(t *testing.T) {
			client := boundClients(t, s, d, token,
				proof{JTI: "the-one-proof-" + proto})[proto]

			_, err := client.Echo(t.Context(),
				&testservice.EchoRequest{Message: echoMessage})
			test.Mustf(t, err, "use the proof once")

			_, err = client.Echo(t.Context(),
				&testservice.EchoRequest{Message: echoMessage})
			test.IsRPCError(t, err, connect.CodeUnauthenticated)
		})
	}

	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()

	for proto, client := range boundClients(t, s, d, token, proof{}) {
		t.Run("store_down_"+proto, func(t *testing.T) {
			_, err := client.Echo(t.Context(),
				&testservice.EchoRequest{Message: echoMessage})

			test.IsRPCError(t, err, connect.CodeInternal)
		})
	}
}

// TestBoundTokenHiddenUntilVerified checks the backstop: a bound token put on a
// context without its proof having been verified is invisible to
// GetAuthInfo and refused by RequireAnyScope, whoever put it there.
func TestBoundTokenHiddenUntilVerified(t *testing.T) {
	info := elephantine.AuthInfo{
		Token: "token",
		Claims: elephantine.JWTClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "core://user/someone",
			},
			Scope:        "test_write",
			Confirmation: &elephantine.Confirmation{JKT: "thumbprint"},
		},
	}

	ctx := elephantine.SetAuthInfo(t.Context(), &info)

	_, ok := rpc.GetAuthInfo(ctx)
	if ok {
		t.Fatal("expected the unverified bound token to be hidden")
	}

	_, err := rpc.RequireAnyScope(ctx, "test_write")
	test.IsRPCError(t, err, connect.CodeUnauthenticated)
}

// TestNewDPoPVerifierOptions checks that a public URL the htu check could not
// be made against is refused at startup.
func TestNewDPoPVerifierOptions(t *testing.T) {
	for _, u := range []string{
		"", "stream.example.com", "/path", "ftp://stream.example.com",
		"https://stream.example.com?x=1",
	} {
		_, err := rpc.NewDPoPVerifier(prometheus.NewRegistry(),
			rpc.DPoPOptions{PublicURL: u})
		test.MustNotf(t, err, "refuse the public URL %q", u)
	}

	reg := prometheus.NewRegistry()

	_, err := rpc.NewDPoPVerifier(reg, rpc.DPoPOptions{PublicURL: publicURL})
	test.Mustf(t, err, "create a verifier")

	_, err = rpc.NewDPoPVerifier(reg, rpc.DPoPOptions{PublicURL: publicURL})
	test.Mustf(t, err, "create a second verifier on the same registry")
}
