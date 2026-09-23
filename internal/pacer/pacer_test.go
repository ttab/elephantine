package pacer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ttab/elephantine/internal/pacer"
)

// newPacer creates a pacer, failing the test if the options are rejected.
func newPacer(t *testing.T, opts pacer.Options) *pacer.Pacer {
	t.Helper()

	p, err := pacer.New(opts)
	if err != nil {
		t.Fatalf("create pacer: %v", err)
	}

	return p
}

func TestPacerPadsFastReturns(t *testing.T) {
	p := newPacer(t, pacer.Options{})

	// A fast nil return is padded out to the minimum runtime.
	wait := pace(t, p, time.Millisecond, nil)

	lower := pacer.DefaultMinRuntime - 10*time.Millisecond

	if wait < lower || wait > pacer.DefaultMinRuntime {
		t.Fatalf("expected wait close to %s, got %s",
			pacer.DefaultMinRuntime, wait)
	}

	// A run past the minimum runtime is restarted immediately.
	wait = pace(t, p, pacer.DefaultMinRuntime+time.Second, nil)
	if wait != 0 {
		t.Fatalf("expected no wait after a long run, got %s", wait)
	}
}

func TestPacerBacksOffOnErrors(t *testing.T) {
	p := newPacer(t, pacer.Options{})

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
}

// TestPacerResetsBackoffOnMinRuntime verifies that the backoff resets after a
// run that only reached the minimum runtime — it paces a task that is failing
// right now, and does not wait for a healthy run to judge that.
func TestPacerResetsBackoffOnMinRuntime(t *testing.T) {
	errFail := errors.New("dependency down")

	p := newPacer(t, pacer.Options{})

	for range 10 {
		pace(t, p, time.Millisecond, errFail)
	}

	wait := pace(t, p, pacer.DefaultMinRuntime+time.Second, errFail)
	if wait < pacer.DefaultBackoffFloor/2 || wait >= pacer.DefaultBackoffFloor {
		t.Fatalf("expected wait in [%s, %s) after reset, got %s",
			pacer.DefaultBackoffFloor/2, pacer.DefaultBackoffFloor, wait)
	}
}

// TestPacerResetsBackoffOnHealthyRun verifies the same for a run that lasted
// the healthy runtime, which additionally clears the failure budget.
func TestPacerResetsBackoffOnHealthyRun(t *testing.T) {
	errFail := errors.New("dependency down")

	p := newPacer(t, pacer.Options{})

	for range 10 {
		pace(t, p, time.Millisecond, errFail)
	}

	wait := pace(t, p, pacer.DefaultHealthyRuntime, errFail)
	if wait < pacer.DefaultBackoffFloor/2 || wait >= pacer.DefaultBackoffFloor {
		t.Fatalf("expected wait in [%s, %s) after a healthy run, got %s",
			pacer.DefaultBackoffFloor/2, pacer.DefaultBackoffFloor, wait)
	}

	// The healthy run cleared the streak, and the error that ended it
	// opened a new one — it is the first failure of that streak, not a
	// restart with no failures behind it.
	if p.Failures() != 1 {
		t.Fatalf("expected the failure that ended the healthy run to count, got %d",
			p.Failures())
	}
}

func TestPacerJitterBounds(t *testing.T) {
	errFail := errors.New("dependency down")

	// Drive the backoff to the ceiling, then check that jittered delays
	// stay within [ceil/2, ceil).
	p := newPacer(t, pacer.Options{})

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

	p := newPacer(t, pacer.Options{
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

// TestPacerGivesUpAfterBudget verifies that the budget is the time spent
// failing — the failing runs and the waits between them — rather than a
// number of attempts.
func TestPacerGivesUpAfterBudget(t *testing.T) {
	errFail := errors.New("dependency down")

	// Runs that fail after a minute, restarted immediately, so the budget
	// is spent a minute at a time.
	p := newPacer(t, pacer.Options{
		GiveUpAfter:    10 * time.Minute,
		HealthyRuntime: 5 * time.Minute,
		MinRuntime:     time.Second,
	})

	for i := range 10 {
		_, giveUp := p.Pace(time.Minute, errFail)
		if giveUp != nil {
			t.Fatalf("failure %d: unexpected give up: %v", i+1, giveUp)
		}
	}

	// The eleventh failure is the one that carries the streak past ten
	// minutes: the clock starts at the first failure, not at the start of
	// the run that failed first.
	_, giveUp := p.Pace(time.Minute, errFail)
	if giveUp == nil {
		t.Fatal("expected the pacer to give up once the budget was spent")
	}

	if !errors.Is(giveUp, errFail) {
		t.Fatalf("expected the last error to be wrapped, got: %v", giveUp)
	}
}

// TestPacerBudgetCountsWaits verifies that the waits between restarts are
// spent from the budget, not only the time the function was running.
func TestPacerBudgetCountsWaits(t *testing.T) {
	errFail := errors.New("dependency down")

	// A backoff that stays under the minimum runtime makes every wait the
	// ten second pad, so the budget is spent ten seconds per restart.
	p := newPacer(t, pacer.Options{
		GiveUpAfter:    90 * time.Second,
		HealthyRuntime: time.Minute,
		BackoffFloor:   time.Second,
		BackoffCeil:    time.Second,
		MinRuntime:     10 * time.Second,
	})

	// A task that fails instantly spends nothing but those waits: the
	// first failure opens the streak, and the nine after it carry it to
	// ninety seconds.
	for i := range 9 {
		_, giveUp := p.Pace(0, errFail)
		if giveUp != nil {
			t.Fatalf("failure %d: unexpected give up: %v", i+1, giveUp)
		}
	}

	if _, giveUp := p.Pace(0, errFail); giveUp == nil {
		t.Fatal("expected the waits alone to spend the budget")
	}
}

// TestPacerOnlyResetsBudgetOnHealthyRuns verifies that the failure streak
// survives runs that returned without an error, but too soon to count as
// healthy. Short runs are what the budget is there to catch.
func TestPacerOnlyResetsBudgetOnHealthyRuns(t *testing.T) {
	errFail := errors.New("dependency down")

	healthy := 5 * time.Minute

	p := newPacer(t, pacer.Options{
		GiveUpAfter:    10 * time.Minute,
		HealthyRuntime: healthy,
		MinRuntime:     time.Second,
	})

	for i := range 10 {
		_, giveUp := p.Pace(time.Minute, errFail)
		if giveUp != nil {
			t.Fatalf("failure %d: unexpected give up: %v", i+1, giveUp)
		}

		// A nil return that was too short to be healthy neither counts
		// as a failure nor clears the streak before it.
		_, giveUp = p.Pace(time.Second, nil)
		if giveUp != nil {
			t.Fatalf("failure %d: unexpected give up after a nil return: %v",
				i+1, giveUp)
		}
	}

	if _, giveUp := p.Pace(time.Minute, errFail); giveUp == nil {
		t.Fatal("expected the pacer to give up once the budget was spent")
	}

	// A run that lasted at least the healthy runtime clears the streak,
	// even though it ended in an error, so the budget never runs out.
	p = newPacer(t, pacer.Options{
		GiveUpAfter:    10 * time.Minute,
		HealthyRuntime: healthy,
		MinRuntime:     time.Second,
	})

	for i := range 100 {
		_, giveUp := p.Pace(time.Minute, errFail)
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

// TestPacerIgnoresCancellation verifies that a run cut short because the job
// lock was lost doesn't count towards the failure budget, and that a pacer
// without the exemption does count it.
func TestPacerIgnoresCancellation(t *testing.T) {
	p := newPacer(t, pacer.Options{
		GiveUpAfter:        10 * time.Minute,
		HealthyRuntime:     5 * time.Minute,
		MinRuntime:         time.Second,
		IgnoreCancellation: true,
	})

	for i := range 100 {
		_, giveUp := p.Pace(time.Minute, context.Canceled)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up: %v", i, giveUp)
		}
	}

	// Without the exemption a cancellation is an ordinary failure.
	p = newPacer(t, pacer.Options{
		GiveUpAfter:    10 * time.Minute,
		HealthyRuntime: 5 * time.Minute,
		MinRuntime:     time.Second,
	})

	for range 10 {
		pace(t, p, time.Minute, context.DeadlineExceeded)
	}

	if _, giveUp := p.Pace(time.Minute, context.DeadlineExceeded); giveUp == nil {
		t.Fatal("expected a cancellation to count as a failure")
	}
}

// TestPacerLockChurnDoesNotSpendBudget is the scenario the budget must
// survive: one genuine failure, then a long run of lock holds each ended by
// the loss of the lock, then another genuine failure. Lock churn is not the
// job being broken, and the lengths of those holds are not the caller's to
// choose, so no configuration could avoid it if they spent the budget.
func TestPacerLockChurnDoesNotSpendBudget(t *testing.T) {
	errFail := errors.New("db hiccup")

	p := newPacer(t, pacer.Options{
		GiveUpAfter:        20 * time.Minute,
		HealthyRuntime:     5 * time.Minute,
		IgnoreCancellation: true,
	})

	pace(t, p, time.Second, errFail)

	// Seven three-minute holds, each ended by the loss of the lock: too
	// short to be healthy, and adding up to more than the budget.
	for i := range 7 {
		_, giveUp := p.Pace(3*time.Minute, context.Canceled)
		if giveUp != nil {
			t.Fatalf("lock loss %d: unexpected give up: %v", i+1, giveUp)
		}
	}

	if _, giveUp := p.Pace(time.Second, errFail); giveUp != nil {
		t.Fatalf("two failures twenty-one minutes apart gave up: %v", giveUp)
	}
}

// TestPacerUnlimitedByDefault verifies that the zero value of GiveUpAfter
// restarts forever.
func TestPacerUnlimitedByDefault(t *testing.T) {
	errFail := errors.New("dependency down")

	p := newPacer(t, pacer.Options{})

	for i := range 1000 {
		_, giveUp := p.Pace(time.Millisecond, errFail)
		if giveUp != nil {
			t.Fatalf("iteration %d: unexpected give up: %v", i, giveUp)
		}
	}
}

// TestPacerRejectsBudgetBelowHealthyRuntime verifies that a budget nothing can
// clear is refused rather than accepted as a two-strikes rule.
func TestPacerRejectsBudgetBelowHealthyRuntime(t *testing.T) {
	cases := []struct {
		name     string
		opts     pacer.Options
		rejected bool
	}{
		{
			name: "budget below the default healthy runtime",
			opts: pacer.Options{
				GiveUpAfter: time.Minute,
			},
			rejected: true,
		},
		{
			name: "budget equal to the healthy runtime",
			opts: pacer.Options{
				GiveUpAfter:    time.Hour,
				HealthyRuntime: time.Hour,
			},
			rejected: true,
		},
		{
			name: "budget above the healthy runtime",
			opts: pacer.Options{
				GiveUpAfter:    5 * time.Minute,
				HealthyRuntime: time.Minute,
			},
		},
		{
			name: "no budget at all",
			opts: pacer.Options{
				HealthyRuntime: time.Hour,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := pacer.New(c.opts)

			switch {
			case c.rejected && err == nil:
				t.Fatal("expected the options to be rejected")
			case !c.rejected && err != nil:
				t.Fatalf("expected the options to be accepted, got: %v", err)
			}
		})
	}
}

// TestPacerClampsNegativeDurations verifies that a negative duration is taken
// as a mistake rather than a request. A negative healthy runtime would
// otherwise make every run healthy and silently disable the budget.
func TestPacerClampsNegativeDurations(t *testing.T) {
	errFail := errors.New("dependency down")

	p := newPacer(t, pacer.Options{
		GiveUpAfter:    10 * time.Minute,
		HealthyRuntime: -time.Hour,
		MinRuntime:     time.Second,
	})

	for i := range 10 {
		_, giveUp := p.Pace(time.Minute, errFail)
		if giveUp != nil {
			t.Fatalf("failure %d: unexpected give up: %v", i+1, giveUp)
		}
	}

	if _, giveUp := p.Pace(time.Minute, errFail); giveUp == nil {
		t.Fatal("expected a negative healthy runtime to be replaced by the default")
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
