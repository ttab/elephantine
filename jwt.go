package elephantine

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jellydator/ttlcache/v3"
	"github.com/ttab/elephantine/internal/auth"
	"github.com/ttab/elephantine/rpc"
)

// JWTClaims defines the claims that the elephant services understand.
//
// It is an alias of the type in the internal auth package, which is where it
// has to live for the rpc package to be able to use it without importing this
// one. It is the same type as rpc.JWTClaims.
type JWTClaims = auth.JWTClaims

// AuthInfo is used to add authentication information to a request context. It
// is the same type as rpc.AuthInfo.
type AuthInfo = auth.Info

// ErrNoAuthorization is used to communicate that authorization was completely
// missing, rather than being invalid, expired, or malformed.
var ErrNoAuthorization = auth.ErrNoAuthorization

// ErrBoundToken is the error JWTAuthInfoParser refuses a token bound to a key
// with, everywhere but in the authentication middleware, which verifies its
// DPoP proof. See JWTAuthInfoParser.AuthInfoFromHeader.
var ErrBoundToken = auth.ErrBoundToken

// AuthInfoParser validates bearer tokens and turns them into AuthInfo. See
// JWTAuthInfoParser for the standard JWT-based implementation.
type AuthInfoParser = auth.Parser

// Confirmation is the "cnf" claim of a token bound to a key. It is an alias
// of the type in the internal auth package.
type Confirmation = auth.Confirmation

// validSigningMethods are the JWT algorithms a token may be signed with. RS256
// and ES384 are the identity provider's, EdDSA is elephant-delegate's. The
// algorithm also has to fit the key the issuer's key function returns, so
// accepting one more here does not let an issuer's tokens be signed with
// another issuer's kind of key.
var validSigningMethods = []string{
	jwt.SigningMethodRS256.Name,
	jwt.SigningMethodES384.Name,
	jwt.SigningMethodEdDSA.Alg(),
}

// tokenLeeway is the clock skew allowed when validating a token's time
// claims.
const tokenLeeway = 5 * time.Second

// JWTAuthInfoParser is the standard AuthInfoParser implementation. It
// validates JWTs using the configured key function, caches successful results
// until the token expires, and optionally strips a scope prefix.
//
// It trusts one primary issuer, the identity provider, and any number of
// additional issuers configured with JWTAuthInfoParserOptions.TrustedIssuers,
// such as elephant-delegate. A token is validated against the issuer its
// unverified iss claim names, with that issuer's keys, audience and rules, so
// a forged iss gets the forger nothing.
type JWTAuthInfoParser struct {
	primary     *issuerValidator
	trusted     map[string]*issuerValidator
	cache       *ttlcache.Cache[string, AuthInfo]
	scopePrefix string
}

// issuerValidator validates the tokens of one issuer.
type issuerValidator struct {
	keyfunc   jwt.Keyfunc
	validator *jwt.Validator
	// err refuses every token of the issuer, for a trusted issuer whose
	// configuration is not safe to validate against.
	err error
}

// PendingParser is implemented by an AuthInfoParser that refuses tokens bound
// to a key in AuthInfoFromHeader, and accepts them pending their DPoP proof in
// PendingAuthInfoFromHeader. The authentication middleware uses the latter
// when a parser has it.
type PendingParser = auth.PendingParser

// The middleware relies on this to accept bound tokens.
var _ PendingParser = &JWTAuthInfoParser{}

// JWTAuthInfoParserOptions configures a JWTAuthInfoParser: the expected
// audience and issuer to validate against, an optional scope prefix to strip
// from token scopes, and the additional issuers to trust.
type JWTAuthInfoParserOptions struct {
	Audience    string
	Issuer      string
	ScopePrefix string

	// TrustedIssuers are issuers whose tokens are accepted besides the
	// primary issuer's, each validated with its own keys and audience.
	// Leaving it empty keeps the parser single-issuer.
	TrustedIssuers []TrustedIssuer
}

// TrustedIssuer is an additional token issuer for a JWTAuthInfoParser, such as
// elephant-delegate.
//
// Its tokens are held to more than the primary issuer's: they must carry an
// exp, and an aud that matches Audience. An empty Audience falls back to the
// parser's own, and a trusted issuer that ends up with no audience at all has
// every one of its tokens refused. An issuer that mints tokens for several
// services must have them say which one they are for, or a service that trusts
// the issuer would accept a token meant for another; that is why services that
// accept elephant-delegate tokens must set jwt-audience.
type TrustedIssuer struct {
	// Issuer is the iss claim of the issuer's tokens. It must differ from
	// the primary issuer.
	Issuer string
	// Audience is the aud the issuer's tokens must carry to be accepted
	// here. Empty means the parser's Audience.
	Audience string
	// Keyfunc returns the key to verify a token's signature with,
	// normally the issuer's JWKS.
	Keyfunc jwt.Keyfunc
}

// ScopePrefixRegexp returns a regular expression that matches the prefix.
//
// Deprecated: the expression matches the prefix at any word boundary, which
// includes one inside a scope suffix, as in "doc_read:<prefix>x". Use
// StripScopePrefix, which strips the prefix from the start of each scope only.
func ScopePrefixRegexp(prefix string) *regexp.Regexp {
	if prefix == "" {
		return nil
	}

	return regexp.MustCompile(fmt.Sprintf("\\b%s", regexp.QuoteMeta(prefix)))
}

// StripScopePrefix removes the prefix from the start of every scope in the
// space separated scope claim that carries it. A scope without the prefix is
// kept as it is, and the prefix is never stripped from inside a scope, such as
// from the suffix of "doc_read:<uuid>". Scopes are separated by spaces only,
// as in HasScope, so that stripping cannot turn other whitespace into a
// separator.
func StripScopePrefix(scope string, prefix string) string {
	if prefix == "" {
		return scope
	}

	scopes := auth.Scopes(scope)

	for i := range scopes {
		scopes[i] = strings.TrimPrefix(scopes[i], prefix)
	}

	return strings.Join(scopes, " ")
}

func NewJWTAuthInfoParser(
	ctx context.Context,
	keyfunc jwt.Keyfunc,
	opts JWTAuthInfoParserOptions,
) *JWTAuthInfoParser {
	cache := ttlcache.New[string, AuthInfo]()

	go func() {
		go cache.Start()

		<-ctx.Done()
		cache.Stop()
	}()

	parserOpts := []jwt.ParserOption{
		jwt.WithLeeway(tokenLeeway),
	}

	if opts.Issuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(opts.Issuer))
	}

	if opts.Audience != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(opts.Audience))
	}

	p := JWTAuthInfoParser{
		primary: &issuerValidator{
			keyfunc:   keyfunc,
			validator: jwt.NewValidator(parserOpts...),
		},
		cache:       cache,
		scopePrefix: opts.ScopePrefix,
	}

	for _, ti := range opts.TrustedIssuers {
		if p.trusted == nil {
			p.trusted = make(map[string]*issuerValidator)
		}

		p.trusted[ti.Issuer] = newTrustedValidator(ti, opts)
	}

	return &p
}

// newTrustedValidator creates the validator for a trusted issuer. A
// configuration that would be unsafe to validate against gives a validator
// that refuses every token, since the constructor cannot fail;
// AuthenticationConfigFromSettings refuses the same configurations at
// startup.
func newTrustedValidator(
	ti TrustedIssuer, opts JWTAuthInfoParserOptions,
) *issuerValidator {
	err := validateTrustedIssuer(ti, opts)
	if err != nil {
		return &issuerValidator{err: err}
	}

	if ti.Keyfunc == nil {
		return &issuerValidator{err: fmt.Errorf(
			"the trusted issuer %q has no key function", ti.Issuer)}
	}

	audience := ti.Audience
	if audience == "" {
		audience = opts.Audience
	}

	return &issuerValidator{
		keyfunc: ti.Keyfunc,
		validator: jwt.NewValidator(
			jwt.WithLeeway(tokenLeeway),
			jwt.WithIssuer(ti.Issuer),
			jwt.WithAudience(audience),
			jwt.WithExpirationRequired(),
		),
	}
}

// validateTrustedIssuer checks that the issuer and audience of a trusted
// issuer can be validated against safely.
func validateTrustedIssuer(
	ti TrustedIssuer, opts JWTAuthInfoParserOptions,
) error {
	switch {
	case ti.Issuer == "":
		return errors.New("a trusted issuer must have an issuer")
	case ti.Issuer == opts.Issuer:
		return fmt.Errorf(
			"the trusted issuer %q is the primary issuer", ti.Issuer)
	case ti.Audience == "" && opts.Audience == "":
		return fmt.Errorf(
			"the trusted issuer %q has no audience to validate against",
			ti.Issuer)
	}

	return nil
}

func NewJWKSAuthInfoParser(
	ctx context.Context, jwksURL string, opts JWTAuthInfoParserOptions,
) (*JWTAuthInfoParser, error) {
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("could not create keyfunc: %w", err)
	}

	return NewJWTAuthInfoParser(ctx, k.Keyfunc, opts), nil
}

func NewStaticAuthInfoParser(
	ctx context.Context, key ecdsa.PublicKey, opts JWTAuthInfoParserOptions,
) *JWTAuthInfoParser {
	return NewJWTAuthInfoParser(ctx, func(_ *jwt.Token) (any, error) {
		return &key, nil
	}, opts)
}

// AuthInfoFromToken validates the token and returns its AuthInfo. A token bound
// to a key is refused with ErrBoundToken, see AuthInfoFromHeader.
func (p *JWTAuthInfoParser) AuthInfoFromToken(token string) (*AuthInfo, error) {
	info, err := p.authInfoFromToken(token)
	if err != nil {
		return nil, err
	}

	if _, bound := info.Claims.BoundKey(); bound {
		return nil, ErrBoundToken
	}

	return info, nil
}

func (p *JWTAuthInfoParser) authInfoFromToken(token string) (*AuthInfo, error) {
	item := p.cache.Get(token)
	if item != nil && !item.IsExpired() {
		value := item.Value()

		return &value, nil
	}

	var claims JWTClaims

	_, err := p.validateToken(token, &claims)
	if err != nil {
		return nil, err
	}

	unitBase := &url.URL{
		Scheme: coreURIScheme,
		Host:   "unit",
	}

	for i, u := range claims.Units {
		parsed, err := url.Parse(u)
		if err != nil {
			return nil, fmt.Errorf("invalid unit claim %q: %w",
				u, err)
		}

		if parsed.Scheme == "" {
			claims.Units[i] = unitBase.ResolveReference(parsed).String()
		}
	}

	claims.Scope = StripScopePrefix(claims.Scope, p.scopePrefix)

	sub, err := claimsToSubject(claims)
	if err != nil {
		return nil, err
	}

	claims.OriginalSub = claims.Subject
	claims.Subject = sub

	auth := AuthInfo{
		Token:  token,
		Claims: claims,
	}

	if auth.Claims.ExpiresAt != nil {
		p.cache.Set(token, auth, time.Until(auth.Claims.ExpiresAt.Time))
	}

	return &auth, nil
}

// ValidateTokenWithClaims validates the token against the issuer it names
// and decodes its claims into claims. A token bound to a key is refused with
// ErrBoundToken, since the result is taken as authentication and nothing here
// checks the DPoP proof.
func (p *JWTAuthInfoParser) ValidateTokenWithClaims(token string, claims jwt.Claims) (*jwt.Token, error) {
	parsed, err := p.validateToken(token, claims)
	if err != nil {
		return nil, err
	}

	// The claims type is the caller's, so the binding is read on its own,
	// the same way JWTClaims reads it.
	var probe struct {
		jwt.RegisteredClaims

		Confirmation *Confirmation `json:"cnf"`
	}

	_, _, err = jwt.NewParser().ParseUnverified(token, &probe)
	if err != nil {
		return nil, fmt.Errorf("invalid claims: %w", err)
	}

	if probe.Confirmation != nil {
		return nil, ErrBoundToken
	}

	return parsed, nil
}

func (p *JWTAuthInfoParser) validateToken(token string, claims jwt.Claims) (*jwt.Token, error) {
	v := p.validatorFor(token)
	if v.err != nil {
		return nil, fmt.Errorf("invalid token: %w", v.err)
	}

	parsed, err := jwt.ParseWithClaims(token, claims, v.keyfunc,
		jwt.WithValidMethods(validSigningMethods))
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	err = v.validate(claims)
	if err != nil {
		return nil, fmt.Errorf("invalid claims: %w", err)
	}

	return parsed, nil
}

// validatorFor picks the validator for the issuer the token names. The issuer
// is read before the signature is checked, which is safe because the
// validator then checks the signature with that issuer's keys and the iss
// claim against that issuer. Anything that is not a trusted issuer's goes to
// the primary validator, as every token did before there were trusted issuers.
func (p *JWTAuthInfoParser) validatorFor(token string) *issuerValidator {
	if len(p.trusted) == 0 {
		return p.primary
	}

	var claims jwt.RegisteredClaims

	_, _, err := jwt.NewParser().ParseUnverified(token, &claims)
	if err != nil {
		return p.primary
	}

	v, ok := p.trusted[claims.Issuer]
	if !ok {
		return p.primary
	}

	return v
}

// AuthInfoFromHeader parses a Bearer Authorization header.
//
// A token bound to a key is refused with ErrBoundToken, whichever scheme it is
// sent with. Code that calls the parser itself, such as a websocket handler,
// takes the result as authentication and has no DPoP proof to check, so
// handing it a bound token would make that token a bearer token. Bound tokens
// are accepted by the authentication middleware of ServiceOptions, through
// PendingAuthInfoFromHeader.
func (p *JWTAuthInfoParser) AuthInfoFromHeader(authorization string) (*AuthInfo, error) {
	return p.authInfoFromHeader(authorization, false)
}

// PendingAuthInfoFromHeader parses an Authorization header like
// AuthInfoFromHeader, but also accepts a token bound to a key, with the Bearer
// scheme or the DPoP scheme of RFC 9449. The DPoP proof of such a token has
// not been checked: the info is for SetAuthInfo, which holds it back from
// GetAuthInfo until a rpc.DPoPVerifier has verified the proof. It is what the
// authentication middleware uses, and accepting a bound token sent as Bearer
// is no downgrade there, since the proof is checked either way.
func (p *JWTAuthInfoParser) PendingAuthInfoFromHeader(authorization string) (*AuthInfo, error) {
	return p.authInfoFromHeader(authorization, true)
}

func (p *JWTAuthInfoParser) authInfoFromHeader(
	authorization string, allowBound bool,
) (*AuthInfo, error) {
	if authorization == "" {
		return nil, ErrNoAuthorization
	}

	tokenType, token, _ := strings.Cut(authorization, " ")

	var dpop bool

	switch strings.ToLower(tokenType) {
	case "bearer":
	case "dpop":
		dpop = true
	default:
		return nil, errors.New("only bearer and DPoP tokens are supported")
	}

	info, err := p.authInfoFromToken(token)
	if err != nil {
		return nil, err
	}

	_, bound := info.Claims.BoundKey()

	switch {
	case bound && !allowBound:
		return nil, ErrBoundToken
	case !bound && dpop:
		return nil, errors.New(
			"the DPoP scheme is only accepted for a token bound to a key")
	}

	return info, nil
}

// coreURIScheme is the URI scheme used for subject, unit, and application
// identifiers in JWT claims.
const coreURIScheme = "core"

var (
	appURI  = url.URL{Scheme: coreURIScheme, Host: "application"}
	userURI = url.URL{Scheme: coreURIScheme, Host: "user"}
)

func claimsToSubject(claims JWTClaims) (string, error) {
	parsedSub, err := url.Parse(claims.Subject)
	if err != nil {
		return "", fmt.Errorf("invalid sub claim: %w", err)
	}

	// This is a fully qualified subject URI, return it as-is.
	if parsedSub.Scheme != "" {
		return claims.Subject, nil
	}

	// This is an application token, return
	// "core://application/{.AuthorizedParty}".
	if claims.ClientID != "" {
		return appURI.JoinPath(claims.ClientID).String(), nil
	}

	// Assume user URI and return "core://user/{.Subject}".
	return userURI.JoinPath(claims.Subject).String(), nil
}

// Valid validates the jwt.RegisteredClaims against the primary issuer.
func (p *JWTAuthInfoParser) Valid(c jwt.Claims) error {
	return p.primary.validate(c)
}

func (v *issuerValidator) validate(c jwt.Claims) error {
	err := v.validator.Validate(c)
	if err != nil {
		return fmt.Errorf("validate claims: %w", err)
	}

	return nil
}

// SetAuthInfo creates a child context with the given authentication
// information.
func SetAuthInfo(ctx context.Context, info *AuthInfo) context.Context {
	return auth.SetInfo(ctx, info)
}

// GetAuthInfo returns the authentication information for the given context.
func GetAuthInfo(ctx context.Context) (*AuthInfo, bool) {
	return auth.GetInfo(ctx)
}

// RequireAnyScope checks that the authenticated caller carries one of
// the named scopes. On success it returns the AuthInfo from the
// context; on failure it returns a twirp error suitable for direct
// return from an RPC handler. An anonymous caller (no AuthInfo, or
// an empty subject) yields Unauthenticated; an authenticated caller
// without any of the required scopes yields PermissionDenied with
// the accepted scope list in the error meta under
// "required_any_of_scopes".
//
// Scopes are OR-ed: passing more than one means the caller may hold
// any of them.
//
// Deprecated: use [github.com/ttab/elephantine/rpc.RequireAnyScope], which
// returns the same check's result as a *connect.Error. This function is that
// one with rpc.ToTwirp applied to the error, and goes away with the last Twirp
// mount in the fleet.
func RequireAnyScope(ctx context.Context, scopes ...string) (*AuthInfo, error) {
	info, err := rpc.RequireAnyScope(ctx, scopes...)
	if err != nil {
		return nil, rpc.ToTwirp(err)
	}

	return info, nil
}
