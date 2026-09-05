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

// AuthInfoParser validates bearer tokens and turns them into AuthInfo. See
// JWTAuthInfoParser for the standard JWT-based implementation.
type AuthInfoParser = auth.Parser

// JWTAuthInfoParser is the standard AuthInfoParser implementation. It
// validates JWTs using the configured key function, caches successful results
// until the token expires, and optionally strips a scope prefix.
type JWTAuthInfoParser struct {
	keyfunc     jwt.Keyfunc
	validator   *jwt.Validator
	cache       *ttlcache.Cache[string, AuthInfo]
	scopePrefix *regexp.Regexp
}

// JWTAuthInfoParserOptions configures a JWTAuthInfoParser: the expected
// audience and issuer to validate against, and an optional scope prefix to
// strip from token scopes.
type JWTAuthInfoParserOptions struct {
	Audience    string
	Issuer      string
	ScopePrefix string
}

func ScopePrefixRegexp(prefix string) *regexp.Regexp {
	if prefix == "" {
		return nil
	}

	return regexp.MustCompile(fmt.Sprintf("\\b%s", regexp.QuoteMeta(prefix)))
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
		jwt.WithLeeway(5 * time.Second),
	}

	if opts.Issuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(opts.Issuer))
	}

	if opts.Audience != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(opts.Audience))
	}

	return &JWTAuthInfoParser{
		keyfunc:     keyfunc,
		validator:   jwt.NewValidator(parserOpts...),
		cache:       cache,
		scopePrefix: ScopePrefixRegexp(opts.ScopePrefix),
	}
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

func (p *JWTAuthInfoParser) AuthInfoFromToken(token string) (*AuthInfo, error) {
	item := p.cache.Get(token)
	if item != nil && !item.IsExpired() {
		value := item.Value()

		return &value, nil
	}

	var claims JWTClaims

	_, err := p.ValidateTokenWithClaims(token, &claims)
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

	if p.scopePrefix != nil {
		claims.Scope = p.scopePrefix.ReplaceAllLiteralString(claims.Scope, "")
	}

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

func (p *JWTAuthInfoParser) ValidateTokenWithClaims(token string, claims jwt.Claims) (*jwt.Token, error) {
	parsed, err := jwt.ParseWithClaims(token, claims, p.keyfunc,
		jwt.WithValidMethods([]string{
			jwt.SigningMethodRS256.Name,
			jwt.SigningMethodES384.Name,
		}))
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	err = p.Valid(claims)
	if err != nil {
		return nil, fmt.Errorf("invalid claims: %w", err)
	}

	return parsed, nil
}

func (p *JWTAuthInfoParser) AuthInfoFromHeader(authorization string) (*AuthInfo, error) {
	if authorization == "" {
		return nil, ErrNoAuthorization
	}

	tokenType, token, _ := strings.Cut(authorization, " ")

	tokenType = strings.ToLower(tokenType)
	if tokenType != "bearer" {
		return nil, errors.New("only bearer tokens are supported")
	}

	return p.AuthInfoFromToken(token)
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

// Valid validates the jwt.RegisteredClaims.
func (p *JWTAuthInfoParser) Valid(c jwt.Claims) error {
	err := p.validator.Validate(c)
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
