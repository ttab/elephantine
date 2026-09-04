package joblock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg/joblock"
)

func TestRestartPacerPadsFastReturns(t *testing.T) {
	var p joblock.RestartPacer

	// A fast nil return is padded out to the minimum runtime.
	wait := pace(t, &p, time.Millisecond, nil)

	lower := joblock.RestartMinRuntime - 10*time.Millisecond

	if wait < lower || wait > joblock.RestartMinRuntime {
		t.Fatalf("expected wait close to %s, got %s",
			joblock.RestartMinRuntime, wait)
	}

	// A healthy long run is restarted immediately.
	wait = pace(t, &p, joblock.RestartMinRuntime+time.Second, nil)
	if wait != 0 {
		t.Fatalf("expected no wait after a long run, got %s", wait)
	}
}

func TestRestartPacerBacksOffOnErrors(t *testing.T) {
	var p joblock.RestartPacer

	errFail := errors.New("dependency down")

	// The minimum runtime pad dominates until the backoff exceeds it,
	// then the backoff takes over and caps at the ceiling.
	var previous time.Duration

	for i := range 10 {
		wait := pace(t, &p, time.Millisecond, errFail)

		if wait < previous/2 {
			t.Fatalf("iteration %d: wait %s dropped below half of previous %s",
				i, wait, previous)
		}

		if wait > joblock.RestartBackoffCeil {
			t.Fatalf("iteration %d: wait %s exceeds the cap %s",
				i, wait, joblock.RestartBackoffCeil)
		}

		previous = wait
	}

	// After ten consecutive fast failures the backoff must have grown
	// past the minimum runtime pad.
	if previous <= joblock.RestartMinRuntime {
		t.Fatalf("expected backoff to exceed %s after repeated failures, got %s",
			joblock.RestartMinRuntime, previous)
	}

	// A healthy run resets the backoff to the floor.
	wait := pace(t, &p, joblock.RestartMinRuntime+time.Second, errFail)
	if wait < joblock.RestartBackoffFloor/2 || wait >= joblock.RestartBackoffFloor {
		t.Fatalf("expected wait in [%s, %s) after reset, got %s",
			joblock.RestartBackoffFloor/2, joblock.RestartBackoffFloor, wait)
	}
}

func TestRestartPacerJitterBounds(t *testing.T) {
	errFail := errors.New("dependency down")

	// Drive the backoff to the ceiling, then check that jittered delays
	// stay within [ceil/2, ceil).
	var p joblock.RestartPacer

	for range 10 {
		pace(t, &p, time.Millisecond, errFail)
	}

	for range 100 {
		wait := pace(t, &p, time.Millisecond, errFail)

		if wait < joblock.RestartBackoffCeil/2 || wait >= joblock.RestartBackoffCeil {
			t.Fatalf("expected wait in [%s, %s), got %s",
				joblock.RestartBackoffCeil/2, joblock.RestartBackoffCeil, wait)
		}
	}
}

func TestRestartPacerGivesUpAfterConsecutiveFailures(t *testing.T) {
	errFail := errors.New("dependency down")

	p := joblock.NewRestartPacer(5*time.Minute, 3)

	for i := range 2 {
		_, giveUp := p.Pace(time.Second, errFail)
		if giveUp != nil {
			t.Fatalf("failure %d: unexpected give up: %v", i+1, giveUp)
		}
	}

	_, giveUp := p.Pace(time.Second, errFail)
	if giveUp == nil {
		t.Fatal("expected the pacer to give up on the third failure")
	}

	if !errors.Is(giveUp, errFail) {
		t.Fatalf("expected the last error to be wrapped, got: %v", giveUp)
	}
}

// TestRestartPacerOnlyResetsFailuresOnHealthyRuns verifies that failures accrue
// towards the limit even when they are interspersed with runs that returned
// without an error, but too soon to count as healthy. Short runs are what the
// limit is there to catch.
func TestRestartPacerOnlyResetsFailuresOnHealthyRuns(t *testing.T) {
	errFail := errors.New("dependency down")

	healthy := 5 * time.Minute

	p := joblock.NewRestartPacer(healthy, 3)

	for i := range 2 {
		_, giveUp := p.Pace(healthy-time.Second, errFail)
		if giveUp != nil {
			t.Fatalf("failure %d: unexpected give up: %v", i+1, giveUp)
		}

		// A nil return that was too short to be healthy neither counts
		// as a failure nor clears the ones before it.
		_, giveUp = p.Pace(time.Second, nil)
		if giveUp != nil {
			t.Fatalf("failure %d: unexpected give up after a nil return: %v",
				i+1, giveUp)
		}
	}

	if _, giveUp := p.Pace(time.Second, errFail); giveUp == nil {
		t.Fatal("expected the pacer to give up on the third failure")
	}

	// A run that lasted at least the healthy runtime clears the count,
	// even though it ended in an error.
	p = joblock.NewRestartPacer(healthy, 3)

	for i := range 10 {
		_, giveUp := p.Pace(time.Second, errFail)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up: %v", i, giveUp)
		}

		_, giveUp = p.Pace(healthy, errFail)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up after a healthy run: %v",
				i, giveUp)
		}
	}
}

// TestRestartPacerIgnoresLockLoss verifies that a run cut short because the
// lock was lost doesn't count towards the failure limit.
func TestRestartPacerIgnoresLockLoss(t *testing.T) {
	p := joblock.NewRestartPacer(5*time.Minute, 3)

	for i := range 10 {
		_, giveUp := p.Pace(time.Second, context.Canceled)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up: %v", i, giveUp)
		}
	}
}

// TestRestartPacerUnlimitedByDefault verifies that the zero value of
// MaxConsecutiveFailures keeps the old behaviour of restarting forever.
func TestRestartPacerUnlimitedByDefault(t *testing.T) {
	errFail := errors.New("dependency down")

	p := joblock.NewRestartPacer(joblock.DefaultHealthyRuntime, 0)

	for i := range 1000 {
		_, giveUp := p.Pace(time.Millisecond, errFail)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up: %v", i, giveUp)
		}
	}
}

// TestPanickingRunCountsAsFailure verifies that a panic in the guarded
// function is turned into an error, the way Run runs it, and that
// the resulting error counts towards the failure limit.
func TestPanickingRunCountsAsFailure(t *testing.T) {
	err := elephantine.CallWithRecover(context.Background(),
		func(_ context.Context) error {
			panic("boom")
		})

	if _, ok := errors.AsType[elephantine.ErrPanicRecovered](err); !ok {
		t.Fatalf("expected a recovered panic error, got: %v", err)
	}

	p := joblock.NewRestartPacer(5*time.Minute, 1)

	if _, giveUp := p.Pace(time.Second, err); giveUp == nil {
		t.Fatal("expected a recovered panic to count as a failure")
	}
}

// pace calls Pace and fails the test if the pacer gave up, for the tests that
// only exercise restart timing.
func pace(
	t *testing.T, p *joblock.RestartPacer, runtime time.Duration, err error,
) time.Duration {
	t.Helper()

	wait, giveUp := p.Pace(runtime, err)
	if giveUp != nil {
		t.Fatalf("unexpected give up: %v", giveUp)
	}

	return wait
}
