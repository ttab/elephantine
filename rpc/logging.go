package rpc

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	"github.com/ttab/elephantine/internal/auth"
	"github.com/ttab/elephantine/internal/logmeta"
)

// LoggingInterceptor sets the service, method and subject log metadata for the
// request and logs error responses. It is the Connect counterpart of
// elephantine.LoggingHooks and reports the same keys at the same levels, so a
// dual-stack service's logs do not depend on the protocol the caller used.
func LoggingInterceptor(logger *slog.Logger) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(
			ctx context.Context, req connect.AnyRequest,
		) (connect.AnyResponse, error) {
			service, method := splitProcedure(req.Spec().Procedure)

			logmeta.Set(ctx, logKeyService, service)
			logmeta.Set(ctx, logKeyMethod, method)

			info, hasAuth := auth.GetInfo(ctx)
			if hasAuth {
				logmeta.Set(ctx, logKeySubject, info.Claims.Subject)
			}

			res, err := next(ctx, req)
			if err != nil {
				logError(ctx, logger, err)

				return nil, err
			}

			return res, nil
		}
	})
}

// logError logs an error response with the keys and levels
// elephantine.LoggingHooks uses.
func logError(ctx context.Context, logger *slog.Logger, err error) {
	var (
		code    = connect.CodeUnknown
		message = err.Error()
	)

	cErr, ok := errors.AsType[*connect.Error](err)
	if ok {
		code = cErr.Code()
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
	full, method, ok := strings.Cut(strings.TrimPrefix(procedure, "/"), "/")
	if !ok {
		return "", ""
	}

	if i := strings.LastIndex(full, "."); i != -1 {
		full = full[i+1:]
	}

	return full, method
}
