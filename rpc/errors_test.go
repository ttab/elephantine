package rpc_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/elephantine/test"
	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/types/known/durationpb"
)

// codeNotFound is the code string the tests use where they need one.
const codeNotFound = "not_found"

// allCodes is every code the two protocols share, which is every code a handler
// can return.
var allCodes = []connect.Code{
	connect.CodeCanceled,
	connect.CodeUnknown,
	connect.CodeInvalidArgument,
	connect.CodeDeadlineExceeded,
	connect.CodeNotFound,
	connect.CodeAlreadyExists,
	connect.CodePermissionDenied,
	connect.CodeResourceExhausted,
	connect.CodeFailedPrecondition,
	connect.CodeAborted,
	connect.CodeOutOfRange,
	connect.CodeUnimplemented,
	connect.CodeInternal,
	connect.CodeUnavailable,
	connect.CodeDataLoss,
	connect.CodeUnauthenticated,
}

// TestCodeRoundTrip takes an error with metadata through both translations for
// every shared code and checks that nothing about it changes.
func TestCodeRoundTrip(t *testing.T) {
	meta := map[string]string{
		"argument":      "uuid",
		"lock_holder":   "core://user/hugo",
		"err_count":     "3",
		"empty_ish_key": "",
	}

	for _, code := range allCodes {
		t.Run(code.String(), func(t *testing.T) {
			err := rpc.Errorf(code, "something went wrong")

			for key, value := range meta {
				err = rpc.WithMeta(err, key, value)
			}

			twirpErr, ok := errors.AsType[twirp.Error](rpc.ToTwirp(err))
			if !ok {
				t.Fatalf("did not get a Twirp error from %v", err)
			}

			test.Equalf(t, code.String(), string(twirpErr.Code()),
				"translate the code to Twirp")
			test.Equalf(t, "something went wrong", twirpErr.Msg(),
				"keep the message")
			test.EqualDiff(t, meta, twirpErr.MetaMap(),
				"flatten the metadata into the Twirp meta map")

			back, ok := errors.AsType[*connect.Error](rpc.FromTwirp(twirpErr))
			if !ok {
				t.Fatalf("did not get a Connect error from %v", twirpErr)
			}

			test.Equalf(t, code, back.Code(),
				"translate the code back")
			test.Equalf(t, "something went wrong", back.Message(),
				"keep the message")
			test.EqualDiff(t, meta, rpc.Meta(back),
				"put the metadata back in the ErrorMeta detail")
		})
	}
}

// TestTwirpOnlyCodes checks the two codes Twirp has that Connect does not. The
// third, no_error, cannot appear on an error at all.
func TestTwirpOnlyCodes(t *testing.T) {
	cases := map[twirp.ErrorCode]connect.Code{
		twirp.Malformed: connect.CodeInvalidArgument,
		twirp.BadRoute:  connect.CodeUnimplemented,
	}

	for code, want := range cases {
		t.Run(string(code), func(t *testing.T) {
			err := twirp.NewError(code, "framework said no")

			got, ok := errors.AsType[*connect.Error](rpc.FromTwirp(err))
			if !ok {
				t.Fatalf("did not get a Connect error from %v", err)
			}

			test.Equalf(t, want, got.Code(), "translate the code")
			test.Equalf(t, "framework said no", got.Message(),
				"keep the message")
		})
	}
}

// TestUncodedErrors checks that an error that carries no RPC code is treated
// the way the frameworks themselves treat it.
func TestUncodedErrors(t *testing.T) {
	plain := errors.New("boom")

	twirpErr, ok := errors.AsType[twirp.Error](rpc.ToTwirp(plain))
	if !ok {
		t.Fatalf("did not get a Twirp error from %v", plain)
	}

	test.Equalf(t, twirp.Internal, twirpErr.Code(),
		"give an uncoded error the internal code, as Twirp does")
	test.Equalf(t, "boom", twirpErr.Msg(), "keep the message")

	if !errors.Is(rpc.FromTwirp(plain), plain) {
		t.Fatal("an uncoded error should come back from FromTwirp unchanged")
	}
}

// TestErrorsArePassedThrough checks that a translation leaves an error that is
// already in the target vocabulary alone.
func TestErrorsArePassedThrough(t *testing.T) {
	twirpErr := twirp.NotFoundError("no such thing")

	//nolint:errorlint // identity is the property under test: the error has
	// to come back as it went in, not merely be reachable through a wrap.
	if rpc.ToTwirp(twirpErr) != error(twirpErr) {
		t.Fatal("a Twirp error should come back from ToTwirp unchanged")
	}

	connectErr := rpc.NotFound("no such thing")

	//nolint:errorlint // identity is the property under test, see above.
	if rpc.FromTwirp(connectErr) != connectErr {
		t.Fatal("a Connect error should come back from FromTwirp unchanged")
	}

	if rpc.ToTwirp(nil) != nil || rpc.FromTwirp(nil) != nil {
		t.Fatal("nil should translate to nil")
	}
}

// TestCauseChainSurvivesTranslation checks that errors.Is keeps reaching the
// error a handler wrapped, in both directions.
func TestCauseChainSurvivesTranslation(t *testing.T) {
	sentinel := errors.New("the underlying failure")

	err := rpc.Errorf(connect.CodeInternal, "read the thing: %w", sentinel)

	twirpErr := rpc.ToTwirp(err)

	if !errors.Is(twirpErr, sentinel) {
		t.Fatal("the cause should survive the translation to Twirp")
	}

	if !errors.Is(rpc.FromTwirp(twirpErr), sentinel) {
		t.Fatal("the cause should survive the translation back")
	}
}

// TestDroppedDetails checks that a detail Twirp cannot render does not stop the
// translation.
func TestDroppedDetails(t *testing.T) {
	err := connect.NewError(connect.CodeInternal, errors.New("nope"))

	detail, dErr := connect.NewErrorDetail(&rpc.ErrorMeta{
		Meta: map[string]string{"kept": "yes"},
	})
	test.Mustf(t, dErr, "create the metadata detail")

	err.AddDetail(detail)

	other, dErr := connect.NewErrorDetail(durationpb.New(time.Second))
	test.Mustf(t, dErr, "create the other detail")

	err.AddDetail(other)

	twirpErr, ok := errors.AsType[twirp.Error](rpc.ToTwirp(err))
	if !ok {
		t.Fatalf("did not get a Twirp error from %v", err)
	}

	test.EqualDiff(t, map[string]string{"kept": "yes"}, twirpErr.MetaMap(),
		"keep the metadata and drop the rest")
}

// TestHelperShapes checks the helpers against the Twirp helpers they replace.
func TestHelperShapes(t *testing.T) {
	cases := map[string]struct {
		Got  error
		Want twirp.Error
	}{
		"invalid_argument": {
			Got:  rpc.InvalidArgument("uuid", "is not a valid UUID"),
			Want: twirp.InvalidArgumentError("uuid", "is not a valid UUID"),
		},
		"required_argument": {
			Got:  rpc.RequiredArgument("uuid"),
			Want: twirp.RequiredArgumentError("uuid"),
		},
		codeNotFound: {
			Got:  rpc.NotFound("no such document"),
			Want: twirp.NotFoundError("no such document"),
		},
		"internal": {
			Got:  rpc.Internalf("could not read %q", "thing"),
			Want: twirp.InternalErrorf("could not read %q", "thing"),
		},
		"already_exists": {
			Got:  rpc.AlreadyExists("it is already there"),
			Want: twirp.NewError(twirp.AlreadyExists, "it is already there"),
		},
		"unauthenticated": {
			Got:  rpc.Unauthenticated("who are you"),
			Want: twirp.NewError(twirp.Unauthenticated, "who are you"),
		},
		"failed_precondition": {
			Got:  rpc.FailedPreconditionf("the document is locked by %q", "hugo"),
			Want: twirp.NewErrorf(twirp.FailedPrecondition, "the document is locked by %q", "hugo"),
		},
		"permission_denied": {
			Got:  rpc.PermissionDeniedf("not for %s", "you"),
			Want: twirp.NewErrorf(twirp.PermissionDenied, "not for %s", "you"),
		},
	}

	for name := range cases {
		tc := cases[name]

		t.Run(name, func(t *testing.T) {
			test.ErrorParity(t, tc.Want, tc.Got)
		})
	}
}

// TestInvalidArgumentfKeepsTheCause checks that the replacement for
// elephantine.InvalidArgumentf keeps behaving like it.
func TestInvalidArgumentfKeepsTheCause(t *testing.T) {
	sentinel := errors.New("not a UUID")

	err := rpc.InvalidArgumentf("uuid", "could not be parsed: %w", sentinel)

	test.IsRPCError(t, err, connect.CodeInvalidArgument)

	if !errors.Is(err, sentinel) {
		t.Fatal("the cause should be reachable")
	}

	cErr, ok := errors.AsType[*connect.Error](err)
	if !ok {
		t.Fatalf("did not get a Connect error from %v", err)
	}

	test.Equalf(t, "uuid could not be parsed: not a UUID", cErr.Message(),
		"name the argument in the message")
	test.EqualDiff(t, map[string]string{"argument": "uuid"}, rpc.Meta(err),
		"carry the argument name in the metadata")
}

// TestIsCode checks that a code check works on both error types.
func TestIsCode(t *testing.T) {
	connectErr := rpc.NotFound("gone")
	twirpErr := twirp.NotFoundError("gone")

	for name, err := range map[string]error{
		"connect": connectErr,
		"twirp":   twirpErr,
		"wrapped": fmt.Errorf("look up the document: %w", connectErr),
	} {
		t.Run(name, func(t *testing.T) {
			test.IsRPCError(t, err, connect.CodeNotFound)

			if rpc.IsCode(err, connect.CodeInternal) {
				t.Fatal("should not match another code")
			}
		})
	}

	if rpc.IsCode(nil, connect.CodeNotFound) {
		t.Fatal("nil is not an error with a code")
	}

	if rpc.IsCode(errors.New("boom"), connect.CodeNotFound) {
		t.Fatal("an uncoded error has no code to match")
	}
}

// TestMetaIsACopy checks that the metadata a caller reads cannot be used to
// change the error it came from.
func TestMetaIsACopy(t *testing.T) {
	err := rpc.WithMeta(rpc.NotFound("gone"), "key", "value")

	meta := rpc.Meta(err)
	meta["key"] = "something else"

	test.EqualDiff(t, map[string]string{"key": "value"}, rpc.Meta(err),
		"keep the error's own metadata")

	test.Equalf(t, 0, len(rpc.Meta(rpc.NotFound("gone"))),
		"report no metadata for an error that has none")
}

// TestWithMetaOnUncodedError checks that metadata can be attached to an error
// that has no code yet.
func TestWithMetaOnUncodedError(t *testing.T) {
	err := rpc.WithMeta(errors.New("boom"), "key", "value")

	test.IsRPCError(t, err, connect.CodeUnknown)
	test.EqualDiff(t, map[string]string{"key": "value"}, rpc.Meta(err),
		"carry the metadata")

	cErr, ok := errors.AsType[*connect.Error](err)
	if !ok {
		t.Fatalf("did not get a Connect error from %v", err)
	}

	test.Equalf(t, "boom", cErr.Message(), "keep the message")
}

// TestHTTPStatus checks the statuses, and names the three that differ from the
// ones Twirp answers with.
func TestHTTPStatus(t *testing.T) {
	differs := map[connect.Code]int{
		connect.CodeCanceled:           499,
		connect.CodeDeadlineExceeded:   http.StatusGatewayTimeout,
		connect.CodeFailedPrecondition: http.StatusBadRequest,
	}

	for _, code := range allCodes {
		t.Run(code.String(), func(t *testing.T) {
			got := rpc.HTTPStatus(code)

			want, isDifferent := differs[code]
			if !isDifferent {
				want = twirp.ServerHTTPStatusFromErrorCode(
					twirp.ErrorCode(code.String()))
			}

			test.Equalf(t, want, got, "report the Connect status")
		})
	}
}
