package pacer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ttab/elephantine/internal/pacer"
)

// testClock is a manually advanced clock, so that the failure budget can be
// exhausted without a test waiting for it.
type testClock struct {
	now time.Time
}

func (c *testClock) Now() time.Time {
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

// newTestPacer creates a pacer on a manually advanced clock. The clock is not
// advanced by the pacer itself, so a test that doesn't touch it exercises a
// budget that never elapses.
func newTestPacer(t *testing.T, opts pacer.Options) (*pacer.Pacer, *testClock) {
	t.Helper()

	clock := testClock{now: time.Now()}

	p := pacer.New(opts)
	p.SetClock(clock.Now)

	return p, &clock
}

func TestPacerPadsFastReturns(t *testing.T) {
	p, _ := newTestPacer(t, pacer.Options{})

	// A fast nil return is padded out to the minimum runtime.
	wait := pace(t, p, time.Millisecond, nil)

	lower := pacer.DefaultMinRuntime - 10*time.Millisecond

	if wait < lower || wait > pacer.DefaultMinRuntime {
		t.Fatalf("expected wait close to %s, got %s",
			pacer.DefaultMinRuntime, wait)
	}

	// A healthy long run is restarted immediately.
	wait = pace(t, p, pacer.DefaultMinRuntime+time.Second, nil)
	if wait != 0 {
		t.Fatalf("expected no wait after a long run, got %s", wait)
	}
}

func TestPacerBacksOffOnErrors(t *testing.T) {
	p, _ := newTestPacer(t, pacer.Options{})

	errFail := errors.New("dependency down")

	// The minimum runtime pad dominates until the backoff exceeds it,
	// then the backoff takes over and caps at the ceiling.
	var previous time.Duration

	for i := range 10 {
		wait := pace(t, p, time.Millisecond, errFail)

		if wait < previous/2 {
			t.Fatalf("iteration %d: wait %s dropped below half of previous %s",
				i, wait, previous)
		}

		if wait > pacer.DefaultBackoffCeil {
			t.Fatalf("iteration %d: wait %s exceeds the cap %s",
				i, wait, pacer.DefaultBackoffCeil)
		}

		previous = wait
	}

	// After ten consecutive fast failures the backoff must have grown
	// past the minimum runtime pad.
	if previous <= pacer.DefaultMinRuntime {
		t.Fatalf("expected backoff to exceed %s after repeated failures, got %s",
			pacer.DefaultMinRuntime, previous)
	}

	// A healthy run resets the backoff to the floor.
	wait := pace(t, p, pacer.DefaultMinRuntime+time.Second, errFail)
	if wait < pacer.DefaultBackoffFloor/2 || wait >= pacer.DefaultBackoffFloor {
		t.Fatalf("expected wait in [%s, %s) after reset, got %s",
			pacer.DefaultBackoffFloor/2, pacer.DefaultBackoffFloor, wait)
	}
}

func TestPacerJitterBounds(t *testing.T) {
	errFail := errors.New("dependency down")

	// Drive the backoff to the ceiling, then check that jittered delays
	// stay within [ceil/2, ceil).
	p, _ := newTestPacer(t, pacer.Options{})

	for range 10 {
		pace(t, p, time.Millisecond, errFail)
	}

	for range 100 {
		wait := pace(t, p, time.Millisecond, errFail)

		if wait < pacer.DefaultBackoffCeil/2 || wait >= pacer.DefaultBackoffCeil {
			t.Fatalf("expected wait in [%s, %s), got %s",
				pacer.DefaultBackoffCeil/2, pacer.DefaultBackoffCeil, wait)
		}
	}
}

// TestPacerRespectsConfiguredCurve verifies that the floor, ceiling and
// minimum runtime are honoured, which is what lets a test pace itself without
// a clock injection.
func TestPacerRespectsConfiguredCurve(t *testing.T) {
	errFail := errors.New("dependency down")

	p, _ := newTestPacer(t, pacer.Options{
		BackoffFloor: 2 * time.Millisecond,
		BackoffCeil:  8 * time.Millisecond,
		MinRuntime:   time.Millisecond,
	})

	// The floor, jittered.
	wait := pace(t, p, time.Millisecond, errFail)
	if wait < time.Millisecond || wait >= 2*time.Millisecond {
		t.Fatalf("expected the first wait in [1ms, 2ms), got %s", wait)
	}

	for range 10 {
		wait = pace(t, p, 0, errFail)

		if wait > 8*time.Millisecond {
			t.Fatalf("expected the wait to cap at 8ms, got %s", wait)
		}
	}

	if wait < 4*time.Millisecond {
		t.Fatalf("expected the backoff to reach the ceiling, got %s", wait)
	}
}

// TestPacerGivesUpAfterBudget verifies that the failure budget is wall-clock
// time since the first failure of the streak, not a number of attempts.
func TestPacerGivesUpAfterBudget(t *testing.T) {
	errFail := errors.New("dependency down")

	p, clock := newTestPacer(t, pacer.Options{GiveUpAfter: 5 * time.Minute})

	// However many times it fails, it keeps restarting while the budget
	// lasts.
	for i := range 100 {
		_, giveUp := p.Pace(time.Second, errFail)
		if giveUp != nil {
			t.Fatalf("failure %d: unexpected give up: %v", i+1, giveUp)
		}
	}

	clock.Advance(5 * time.Minute)

	_, giveUp := p.Pace(time.Second, errFail)
	if giveUp == nil {
		t.Fatal("expected the pacer to give up once the budget had elapsed")
	}

	if !errors.Is(giveUp, errFail) {
		t.Fatalf("expected the last error to be wrapped, got: %v", giveUp)
	}
}

// TestPacerBudgetCountsWaits verifies that a budget can elapse across two
// failures, as the backoff waits between them count towards it.
func TestPacerBudgetCountsWaits(t *testing.T) {
	errFail := errors.New("dependency down")

	p, clock := newTestPacer(t, pacer.Options{GiveUpAfter: time.Minute})

	_, giveUp := p.Pace(time.Second, errFail)
	if giveUp != nil {
		t.Fatalf("unexpected give up on the first failure: %v", giveUp)
	}

	clock.Advance(time.Minute)

	if _, giveUp := p.Pace(time.Second, errFail); giveUp == nil {
		t.Fatal("expected the pacer to give up on the second failure")
	}
}

// TestPacerOnlyResetsBudgetOnHealthyRuns verifies that the failure streak
// survives runs that returned without an error, but too soon to count as
// healthy. Short runs are what the budget is there to catch.
func TestPacerOnlyResetsBudgetOnHealthyRuns(t *testing.T) {
	errFail := errors.New("dependency down")

	healthy := 5 * time.Minute

	p, clock := newTestPacer(t, pacer.Options{
		GiveUpAfter:    time.Hour,
		HealthyRuntime: healthy,
	})

	_, giveUp := p.Pace(healthy-time.Second, errFail)
	if giveUp != nil {
		t.Fatalf("unexpected give up: %v", giveUp)
	}

	clock.Advance(time.Hour)

	// A nil return that was too short to be healthy neither counts as a
	// failure nor clears the streak before it.
	_, giveUp = p.Pace(time.Second, nil)
	if giveUp != nil {
		t.Fatalf("unexpected give up after a nil return: %v", giveUp)
	}

	if _, giveUp := p.Pace(time.Second, errFail); giveUp == nil {
		t.Fatal("expected the pacer to give up once the budget had elapsed")
	}

	// A run that lasted at least the healthy runtime clears the streak,
	// even though it ended in an error.
	p, clock = newTestPacer(t, pacer.Options{
		GiveUpAfter:    time.Hour,
		HealthyRuntime: healthy,
	})

	for i := range 10 {
		_, giveUp := p.Pace(time.Second, errFail)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up: %v", i, giveUp)
		}

		clock.Advance(time.Hour)

		_, giveUp = p.Pace(healthy, errFail)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up after a healthy run: %v",
				i, giveUp)
		}
	}
}

// TestPacerIgnoresCancellation verifies that a run cut short because the job
// lock was lost doesn't count towards the failure budget, and that a pacer
// without the exemption does count it.
func TestPacerIgnoresCancellation(t *testing.T) {
	p, clock := newTestPacer(t, pacer.Options{
		GiveUpAfter:        time.Minute,
		IgnoreCancellation: true,
	})

	for i := range 10 {
		clock.Advance(time.Hour)

		_, giveUp := p.Pace(time.Second, context.Canceled)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up: %v", i, giveUp)
		}
	}

	// Without the exemption a cancellation is an ordinary failure.
	p, clock = newTestPacer(t, pacer.Options{GiveUpAfter: time.Minute})

	_, giveUp := p.Pace(time.Second, context.DeadlineExceeded)
	if giveUp != nil {
		t.Fatalf("unexpected give up on the first failure: %v", giveUp)
	}

	clock.Advance(time.Minute)

	if _, giveUp := p.Pace(time.Second, context.DeadlineExceeded); giveUp == nil {
		t.Fatal("expected a cancellation to count as a failure")
	}
}

// TestPacerUnlimitedByDefault verifies that the zero value of GiveUpAfter
// restarts forever.
func TestPacerUnlimitedByDefault(t *testing.T) {
	errFail := errors.New("dependency down")

	p, clock := newTestPacer(t, pacer.Options{})

	for i := range 1000 {
		clock.Advance(time.Hour)

		_, giveUp := p.Pace(time.Millisecond, errFail)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up: %v", i, giveUp)
		}
	}
}

// pace calls Pace and fails the test if the pacer gave up, for the tests that
// only exercise restart timing.
func pace(
	t *testing.T, p *pacer.Pacer, runtime time.Duration, err error,
) time.Duration {
	t.Helper()

	wait, giveUp := p.Pace(runtime, err)
	if giveUp != nil {
		t.Fatalf("unexpected give up: %v", giveUp)
	}

	return wait
}
