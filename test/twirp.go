package test

import (
	"errors"

	"github.com/twitchtv/twirp"
)

// IsTwirpError checks that the error is a Twirp error with the given code.
//
// Deprecated: use [IsRPCError], which takes a connect.Code and accepts both
// error types, so the check does not have to change with the client
// constructor.
func IsTwirpError(
	t TestingT, err error, code twirp.ErrorCode,
) {
	t.Helper()

	var tErr twirp.Error

	ok := errors.As(err, &tErr)

	if !ok || tErr.Code() != code {
		t.Fatalf("failed: expected a %q error: got %v", code, err)
	}

	if debug() {
		t.Logf("success: got a %q error", code)
	}
}
