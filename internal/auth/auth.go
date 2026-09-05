// Package auth holds the authentication types and the request-scoped
// authentication state that both the elephantine root package and the rpc
// package need.
//
// They live here rather than in the root package because the root package uses
// the Connect interceptors in rpc, and rpc needs the caller's AuthInfo. The
// types are declared once here and aliased by both packages, so
// elephantine.AuthInfo and rpc.AuthInfo are the same type.
package auth

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// JWTClaims defines the claims that the elephant services understand.
type JWTClaims struct {
	jwt.RegisteredClaims

	OriginalSub string `json:"-"`

	Name            string   `json:"sub_name"`
	Email           string   `json:"email"`
	Scope           string   `json:"scope"`
	AuthorizedParty string   `json:"azp"`
	ClientID        string   `json:"client_id"`
	Units           []string `json:"units,omitempty"`
	Org             string   `json:"org"`
}

// HasScope returns true if the Scope claim contains the named scope.
func (c JWTClaims) HasScope(name string) bool {
	scopes := strings.Split(c.Scope, " ")

	return slices.Contains(scopes, name)
}

// HasAnyScope returns true if the Scope claim contains any of the named scopes.
func (c JWTClaims) HasAnyScope(names ...string) bool {
	scopes := strings.Split(c.Scope, " ")

	for i := range scopes {
		if slices.Contains(names, scopes[i]) {
			return true
		}
	}

	return false
}

// Info is used to add authentication information to a request context. It is
// exposed as elephantine.AuthInfo and rpc.AuthInfo.
type Info struct {
	Token  string
	Claims JWTClaims
}

// ErrNoAuthorization is used to communicate that authorization was completely
// missing, rather than being invalid, expired, or malformed.
var ErrNoAuthorization = errors.New("no authorization provided")

// Parser validates bearer tokens and turns them into Info.
type Parser interface {
	// AuthInfoFromHeader extracts the Info from a HTTP Authorization
	// header, then validates the bearer token. Return ErrNoAuthorization
	// if no authorization information was provided.
	AuthInfoFromHeader(authorization string) (*Info, error)
	// AuthInfoFromToken validates a bearer token and returns the Info.
	// Useful when we have already extracted the token from header and/or
	// query parameter.
	AuthInfoFromToken(token string) (*Info, error)
	// ValidateTokenWithClaims validates a bearer token and returns the raw
	// token object. Useful if you need to do custom claims
	// deserialization.
	ValidateTokenWithClaims(token string, claims jwt.Claims) (*jwt.Token, error)
}

type ctxKey int

const (
	infoCtxKey  ctxKey = 1
	errorCtxKey ctxKey = 2
)

// SetInfo creates a child context with the given authentication information.
func SetInfo(ctx context.Context, info *Info) context.Context {
	return context.WithValue(ctx, infoCtxKey, info)
}

// GetInfo returns the authentication information for the given context.
func GetInfo(ctx context.Context) (*Info, bool) {
	info, ok := ctx.Value(infoCtxKey).(*Info)

	return info, ok && info != nil
}

// SetError creates a child context carrying the reason the request could not
// be authenticated. The authentication middleware is protocol neutral and
// cannot render an RPC error itself, so it marks the request and lets it
// through; the Twirp hook and the Connect interceptor that guard the handlers
// turn the marker into a coded error in the protocol the caller is speaking.
func SetError(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, errorCtxKey, err)
}

// GetError returns the authentication error recorded for the context, or nil
// if the request authenticated.
func GetError(ctx context.Context) error {
	err, _ := ctx.Value(errorCtxKey).(error)

	return err
}
