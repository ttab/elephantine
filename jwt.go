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
	"github.com/twitchtv/twirp"
)

// JWTClaims defines the claims that the elephant services understand.
type JWTClaims struct {
	jwt.RegisteredClaims

	OriginalSub string `json:"-"`

	Name            string   `json:"sub_name"`
	Scope           string   `json:"scope"`
	AuthorizedParty string   `json:"azp"`
	ClientID        string   `json:"client_id"`
	Units           []string `json:"units,omitempty"`
}

// HasScope returns true if the Scope claim contains the named scope.
func (c JWTClaims) HasScope(name string) bool {
	scopes := strings.Split(c.Scope, " ")

	for i := range scopes {
		if scopes[i] == name {
			return true
		}
	}

	return false
}

// HasScope returns true if the Scope claim contains any of the named scopes.
func (c JWTClaims) HasAnyScope(names ...string) bool {
	scopes := strings.Split(c.Scope, " ")

	for i := range scopes {
		for j := range names {
			if scopes[i] == names[j] {
				return true
			}
		}
	}

	return false
}

const authInfoCtxKey ctxKey = 1

// AuthInfo is used to add authentication information to a request context.
type AuthInfo struct {
	Token  string
	Claims JWTClaims
}

// ErrNoAuthorization is used to communicate that authorization was completely
// missing, rather than being invalid, expired, or malformed.
var ErrNoAuthorization = errors.New("no authorization provided")

type AuthInfoParser interface {
	// AuthInfoFromHeader extracts the AuthInfo from a HTTP Authorization
	// header, then validates the bearer token. Return ErrNoAuthorization
	// if no authorization information was provided.
	AuthInfoFromHeader(authorization string) (*AuthInfo, error)
	// AuthInfoFromToken validates a bearer token and returns the AuthInfo.
	// Useful when we have already extracted the token from header and/or
	// query parameter.
	AuthInfoFromToken(token string) (*AuthInfo, error)
	// ValidateTokenWithClaims validates a bearer token and returns the raw token
	// object. Useful if you need to do custom claims deserialization.
	ValidateTokenWithClaims(token string, claims jwt.Claims) (*jwt.Token, error)
}

type JWTAuthInfoParser struct {
	keyfunc     jwt.Keyfunc
	validator   *jwt.Validator
	cache       *ttlcache.Cache[string, AuthInfo]
	scopePrefix *regexp.Regexp
}

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

func newJWTAuthInfoParser(keyfunc jwt.Keyfunc, opts JWTAuthInfoParserOptions) *JWTAuthInfoParser {
	return &JWTAuthInfoParser{
		keyfunc: keyfunc,
		validator: jwt.NewValidator(
			jwt.WithLeeway(5*time.Second),
			jwt.WithIssuer(opts.Issuer),
			jwt.WithAudience(opts.Audience),
		),
		cache:       ttlcache.New[string, AuthInfo](),
		scopePrefix: ScopePrefixRegexp(opts.ScopePrefix),
	}
}

func NewJWKSAuthInfoParser(ctx context.Context, jwksUrl string, opts JWTAuthInfoParserOptions) (*JWTAuthInfoParser, error) {
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksUrl})
	if err != nil {
		return nil, fmt.Errorf("could not create keyfunc: %w", err)
	}
	return newJWTAuthInfoParser(k.Keyfunc, opts), nil
}

func NewStaticAuthInfoParser(key ecdsa.PublicKey, opts JWTAuthInfoParserOptions) *JWTAuthInfoParser {
	return newJWTAuthInfoParser(func(t *jwt.Token) (interface{}, error) {
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
		Scheme: "core",
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

var (
	appURI  = url.URL{Scheme: "core", Host: "application"}
	userURI = url.URL{Scheme: "core", Host: "user"}
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
	return p.validator.Validate(c)
}

// SetAuthInfo creates a child context with the given authentication
// information.
func SetAuthInfo(ctx context.Context, info *AuthInfo) context.Context {
	return context.WithValue(ctx, authInfoCtxKey, info)
}

// GetAuthInfo returns the authentication information for the given context.
func GetAuthInfo(ctx context.Context) (*AuthInfo, bool) {
	info, ok := ctx.Value(authInfoCtxKey).(*AuthInfo)

	return info, ok && info != nil
}

func RequireAnyScope(ctx context.Context, scopes ...string) (*AuthInfo, error) {
	auth, ok := GetAuthInfo(ctx)
	if !ok {
		return nil, twirp.Unauthenticated.Error(
			"no anonymous access allowed")
	}

	if !auth.Claims.HasAnyScope(scopes...) {
		return nil, twirp.PermissionDenied.Errorf(
			"one of the the scopes %s is required",
			strings.Join(scopes, ", "))
	}

	return auth, nil
}
