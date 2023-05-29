package elephantine

import (
	"context"
	"errors"

	"github.com/twitchtv/twirp"
	"golang.org/x/exp/slog"
)

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

func LoggingHooks(
	logger *slog.Logger, scopesFunc func(context.Context) string,
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

			return ctx, nil
		},
		Error: func(ctx context.Context, err twirp.Error) context.Context {
			code := err.Code()
			status := twirp.ServerHTTPStatusFromErrorCode(err.Code())

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

			if code == twirp.PermissionDenied {
				args = append(args,
					LogKeyScopes, scopesFunc(ctx))
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
