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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

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

	// Confirmation is the RFC 7800 "cnf" claim of a token bound to a
	// key. A bound token is only accepted with a DPoP proof made with that
	// key, see BoundKey.
	Confirmation *Confirmation `json:"cnf,omitempty"`
	// DocumentUUID binds an elephant-delegate document token to a
	// document. It is a claim of its own rather than a scope suffix, and
	// only a token minted after a permission check on the document carries
	// it.
	DocumentUUID string `json:"document_uuid,omitempty"`
	// AssignmentUUID narrows a document binding to one assignment within
	// the document.
	AssignmentUUID string `json:"assignment_uuid,omitempty"`
}

// BoundKey returns the RFC 7638 thumbprint of the key the token is bound to,
// and false for a token that is not bound to a key.
func (c JWTClaims) BoundKey() (string, bool) {
	if c.Confirmation == nil {
		return "", false
	}

	return c.Confirmation.JKT, true
}

// Confirmation is the "cnf" claim of RFC 7800. The only confirmation method
// that is understood is the "jkt" of RFC 9449, the thumbprint of a DPoP key;
// a claim with any other member is refused when the token is parsed, since
// nothing here could check it, and accepting it would turn a bound token into
// a bearer token.
type Confirmation struct {
	JKT string `json:"jkt"`
}

// UnmarshalJSON refuses a confirmation that is not a lone, non-empty jkt.
func (c *Confirmation) UnmarshalJSON(data []byte) error {
	var members map[string]json.RawMessage

	err := json.Unmarshal(data, &members)
	if err != nil {
		return fmt.Errorf("the cnf claim is not an object: %w", err)
	}

	for name := range members {
		if name != "jkt" {
			return fmt.Errorf(
				"unsupported confirmation method %q in the cnf claim", name)
		}
	}

	var jkt string

	err = json.Unmarshal(members["jkt"], &jkt)
	if err != nil || jkt == "" {
		return errors.New("the cnf claim has no jkt")
	}

	c.JKT = jkt

	return nil
}

// HasScope returns true if the Scope claim contains the named scope. Scopes
// are matched exactly: "doc_read:<uuid>" is a scope of its own and does not
// satisfy "doc_read".
func (c JWTClaims) HasScope(name string) bool {
	return slices.Contains(strings.Fields(c.Scope), name)
}

// HasAnyScope returns true if the Scope claim contains any of the named
// scopes. Scopes are matched exactly, as in HasScope.
func (c JWTClaims) HasAnyScope(names ...string) bool {
	for scope := range strings.FieldsSeq(c.Scope) {
		if slices.Contains(names, scope) {
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

const (
	infoCtxKey ctxKey = iota + 1
	proofCtxKey
)

// proofState records whether the DPoP proof for a key-bound token has been
// verified. It is a pointer on the context, set when the info is, so that the
// interceptors outside the one that verifies the proof, such as logging and
// metrics, see the caller once it has been verified further in.
type proofState struct {
	info     *Info
	verified atomic.Bool
}

// SetInfo creates a child context with the given authentication information.
//
// The information of a token bound to a key is held back by GetInfo until
// MarkProofVerified has been called for it, so that nothing reading the caller
// from the context can act on a bound token whose proof has not been checked.
func SetInfo(ctx context.Context, info *Info) context.Context {
	ctx = context.WithValue(ctx, infoCtxKey, info)

	if info == nil {
		return ctx
	}

	if _, bound := info.Claims.BoundKey(); bound {
		ctx = context.WithValue(ctx, proofCtxKey, &proofState{info: info})
	}

	return ctx
}

// GetInfo returns the authentication information for the given context. A
// token bound to a key is only returned once its proof has been verified.
func GetInfo(ctx context.Context) (*Info, bool) {
	info, ok := PendingInfo(ctx)
	if !ok {
		return nil, false
	}

	if _, bound := info.Claims.BoundKey(); !bound {
		return info, true
	}

	state, _ := ctx.Value(proofCtxKey).(*proofState)
	if state == nil || state.info != info || !state.verified.Load() {
		return nil, false
	}

	return info, true
}

// PendingInfo returns the authentication information for the given context
// whether or not a proof it needs has been verified. It is for the code that
// verifies the proof, and for nothing that authorizes a call.
func PendingInfo(ctx context.Context) (*Info, bool) {
	info, ok := ctx.Value(infoCtxKey).(*Info)

	return info, ok && info != nil
}

// ProofVerified reports whether the info on the context is bound to a key and
// has had its proof verified.
func ProofVerified(ctx context.Context) bool {
	info, ok := PendingInfo(ctx)
	if !ok {
		return false
	}

	state, _ := ctx.Value(proofCtxKey).(*proofState)

	return state != nil && state.info == info && state.verified.Load()
}

// MarkProofVerified records that the proof for the key-bound token on the
// context has been verified, which makes GetInfo return it. It is a no-op for
// a context with no bound token on it.
func MarkProofVerified(ctx context.Context) {
	info, ok := PendingInfo(ctx)
	if !ok {
		return
	}

	state, _ := ctx.Value(proofCtxKey).(*proofState)
	if state == nil || state.info != info {
		return
	}

	state.verified.Store(true)
}
