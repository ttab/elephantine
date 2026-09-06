package rpc

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/ttab/elephantine/internal/auth"
	"github.com/ttab/elephantine/internal/logmeta"
	"github.com/ttab/elephantine/internal/rpcmetrics"
)

// LoggingInterceptor sets the service, method and subject log metadata for the
// request and logs error responses. It is the Connect counterpart of
// elephantine.LoggingHooks and reports the same keys at the same levels, so a
// dual-stack service's logs do not depend on the protocol the caller used.
func LoggingInterceptor(logger *slog.Logger) connect.Interceptor {
	return interceptor{
		unary: func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(
				ctx context.Context, req connect.AnyRequest,
			) (connect.AnyResponse, error) {
				setCallLogMetadata(ctx, req.Spec().Procedure)

				res, err := next(ctx, req)
				if err != nil {
					LogErrorResponse(ctx, logger, err)

					return nil, err
				}

				return res, nil
			}
		},
		streamingHandler: func(
			next connect.StreamingHandlerFunc,
		) connect.StreamingHandlerFunc {
			return func(
				ctx context.Context, conn connect.StreamingHandlerConn,
			) error {
				setCallLogMetadata(ctx, conn.Spec().Procedure)

				err := next(ctx, conn)
				if err != nil {
					LogErrorResponse(ctx, logger, err)

					return err
				}

				return nil
			}
		},
	}
}

// setCallLogMetadata records the service, the method and the authenticated
// subject of a call as log metadata.
func setCallLogMetadata(ctx context.Context, procedure string) {
	service, method := splitProcedure(procedure)

	logmeta.Set(ctx, logKeyService, service)
	logmeta.Set(ctx, logKeyMethod, method)

	info, hasAuth := auth.GetInfo(ctx)
	if hasAuth {
		logmeta.Set(ctx, logKeySubject, info.Claims.Subject)
	}
}

// LogErrorResponse logs an error response with the keys and levels
// elephantine.LoggingHooks and LoggingInterceptor use. Middleware that answers
// a request before it reaches a handler chain — the authentication middleware
// in elephantine.ServiceOptions is the one in the fleet — logs the refusal with
// it, so that a refused request is logged the way a refused call is.
//
// The level follows the HTTP status the code is answered with: a bad request or
// a not found is informational, a server fault is an error, and everything else
// is a warning. An error that is not a *connect.Error but that wraps a context
// cancellation or a deadline is logged as canceled or deadline_exceeded, which
// is how Connect answers it, rather than as an unknown server fault.
func LogErrorResponse(ctx context.Context, logger *slog.Logger, err error) {
	var (
		code    = ResponseCode(err)
		message = err.Error()
	)

	cErr, ok := errors.AsType[*connect.Error](err)
	if ok {
		message = cErr.Message()
	}

	status := HTTPStatus(code)
	info, hasAuth := auth.GetInfo(ctx)

	args := []any{
		logKeyErrorCode, code.String(),
		logKeyError, message,
		logKeyStatusCode, status,
	}

	meta := Meta(err)
	if meta != nil {
		args = append(args, logKeyErrorMeta, meta)
	}

	if code == connect.CodePermissionDenied && hasAuth {
		args = append(args, logKeyScopes, info.Claims.Scope)
	}

	level := slog.LevelWarn

	switch {
	case status == 400 || status == 404:
		level = slog.LevelInfo
	case status >= 500:
		level = slog.LevelError
	}

	logger.Log(ctx, level, "error response", args...)
}

// splitProcedure splits a Connect procedure ("/elephant.repository.Documents/Get")
// into the short service name ("Documents") and the method name ("Get"). The
// short name is what twirp.ServiceName reports, so the two stacks label a
// series the same way.
func splitProcedure(procedure string) (string, string) {
	service, method, ok := rpcmetrics.SplitProcedure(procedure)
	if !ok {
		return "", ""
	}

	return service, method
}
