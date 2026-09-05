package test

import (
	"errors"

	"connectrpc.com/connect"
	"github.com/google/go-cmp/cmp"
	"github.com/ttab/elephantine/rpc"
	"github.com/twitchtv/twirp"
)

// IsRPCError checks that the error is an RPC error with the given code. Both
// *connect.Error and twirp.Error are accepted, so a test does not have to be
// changed at the same time as the client constructor that produces its errors.
func IsRPCError(t TestingT, err error, code connect.Code) {
	t.Helper()

	if !rpc.IsCode(err, code) {
		t.Fatalf("failed: expected a %q error: got %v", code, err)
	}

	if debug() {
		t.Logf("success: got a %q error", code)
	}
}

// ErrorParity checks that a Twirp error and a Connect error describe the same
// failure: the same code, the same message and the same error metadata. It is
// what makes a service's move to the Connect error vocabulary checkable, by
// running the same call against both stacks and comparing the two errors.
func ErrorParity(t TestingT, twirpErr error, connectErr error) {
	t.Helper()

	tErr, ok := errors.AsType[twirp.Error](twirpErr)
	if !ok {
		t.Fatalf("failed: expected a Twirp error: got %v", twirpErr)

		return
	}

	cErr, ok := errors.AsType[*connect.Error](connectErr)
	if !ok {
		t.Fatalf("failed: expected a Connect error: got %v", connectErr)

		return
	}

	// The Twirp error is translated rather than compared code by code, so
	// that the parity check and the translation cannot disagree about what
	// a Twirp code means.
	want, ok := errors.AsType[*connect.Error](rpc.FromTwirp(tErr))
	if !ok {
		t.Fatalf("failed: could not translate the Twirp error %v", twirpErr)

		return
	}

	if want.Code() != cErr.Code() {
		t.Fatalf("failed: the Twirp error is %q and the Connect error is %q",
			want.Code(), cErr.Code())
	}

	if want.Message() != cErr.Message() {
		t.Fatalf("failed: the Twirp message is %q and the Connect message is %q",
			want.Message(), cErr.Message())
	}

	diff := cmp.Diff(rpc.Meta(want), rpc.Meta(cErr))
	if diff != "" {
		t.Fatalf("failed: error metadata mismatch (-twirp +connect):\n%s",
			diff)
	}

	if debug() {
		t.Logf("success: the Twirp and Connect errors match")
	}
}
