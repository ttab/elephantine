// Package rpc is the protocol-neutral RPC vocabulary for elephant services:
// error construction and inspection, the translation between Connect and Twirp
// errors, and the server and client interceptors that give a Connect mount the
// same authorization, logging and metrics behaviour the Twirp hooks give a
// Twirp mount.
//
// The neutral error type is *connect.Error. A handler constructs one through
// the helpers here, the Connect mount returns it untouched, and the Twirp mount
// translates it with TwirpInterceptor. Error metadata, which Connect has no
// free-form place for in the response body, travels as an ErrorMeta detail and
// is flattened back into Twirp's meta map by that translation.
//
// A service that has not yet moved its handlers off Twirp errors adds
// LegacyTwirpErrors to its Connect interceptors, which translates the other
// way, and removes it in the pull request that flips the handlers.
//
// # Wrapping a coded error
//
// Do not. Both stacks find a coded error anywhere in the error tree, so the
// code survives a wrapping, but nothing else does: the caller is answered with
// the inner error's message, so the wrapper's prefix is written and never read,
// and Meta and WithMeta look at the outermost coded error only. An error built
// as Errorf(code, "load the document: %w", inner) therefore answers with the
// outer code and the inner message, and reports no metadata even when inner
// carried some.
//
// That last part is deliberate rather than an oversight: re-coding an error is
// a decision about what the caller is told, and carrying the inner error's
// metadata out under a different code would tell them something else. Put the
// context in the message passed to the helper, and add the metadata the caller
// should see with WithMeta.
//
// The package deliberately does not import the elephantine root package: the
// root package's APIServer and ServiceOptions use these interceptors, so the
// dependency runs the other way. The types both packages need are declared in
// internal packages and aliased here, so rpc.AuthInfo and elephantine.AuthInfo
// are the same type.
package rpc

import (
	"context"

	"github.com/ttab/elephantine/internal/auth"
)

// AuthInfo is the authentication information of the calling client. It is an
// alias of elephantine.AuthInfo, not a separate type.
type AuthInfo = auth.Info

// JWTClaims are the claims the elephant services understand. It is an alias of
// elephantine.JWTClaims, not a separate type.
type JWTClaims = auth.JWTClaims

// GetAuthInfo returns the authentication information for the given context. It
// is the same function as elephantine.GetAuthInfo, repeated here so that a
// service that has moved to this package does not have to import both.
func GetAuthInfo(ctx context.Context) (*AuthInfo, bool) {
	return auth.GetInfo(ctx)
}

// Log metadata keys. They mirror the elephantine.LogKey* constants of the same
// names; the root package cannot be imported here, so RPCLogKeyParity in the
// tests is what keeps the two lists identical.
const (
	logKeyService    = "service"
	logKeyMethod     = "method"
	logKeySubject    = "sub"
	logKeyScopes     = "scopes"
	logKeyError      = "err"
	logKeyErrorCode  = "err_code"
	logKeyErrorMeta  = "err_meta"
	logKeyStatusCode = "status_code"
)
