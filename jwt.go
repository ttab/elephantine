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
)

// JWTClaims defines the claims that the elephant services understand.
type JWTClaims struct {
	jwt.RegisteredClaims

	OriginalSub string `json:"-"`

	Name            string   `json:"sub_name"`
	Scope           string   `json:"scope"`
	AuthorizedParty string   `json:"azp"`
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

type AuthInfoParser struct {
	keyfunc     jwt.Keyfunc
	validator   *jwt.Validator
	cache       *ttlcache.Cache[string, AuthInfo]
	scopePrefix *regexp.Regexp
}

type AuthInfoParserOptions struct {
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

func newAuthInfoParser(keyfunc jwt.Keyfunc, opts AuthInfoParserOptions) *AuthInfoParser {
	return &AuthInfoParser{
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

func NewJWKSAuthInfoParser(ctx context.Context, jwksUrl string, opts AuthInfoParserOptions) (*AuthInfoParser, error) {
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksUrl})
	if err != nil {
		return nil, fmt.Errorf("could not create keyfunc: %w", err)
	}
	return newAuthInfoParser(k.Keyfunc, opts), nil
}

func NewStaticAuthInfoParser(key ecdsa.PublicKey, opts AuthInfoParserOptions) *AuthInfoParser {
	return newAuthInfoParser(func(t *jwt.Token) (interface{}, error) {
		return &key, nil
	}, opts)
}

// AuthInfoFromHeader extracts the AuthInfo from a HTTP Authorization
// header. This is a placeholder implementation with a static JWT signing key
// that only will work with tokens that have the `iss: test` claim.
func (p *AuthInfoParser) AuthInfoFromHeader(authorization string) (*AuthInfo, error) {
	if authorization == "" {
		return nil, ErrNoAuthorization
	}

	tokenType, token, _ := strings.Cut(authorization, " ")

	tokenType = strings.ToLower(tokenType)
	if tokenType != "bearer" {
		return nil, errors.New("only bearer tokens are supported")
	}

	item := p.cache.Get(token)
	if item != nil && !item.IsExpired() {
		value := item.Value()

		return &value, nil
	}

	var claims JWTClaims

	_, err := jwt.ParseWithClaims(token, &claims, p.keyfunc,
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
	if claims.AuthorizedParty != "" {
		return appURI.JoinPath(claims.AuthorizedParty).String(), nil
	}

	// Assume user URI and return "core://user/{.Subject}".
	return userURI.JoinPath(claims.Subject).String(), nil
}

// Valid validates the jwt.RegisteredClaims.
func (p *AuthInfoParser) Valid(c JWTClaims) error {
	return p.validator.Validate(c.RegisteredClaims)
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
