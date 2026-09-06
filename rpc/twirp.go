package rpc

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/twitchtv/twirp"
)

// twirpCodeToConnect maps a Twirp error code to the Connect code with the same
// meaning. The sixteen shared codes map to themselves. Twirp's three extra
// codes are framework generated and never returned by a handler: malformed is
// a request Twirp could not decode, which is an invalid argument; bad_route is
// a path that named no method, which is unimplemented; and no_error is the zero
// value, which cannot appear on an error at all and is reported as unknown.
func twirpCodeToConnect(code twirp.ErrorCode) connect.Code {
	switch code {
	case twirp.Canceled:
		return connect.CodeCanceled
	case twirp.Unknown:
		return connect.CodeUnknown
	case twirp.InvalidArgument:
		return connect.CodeInvalidArgument
	case twirp.Malformed:
		return connect.CodeInvalidArgument
	case twirp.DeadlineExceeded:
		return connect.CodeDeadlineExceeded
	case twirp.NotFound:
		return connect.CodeNotFound
	case twirp.BadRoute:
		return connect.CodeUnimplemented
	case twirp.AlreadyExists:
		return connect.CodeAlreadyExists
	case twirp.PermissionDenied:
		return connect.CodePermissionDenied
	case twirp.Unauthenticated:
		return connect.CodeUnauthenticated
	case twirp.ResourceExhausted:
		return connect.CodeResourceExhausted
	case twirp.FailedPrecondition:
		return connect.CodeFailedPrecondition
	case twirp.Aborted:
		return connect.CodeAborted
	case twirp.OutOfRange:
		return connect.CodeOutOfRange
	case twirp.Unimplemented:
		return connect.CodeUnimplemented
	case twirp.Internal:
		return connect.CodeInternal
	case twirp.Unavailable:
		return connect.CodeUnavailable
	case twirp.DataLoss:
		return connect.CodeDataLoss
	case twirp.NoError:
		return connect.CodeUnknown
	default:
		return connect.CodeUnknown
	}
}

// connectCodeToTwirp maps a Connect code to the Twirp error code with the same
// meaning. All sixteen map to themselves; a code outside the enumeration, which
// the Connect protocol allows on the wire, is reported as unknown.
func connectCodeToTwirp(code connect.Code) twirp.ErrorCode {
	switch code {
	case connect.CodeCanceled:
		return twirp.Canceled
	case connect.CodeUnknown:
		return twirp.Unknown
	case connect.CodeInvalidArgument:
		return twirp.InvalidArgument
	case connect.CodeDeadlineExceeded:
		return twirp.DeadlineExceeded
	case connect.CodeNotFound:
		return twirp.NotFound
	case connect.CodeAlreadyExists:
		return twirp.AlreadyExists
	case connect.CodePermissionDenied:
		return twirp.PermissionDenied
	case connect.CodeResourceExhausted:
		return twirp.ResourceExhausted
	case connect.CodeFailedPrecondition:
		return twirp.FailedPrecondition
	case connect.CodeAborted:
		return twirp.Aborted
	case connect.CodeOutOfRange:
		return twirp.OutOfRange
	case connect.CodeUnimplemented:
		return twirp.Unimplemented
	case connect.CodeInternal:
		return twirp.Internal
	case connect.CodeUnavailable:
		return twirp.Unavailable
	case connect.CodeDataLoss:
		return twirp.DataLoss
	case connect.CodeUnauthenticated:
		return twirp.Unauthenticated
	default:
		return twirp.Unknown
	}
}

// FromTwirp translates a Twirp error to a *connect.Error: the code through the
// map above, the message as it stands, and the meta map key for key into an
// ErrorMeta detail. The Twirp error is kept as the cause so that errors.Is and
// errors.As reach whatever it wrapped, a pgx or a context error included.
//
// An error that is already a *connect.Error, and an error that is neither, are
// returned unchanged: Connect gives the latter the unknown code, which is what
// it would have done without this translation.
func FromTwirp(err error) error {
	if err == nil {
		return nil
	}

	_, isConnect := errors.AsType[*connect.Error](err)
	if isConnect {
		return err
	}

	tErr, ok := errors.AsType[twirp.Error](err)
	if !ok {
		return err
	}

	out := connect.NewError(twirpCodeToConnect(tErr.Code()), &causeError{
		msg:   tErr.Msg(),
		cause: err,
	})

	return copyDetails(out, tErr.MetaMap(), nil)
}

// ToTwirp translates a *connect.Error to a twirp.Error: the code through the
// map above, the message as it stands, and the ErrorMeta detail flattened back
// into the Twirp meta map key for key. Details of any other type are dropped,
// since Twirp has nowhere to render them, and logged at debug level with their
// message name. The cause chain is preserved.
//
// A twirp.Error is returned unchanged, and any other error becomes
// twirp.InternalErrorWith, which is what the generated Twirp server does with
// an uncoded error, so nothing about uncoded errors changes.
func ToTwirp(err error) error {
	if err == nil {
		return nil
	}

	cErr, ok := errors.AsType[*connect.Error](err)
	if !ok {
		tErr, isTwirp := errors.AsType[twirp.Error](err)
		if isTwirp {
			return tErr
		}

		return twirp.InternalErrorWith(err)
	}

	tErr := twirp.NewError(connectCodeToTwirp(cErr.Code()), cErr.Message())

	meta, dropped := errorMeta(cErr)
	for key, value := range meta {
		tErr = tErr.WithMeta(key, value)
	}

	for _, d := range dropped {
		slog.Debug("dropping an error detail Twirp cannot render",
			"detail_type", d.Type())
	}

	cause := cErr.Unwrap()
	if cause != nil {
		return twirp.WrapError(tErr, cause)
	}

	return tErr
}

// TwirpInterceptor translates the errors a handler returns to Twirp errors, so
// that a handler written against the Connect error vocabulary answers a Twirp
// caller with the code, message and meta it has always answered with. Install
// it with twirp.WithServerInterceptors; elephantine.ServiceOptions.ServerOptions
// does that for you.
//
// An interceptor rather than a hook, because the error hook only sees the error
// Twirp has already decided on.
func TwirpInterceptor() twirp.Interceptor {
	return func(next twirp.Method) twirp.Method {
		return func(ctx context.Context, request any) (any, error) {
			response, err := next(ctx, request)
			if err != nil {
				return nil, ToTwirp(err)
			}

			return response, nil
		}
	}
}

// LegacyTwirpErrors translates the Twirp errors a handler returns to Connect
// errors. It is for a service that serves Connect before its handlers have been
// moved to the Connect error vocabulary, and is removed in the change that
// moves them.
func LegacyTwirpErrors() connect.Interceptor {
	return interceptor{
		unary: func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(
				ctx context.Context, req connect.AnyRequest,
			) (connect.AnyResponse, error) {
				res, err := next(ctx, req)
				if err != nil {
					return nil, FromTwirp(err)
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
				err := next(ctx, conn)
				if err != nil {
					return FromTwirp(err)
				}

				return nil
			}
		},
	}
}
