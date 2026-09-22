package joblock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/internal/pacer"
)

// TestPanickingRunCountsAsFailure verifies that a panic in the guarded
// function is turned into an error, the way Run runs it, and that the
// resulting error counts towards the failure budget. The pacing itself is
// covered by the tests in internal/pacer.
func TestPanickingRunCountsAsFailure(t *testing.T) {
	err := elephantine.CallWithRecover(context.Background(),
		func(_ context.Context) error {
			panic("boom")
		})

	if _, ok := errors.AsType[elephantine.ErrPanicRecovered](err); !ok {
		t.Fatalf("expected a recovered panic error, got: %v", err)
	}

	// IgnoreCancellation is how Run configures the pacer, and a recovered
	// panic must not be mistaken for the loss of the lock.
	p := pacer.New(pacer.Options{IgnoreCancellation: true})

	if _, giveUp := p.Pace(time.Second, err); giveUp != nil {
		t.Fatalf("unexpected give up: %v", giveUp)
	}

	if p.Failures() != 1 {
		t.Fatalf("expected a recovered panic to count as a failure, got %d failures",
			p.Failures())
	}
}
