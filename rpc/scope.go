package rpc

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/ttab/elephantine/internal/auth"
)

// MetaRequiredScopes is the error metadata key that carries the scopes a
// permission denied error from RequireAnyScope would have accepted.
const MetaRequiredScopes = "required_any_of_scopes"

// errMissingScope is the message of the permission denied error, declared once
// so that the Twirp and the Connect stacks cannot drift apart on it.
var errMissingScope = errors.New("missing required scope")

// RequireAnyScope checks that the authenticated caller carries one of the named
// scopes. On success it returns the AuthInfo from the context; on failure it
// returns an error suitable for direct return from an RPC handler. An anonymous
// caller (no AuthInfo, or an empty subject) yields unauthenticated; an
// authenticated caller without any of the required scopes yields
// permission_denied with the accepted scope list in the error metadata under
// "required_any_of_scopes".
//
// Scopes are OR-ed: passing more than one means the caller may hold any of
// them.
func RequireAnyScope(
	ctx context.Context, scopes ...string,
) (*AuthInfo, error) {
	info, ok := auth.GetInfo(ctx)
	if !ok || info.Claims.Subject == "" {
		return nil, Unauthenticated("no anonymous access allowed")
	}

	if !info.Claims.HasAnyScope(scopes...) {
		err := connect.NewError(connect.CodePermissionDenied,
			errMissingScope)

		return nil, WithMeta(err,
			MetaRequiredScopes, strings.Join(scopes, " "))
	}

	return info, nil
}
