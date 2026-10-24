package rpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine/internal/auth"
	"github.com/ttab/elephantine/internal/rpcmetrics"
)

// DPoPHeader is the request header that carries a DPoP proof.
const DPoPHeader = "DPoP"

// DefaultDPoPMaxProofAge is how old a DPoP proof may be by default. It suits a
// client that makes its proof just before the call, which is the case RFC 9449
// describes.
const DefaultDPoPMaxProofAge = 60 * time.Second

// dpopFutureLeeway is how far into the future a proof's iat may be, for a
// device whose clock runs slightly ahead.
const dpopFutureLeeway = 5 * time.Second

// dpopType is the typ header of a DPoP proof.
const dpopType = "dpop+jwt"

// dpopLabelResult is the label rpc_dpop_proofs_total counts proofs by.
const dpopLabelResult = "result"

// Results of DPoP proof verification, the values of the result label of
// rpc_dpop_proofs_total.
const (
	DPoPResultValid       = "valid"
	DPoPResultMissing     = "missing"
	DPoPResultMalformed   = "malformed"
	DPoPResultKeyMismatch = "key_mismatch"
	DPoPResultMismatch    = "request_mismatch"
	DPoPResultStale       = "stale"
	DPoPResultReplayed    = "replayed"
	DPoPResultError       = "error"
)

// ErrInvalidDPoPProof is the cause of every refused DPoP proof.
var ErrInvalidDPoPProof = errors.New("invalid DPoP proof")

// DPoPError is a refused DPoP proof. Result is the reason, one of the
// DPoPResult values.
type DPoPError struct {
	Result string
	Reason string
}

func (e *DPoPError) Error() string {
	return fmt.Sprintf("%s: %s", ErrInvalidDPoPProof.Error(), e.Reason)
}

func (e *DPoPError) Unwrap() error {
	return ErrInvalidDPoPProof
}

func proofError(result string, format string, a ...any) error {
	return &DPoPError{Result: result, Reason: fmt.Sprintf(format, a...)}
}

// DPoPReplayStore records the proofs that have been used, so that a proof can
// only be used once.
type DPoPReplayStore interface {
	// UseProof records the proof identifier jti for the key with the
	// thumbprint jkt, and reports whether it was unused. The record may be
	// dropped after expires, when the proof is too old to be accepted
	// anyway. A store shared by the replicas of a service is what makes
	// the check hold across them.
	UseProof(
		ctx context.Context, jkt string, jti string, expires time.Time,
	) (bool, error)
}

// DPoPOptions configures a DPoPVerifier.
type DPoPOptions struct {
	// PublicURL is the base URL callers reach the service at, which the
	// htu of a proof is compared with: scheme, host and path. Behind an
	// ingress the service cannot tell that from the request, so it is
	// configuration. A Connect call's URL is PublicURL followed by the
	// procedure, and any other request's is PublicURL followed by its
	// path.
	PublicURL string

	// MaxProofAge is how old a proof's iat may be. It defaults to
	// DefaultDPoPMaxProofAge.
	//
	// RFC 9449 expects a proof made just before the request it is sent
	// with, and a minute covers that and clock skew. A client that cannot
	// do that has to be given longer: iOS hands a background upload to
	// its upload daemon when it is queued, so the proof is signed at
	// enqueue and may be hours old when the request arrives. A service
	// that accepts such requests sets this to the longest it lets a
	// request wait in a queue, typically the lifetime of the tokens it
	// accepts, and bounds what a replay can do by other means, such as
	// de-duplicating by a client-generated upload ID.
	MaxProofAge time.Duration

	// Replay records used proofs, so that a proof is accepted once. Nil
	// accepts a proof as often as it is sent within MaxProofAge. Each
	// proof is bound to the method, URL and token of its request, so a
	// replay can only repeat that request; a service whose requests are
	// not idempotent should give a store.
	Replay DPoPReplayStore

	// Now is the clock, time.Now when nil.
	Now func() time.Time
}

// DPoPVerifier verifies RFC 9449 DPoP proofs for tokens bound to a key.
type DPoPVerifier struct {
	base        *url.URL
	maxProofAge time.Duration
	replay      DPoPReplayStore
	now         func() time.Time
	results     *prometheus.CounterVec
}

// NewDPoPVerifier creates a DPoP proof verifier, registering its metric with
// reg.
func NewDPoPVerifier(
	reg prometheus.Registerer, opts DPoPOptions,
) (*DPoPVerifier, error) {
	base, err := url.Parse(opts.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("parse the public URL: %w", err)
	}

	// The htu check compares scheme, host and path with this URL; without
	// a scheme and host it would degenerate to comparing paths.
	if (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf(
			"the public URL %q must be an absolute http(s) URL without query or fragment",
			opts.PublicURL)
	}

	if opts.MaxProofAge < 0 {
		return nil, errors.New("the maximum proof age must not be negative")
	}

	v := DPoPVerifier{
		base:        base,
		maxProofAge: opts.MaxProofAge,
		replay:      opts.Replay,
		now:         opts.Now,
	}

	if v.maxProofAge == 0 {
		v.maxProofAge = DefaultDPoPMaxProofAge
	}

	if v.now == nil {
		v.now = time.Now
	}

	results, err := rpcmetrics.RegisterOrReuse(reg, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rpc_dpop_proofs_total",
			Help: "DPoP proofs checked for calls made with a token bound" +
				" to a key, by result. A rise in request_mismatch" +
				" across all callers is usually a public URL that" +
				" does not match what clients call, stale is a" +
				" client clock or a queue holding requests longer" +
				" than the proof age allows, and replayed or" +
				" key_mismatch from one client is a token being used" +
				" by someone who does not hold its key.",
		},
		[]string{dpopLabelResult},
	))
	if err != nil {
		return nil, fmt.Errorf("register the DPoP metric: %w", err)
	}

	for _, r := range []string{
		DPoPResultValid, DPoPResultMissing, DPoPResultMalformed,
		DPoPResultKeyMismatch, DPoPResultMismatch, DPoPResultStale,
		DPoPResultReplayed, DPoPResultError,
	} {
		results.WithLabelValues(r)
	}

	v.results = results

	return &v, nil
}

// DPoPRequest is the request a proof is checked against.
type DPoPRequest struct {
	// Proofs are the values of the DPoP header.
	Proofs []string
	// Method is the HTTP method of the request.
	Method string
	// Path is the path of the request, which is resolved against the
	// public URL.
	Path string
	// AccessToken is the token the request was authorized with.
	AccessToken string
	// JKT is the thumbprint of the key the token is bound to.
	JKT string
}

// Verify checks the DPoP proof for a request made with a token bound to the
// key with the thumbprint req.JKT: that there is exactly one, that it is
// signed with that key, for this method, URL and access token, recently, and,
// with a replay store, only once. A refused proof is a *DPoPError; any other
// error is a failure to record the proof.
func (v *DPoPVerifier) Verify(ctx context.Context, req DPoPRequest) error {
	err := v.verify(ctx, req)

	result := DPoPResultValid

	var pe *DPoPError

	switch {
	case errors.As(err, &pe):
		result = pe.Result
	case err != nil:
		result = DPoPResultError
	}

	v.results.WithLabelValues(result).Inc()

	return err
}

type dpopClaims struct {
	jwt.RegisteredClaims

	HTM string `json:"htm"`
	HTU string `json:"htu"`
	ATH string `json:"ath"`
}

func (v *DPoPVerifier) verify(ctx context.Context, req DPoPRequest) error {
	// RFC 9449 section 4.3: exactly one DPoP header.
	switch len(req.Proofs) {
	case 0:
		return proofError(DPoPResultMissing,
			"the token is bound to a key and no proof was given")
	case 1:
	default:
		return proofError(DPoPResultMalformed, "more than one DPoP header")
	}

	if req.JKT == "" {
		return proofError(DPoPResultKeyMismatch,
			"the token is not bound to a key")
	}

	var (
		claims dpopClaims
		key    ecJWK
	)

	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodES256.Alg()}),
		// The time claims are checked against the proof window below,
		// which is not the token leeway the parser would apply.
		jwt.WithoutClaimsValidation(),
	)

	_, err := parser.ParseWithClaims(req.Proofs[0], &claims,
		func(t *jwt.Token) (any, error) {
			if t.Header["typ"] != dpopType {
				return nil, errors.New("wrong typ header")
			}

			raw, ok := t.Header["jwk"].(map[string]any)
			if !ok {
				return nil, errors.New("no jwk header")
			}

			k, err := parseECJWK(raw)
			if err != nil {
				return nil, err
			}

			key = k

			return k.PublicKey()
		})
	if err != nil {
		return proofError(DPoPResultMalformed, "%v", err)
	}

	if subtle.ConstantTimeCompare(
		[]byte(key.Thumbprint()), []byte(req.JKT)) != 1 {
		return proofError(DPoPResultKeyMismatch,
			"the proof is not signed with the key the token is bound to")
	}

	if claims.ID == "" {
		return proofError(DPoPResultMalformed, "no jti")
	}

	if claims.IssuedAt == nil {
		return proofError(DPoPResultMalformed, "no iat")
	}

	if claims.HTM != req.Method {
		return proofError(DPoPResultMismatch, "wrong htm")
	}

	if !v.sameHTU(claims.HTU, req.Path) {
		return proofError(DPoPResultMismatch, "wrong htu")
	}

	if subtle.ConstantTimeCompare(
		[]byte(claims.ATH), []byte(AccessTokenHash(req.AccessToken))) != 1 {
		return proofError(DPoPResultMismatch, "wrong ath")
	}

	var (
		now = v.now()
		iat = claims.IssuedAt.Time
	)

	if iat.After(now.Add(dpopFutureLeeway)) ||
		iat.Before(now.Add(-v.maxProofAge)) {
		return proofError(DPoPResultStale,
			"iat outside the accepted window")
	}

	if v.replay == nil {
		return nil
	}

	fresh, err := v.replay.UseProof(ctx, req.JKT, claims.ID,
		iat.Add(v.maxProofAge+dpopFutureLeeway))
	if err != nil {
		return fmt.Errorf("record the DPoP proof: %w", err)
	}

	if !fresh {
		return proofError(DPoPResultReplayed,
			"the proof has already been used")
	}

	return nil
}

// sameHTU compares the htu claim with the request URL, ignoring query and
// fragment as RFC 9449 says.
func (v *DPoPVerifier) sameHTU(htu string, path string) bool {
	claimed, err := url.Parse(htu)
	if err != nil {
		return false
	}

	// Not url.JoinPath, which cleans the path, and leaves it relative
	// when the base has none.
	target := strings.TrimSuffix(v.base.Path, "/") +
		"/" + strings.TrimPrefix(path, "/")

	return strings.EqualFold(claimed.Scheme, v.base.Scheme) &&
		strings.EqualFold(claimed.Host, v.base.Host) &&
		claimed.Path == target
}

// AccessTokenHash is the ath of RFC 9449: the base64url encoded SHA-256 of the
// access token.
func AccessTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ecJWK is a P-256 public key in JWK form, the kind of key a DPoP proof is
// accepted with: proofs are ES256, which is what a Secure Enclave key signs.
type ecJWK struct {
	Kty string
	Crv string
	X   string
	Y   string
}

func parseECJWK(raw map[string]any) (ecJWK, error) {
	if _, hasPrivate := raw["d"]; hasPrivate {
		return ecJWK{}, errors.New("the jwk header holds a private key")
	}

	member := func(name string) string {
		s, _ := raw[name].(string)

		return s
	}

	return ecJWK{
		Kty: member("kty"),
		Crv: member("crv"),
		X:   member("x"),
		Y:   member("y"),
	}, nil
}

const p256CoordinateSize = 32

// PublicKey validates the JWK and returns the key it describes.
func (k ecJWK) PublicKey() (*ecdsa.PublicKey, error) {
	if k.Kty != "EC" {
		return nil, fmt.Errorf("unsupported key type %q, must be EC", k.Kty)
	}

	if k.Crv != "P-256" {
		return nil, fmt.Errorf("unsupported curve %q, must be P-256", k.Crv)
	}

	x, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil || len(x) != p256CoordinateSize {
		return nil, errors.New("invalid x coordinate")
	}

	y, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil || len(y) != p256CoordinateSize {
		return nil, errors.New("invalid y coordinate")
	}

	// The parser checks that the point is on the curve.
	uncompressed := append(append([]byte{4}, x...), y...)

	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), uncompressed)
	if err != nil {
		return nil, fmt.Errorf("invalid public key: %w", err)
	}

	return pub, nil
}

// Thumbprint is the RFC 7638 SHA-256 thumbprint of the key, base64url
// encoded: the jkt of RFC 9449. It is only meaningful for a key PublicKey
// accepted, whose members are base64url and so need no JSON escaping.
func (k ecJWK) Thumbprint() string {
	// The required members in lexicographic order, without whitespace.
	canonical := `{"crv":"` + k.Crv + `","kty":"` + k.Kty +
		`","x":"` + k.X + `","y":"` + k.Y + `"}`

	sum := sha256.Sum256([]byte(canonical))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// DPoPThumbprint returns the RFC 7638 thumbprint of a P-256 public key, the
// value a token bound to it carries as cnf.jkt.
func DPoPThumbprint(pub *ecdsa.PublicKey) (string, error) {
	raw, err := pub.Bytes()
	if err != nil {
		return "", fmt.Errorf("encode the public key: %w", err)
	}

	if pub.Curve != elliptic.P256() || len(raw) != 1+2*p256CoordinateSize {
		return "", errors.New("the key is not a P-256 key")
	}

	k := ecJWK{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(raw[1 : 1+p256CoordinateSize]),
		Y:   base64.RawURLEncoding.EncodeToString(raw[1+p256CoordinateSize:]),
	}

	return k.Thumbprint(), nil
}

// VerifyHTTP checks the proof for an HTTP request whose context carries a
// caller with a token bound to a key, and makes the caller visible to
// GetAuthInfo. The proof's htu is checked against the public URL followed by
// the request path. It is what the authentication middleware uses for a
// request that does not pass through DPoPInterceptor, such as a Twirp call. It
// returns nil for a context with no bound token, and otherwise the error the
// caller is to be answered with.
func (v *DPoPVerifier) VerifyHTTP(r *http.Request) error {
	err := v.verifyContext(r.Context(), r.Method, r.URL.Path, r.Header)
	if err != nil {
		return dpopRPCError(err)
	}

	return nil
}

// verifyContext checks the proof for the bound token on the context, if there
// is one that has not been verified yet, and marks it verified.
func (v *DPoPVerifier) verifyContext(
	ctx context.Context, method string, path string, header http.Header,
) error {
	info, ok := auth.PendingInfo(ctx)
	if !ok || auth.ProofVerified(ctx) {
		return nil
	}

	jkt, bound := info.Claims.BoundKey()
	if !bound {
		return nil
	}

	err := v.Verify(ctx, DPoPRequest{
		Proofs:      header.Values(DPoPHeader),
		Method:      method,
		Path:        path,
		AccessToken: info.Token,
		JKT:         jkt,
	})
	if err != nil {
		return err
	}

	auth.MarkProofVerified(ctx)

	return nil
}

// dpopRPCError turns a verification failure into the error a caller is
// answered with: unauthenticated for a refused proof, internal for a replay
// store that failed.
func dpopRPCError(err error) error {
	if errors.Is(err, ErrInvalidDPoPProof) {
		return Errorf(connect.CodeUnauthenticated,
			"invalid authorization: %v", err)
	}

	return Internalf("verify the DPoP proof: %v", err)
}

// DPoPInterceptor verifies the DPoP proof of a call made with a token bound to
// a key, before the call reaches a handler. Until it has, the caller is not
// visible to GetAuthInfo or RequireAnyScope, so a bound token is never
// accepted without its proof. Calls made with a token that is not bound to a
// key, and anonymous calls, pass through untouched.
//
// The proof's htu is checked against the verifier's public URL followed by
// the procedure. It covers streaming handlers as well as unary ones, which is
// why it is not a connect.UnaryInterceptorFunc; a stream is always a POST.
func DPoPInterceptor(v *DPoPVerifier) connect.Interceptor {
	return interceptor{
		unary: func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(
				ctx context.Context, req connect.AnyRequest,
			) (connect.AnyResponse, error) {
				err := v.verifyContext(ctx, req.HTTPMethod(),
					req.Spec().Procedure, req.Header())
				if err != nil {
					return nil, dpopRPCError(err)
				}

				return next(ctx, req)
			}
		},
		streamingHandler: func(
			next connect.StreamingHandlerFunc,
		) connect.StreamingHandlerFunc {
			return func(
				ctx context.Context, conn connect.StreamingHandlerConn,
			) error {
				err := v.verifyContext(ctx, http.MethodPost,
					conn.Spec().Procedure, conn.RequestHeader())
				if err != nil {
					return dpopRPCError(err)
				}

				return next(ctx, conn)
			}
		},
	}
}
