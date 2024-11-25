package elephantine

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/twitchtv/twirp"
)

// IsTwirpErrorCode checks if any error in the tree is a twirp.Error with the
// given error code.
func IsTwirpErrorCode(err error, code twirp.ErrorCode) bool {
	if err == nil {
		return false
	}

	var te twirp.Error

	if errors.As(err, &te) {
		return te.Code() == code
	}

	return false
}

// TwirpErrorToHTTPStatusCode returns the HTTP status code for the given
// error. If the error is nil 200 will be returned, if the error isn't a
// twirp.Error 500 will be returned.
func TwirpErrorToHTTPStatusCode(err error) int {
	if err == nil {
		return http.StatusOK
	}

	var te twirp.Error

	if errors.As(err, &te) {
		return twirp.ServerHTTPStatusFromErrorCode(te.Code())
	}

	return http.StatusInternalServerError
}

// LoggingHooks creaes a twirp.ServerHooks that will set log metadata for the
// twirp service and method name, and log error responses.
func LoggingHooks(
	logger *slog.Logger,
) *twirp.ServerHooks {
	hooks := twirp.ServerHooks{
		RequestRouted: func(ctx context.Context) (context.Context, error) {
			service, ok := twirp.ServiceName(ctx)
			if ok {
				SetLogMetadata(ctx, LogKeyService, service)
			}

			method, ok := twirp.MethodName(ctx)
			if ok {
				SetLogMetadata(ctx, LogKeyMethod, method)
			}

			auth, ok := GetAuthInfo(ctx)
			if ok {
				SetLogMetadata(ctx, LogKeySubject, auth.Claims.Subject)
			}

			return ctx, nil
		},
		Error: func(ctx context.Context, err twirp.Error) context.Context {
			code := err.Code()
			status := twirp.ServerHTTPStatusFromErrorCode(err.Code())
			auth, hasAuth := GetAuthInfo(ctx)

			args := []any{
				LogKeyErrorCode, code,
				LogKeyError, err.Msg(),
				LogKeyStatusCode, status,
			}

			meta := err.MetaMap()
			if meta != nil {
				args = append(args,
					LogKeyErrorMeta, err.MetaMap())
			}

			if code == twirp.PermissionDenied && hasAuth {
				args = append(args,
					LogKeyScopes, auth.Claims.Scope)
			}

			level := slog.LevelWarn

			switch {
			case status == 400:
				level = slog.LevelInfo
			case status >= 500:
				level = slog.LevelError
			}

			logger.Log(ctx, level, "error response", args...)

			return ctx
		},
	}

	return &hooks
}
