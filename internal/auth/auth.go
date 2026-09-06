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

// ClientID returns the id of the application the token was issued to: the
// "client_id" claim, or the authorized party ("azp") for a token that carries
// no client id, which is the shape of a token a user was issued through an
// application. It is the empty string for an anonymous caller, and is safe to
// call on a nil Info.
func (i *Info) ClientID() string {
	if i == nil {
		return ""
	}

	if i.Claims.ClientID != "" {
		return i.Claims.ClientID
	}

	return i.Claims.AuthorizedParty
}

// ClientIDFromContext returns the client id of the caller a request
// authenticated as, or the empty string for an anonymous or unauthenticated
// request.
func ClientIDFromContext(ctx context.Context) string {
	info, _ := GetInfo(ctx)

	return info.ClientID()
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

const infoCtxKey ctxKey = 1

// SetInfo creates a child context with the given authentication information.
func SetInfo(ctx context.Context, info *Info) context.Context {
	return context.WithValue(ctx, infoCtxKey, info)
}

// GetInfo returns the authentication information for the given context.
func GetInfo(ctx context.Context) (*Info, bool) {
	info, ok := ctx.Value(infoCtxKey).(*Info)

	return info, ok && info != nil
}
