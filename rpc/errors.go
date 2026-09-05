package rpc

import (
	"errors"
	"fmt"
	"maps"
	"net/http"

	"connectrpc.com/connect"
	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"
)

// errorMetaType is the fully qualified name of the ErrorMeta message, which is
// how connect.ErrorDetail identifies a detail.
const errorMetaType = "elephantine.rpc.ErrorMeta"

// Errorf creates an error with the given code and a formatted message. A "%w"
// verb in the format wraps the error it formats, so errors.Is and errors.As
// keep reaching it through the returned error.
func Errorf(code connect.Code, format string, a ...any) error {
	return connect.NewError(code, fmt.Errorf(format, a...))
}

// InvalidArgument creates an invalid argument error for the named argument.
// The message is "<argument> <msg>" and the argument name is carried in the
// error metadata as "argument", which is the shape twirp.InvalidArgumentError
// produces.
func InvalidArgument(argument string, msg string) error {
	return WithMeta(
		connect.NewError(connect.CodeInvalidArgument,
			errors.New(argument+" "+msg)),
		"argument", argument)
}

// InvalidArgumentf creates an invalid argument error for the named argument
// with a formatted message, and is the replacement for
// elephantine.InvalidArgumentf. A "%w" verb in the format wraps the error it
// formats.
func InvalidArgumentf(argument string, format string, a ...any) error {
	pErr := fmt.Errorf(format, a...)

	err := InvalidArgument(argument, pErr.Error())

	wrapped := errors.Unwrap(pErr)
	if wrapped != nil {
		err = withCause(err, wrapped)
	}

	return err
}

// RequiredArgument creates an invalid argument error for an argument that must
// be set.
func RequiredArgument(argument string) error {
	return InvalidArgument(argument, "is required")
}

// NotFound creates a not found error.
func NotFound(msg string) error {
	return connect.NewError(connect.CodeNotFound, errors.New(msg))
}

// AlreadyExists creates an already exists error.
func AlreadyExists(msg string) error {
	return connect.NewError(connect.CodeAlreadyExists, errors.New(msg))
}

// Unauthenticated creates an unauthenticated error, for a caller we could not
// identify. Use PermissionDeniedf for a caller we could identify but that is
// not allowed to perform the operation.
func Unauthenticated(msg string) error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New(msg))
}

// Internalf creates an internal error with a formatted message. A "%w" verb in
// the format wraps the error it formats.
func Internalf(format string, a ...any) error {
	return Errorf(connect.CodeInternal, format, a...)
}

// FailedPreconditionf creates a failed precondition error with a formatted
// message. A "%w" verb in the format wraps the error it formats.
//
// Note that Connect answers a failed precondition with HTTP 400, where Twirp
// answered with 412.
func FailedPreconditionf(format string, a ...any) error {
	return Errorf(connect.CodeFailedPrecondition, format, a...)
}

// PermissionDeniedf creates a permission denied error with a formatted
// message. A "%w" verb in the format wraps the error it formats.
func PermissionDeniedf(format string, a ...any) error {
	return Errorf(connect.CodePermissionDenied, format, a...)
}

// IsCode checks if any error in the tree is an RPC error with the given code.
// Both *connect.Error and twirp.Error are recognised, so a check does not have
// to be changed at the same time as the client constructor it inspects the
// errors of.
func IsCode(err error, code connect.Code) bool {
	if err == nil {
		return false
	}

	cErr, ok := errors.AsType[*connect.Error](err)
	if ok {
		return cErr.Code() == code
	}

	tErr, ok := errors.AsType[twirp.Error](err)
	if ok {
		return twirpCodeToConnect(tErr.Code()) == code
	}

	return false
}

// Meta returns the error metadata of an RPC error: the ErrorMeta detail of a
// *connect.Error, or the meta map of a twirp.Error. It returns nil if the error
// carries no metadata.
func Meta(err error) map[string]string {
	if err == nil {
		return nil
	}

	cErr, ok := errors.AsType[*connect.Error](err)
	if ok {
		meta, _ := errorMeta(cErr)

		return meta
	}

	tErr, ok := errors.AsType[twirp.Error](err)
	if ok {
		meta := tErr.MetaMap()
		if len(meta) == 0 {
			return nil
		}

		return meta
	}

	return nil
}

// WithMeta returns a copy of the error with the given key/value pair added to
// its ErrorMeta detail, creating the detail if the error does not have one. An
// error that is not a *connect.Error is given the unknown code, which is what
// Connect would have given it anyway.
//
// The returned error is the *connect.Error itself, so anything the error was
// wrapped in is dropped.
func WithMeta(err error, key string, value string) error {
	if err == nil {
		return nil
	}

	cErr, ok := errors.AsType[*connect.Error](err)
	if !ok {
		cErr = connect.NewError(connect.CodeUnknown, err)
	}

	meta, details := errorMeta(cErr)
	if meta == nil {
		meta = make(map[string]string)
	}

	meta[key] = value

	return rebuild(cErr, meta, details)
}

// withCause returns a copy of the error that wraps cause, so that errors.Is and
// errors.As reach it, while the error message stays the one the error was
// created with.
func withCause(err error, cause error) error {
	cErr, ok := errors.AsType[*connect.Error](err)
	if !ok {
		return err
	}

	meta, details := errorMeta(cErr)

	out := connect.NewError(cErr.Code(), &causeError{
		msg:   cErr.Message(),
		cause: cause,
	})

	return copyDetails(out, meta, details)
}

// errorMeta returns the metadata of the ErrorMeta detail, and the error's other
// details. The map is a copy, so a caller can modify it.
func errorMeta(err *connect.Error) (map[string]string, []*connect.ErrorDetail) {
	var (
		meta  map[string]string
		other []*connect.ErrorDetail
	)

	for _, d := range err.Details() {
		if d.Type() != errorMetaType {
			other = append(other, d)

			continue
		}

		value, vErr := d.Value()
		if vErr != nil {
			continue
		}

		em, ok := value.(*ErrorMeta)
		if !ok {
			continue
		}

		meta = maps.Clone(em.GetMeta())
	}

	if len(meta) == 0 {
		meta = nil
	}

	return meta, other
}

// rebuild creates a new error with the same code and cause as the given one,
// carrying the given metadata and details.
func rebuild(
	err *connect.Error,
	meta map[string]string, details []*connect.ErrorDetail,
) error {
	out := connect.NewError(err.Code(), errorCause(err))

	return copyDetails(out, meta, details)
}

// copyDetails adds the metadata detail and the other details to the error.
func copyDetails(
	err *connect.Error,
	meta map[string]string, details []*connect.ErrorDetail,
) error {
	if len(meta) > 0 {
		// Marshalling a map of strings into an Any cannot fail, and
		// there is nowhere to report it if it somehow did: the caller
		// is constructing an error, not doing work that can fail.
		detail, dErr := connect.NewErrorDetail(&ErrorMeta{Meta: meta})
		if dErr == nil {
			err.AddDetail(detail)
		}
	}

	for _, d := range details {
		err.AddDetail(d)
	}

	return err
}

// errorCause returns the error a *connect.Error was created with, or an error
// with the same message if it has been reduced to a message.
func errorCause(err *connect.Error) error {
	cause := err.Unwrap()
	if cause != nil {
		return cause
	}

	if err.Message() == "" {
		return nil
	}

	return errors.New(err.Message())
}

// causeError carries a cause without letting it decide the error message.
type causeError struct {
	msg   string
	cause error
}

func (e *causeError) Error() string { return e.msg }

func (e *causeError) Unwrap() error { return e.cause }

// HTTPStatus returns the HTTP status code Connect responds with for an RPC
// code. It differs from the Twirp status for three codes: canceled is 499
// rather than 408, deadline_exceeded is 504 rather than 408, and
// failed_precondition is 400 rather than 412.
func HTTPStatus(code connect.Code) int {
	switch code {
	case connect.CodeCanceled:
		return 499
	case connect.CodeUnknown:
		return http.StatusInternalServerError
	case connect.CodeInvalidArgument:
		return http.StatusBadRequest
	case connect.CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodeAlreadyExists:
		return http.StatusConflict
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeFailedPrecondition:
		return http.StatusBadRequest
	case connect.CodeAborted:
		return http.StatusConflict
	case connect.CodeOutOfRange:
		return http.StatusBadRequest
	case connect.CodeUnimplemented:
		return http.StatusNotImplemented
	case connect.CodeInternal:
		return http.StatusInternalServerError
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	case connect.CodeDataLoss:
		return http.StatusInternalServerError
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

// ensure the generated message keeps satisfying proto.Message, which is what
// connect.NewErrorDetail needs.
var _ proto.Message = (*ErrorMeta)(nil)
