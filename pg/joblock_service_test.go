package pg_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ttab/elephantine/pg"
)

func TestRestartPacerPadsFastReturns(t *testing.T) {
	var p pg.RestartPacer

	// A fast nil return is padded out to the minimum runtime.
	wait := p.Pace(time.Millisecond, nil)

	lower := pg.RestartMinRuntime - 10*time.Millisecond

	if wait < lower || wait > pg.RestartMinRuntime {
		t.Fatalf("expected wait close to %s, got %s",
			pg.RestartMinRuntime, wait)
	}

	// A healthy long run is restarted immediately.
	wait = p.Pace(pg.RestartMinRuntime+time.Second, nil)
	if wait != 0 {
		t.Fatalf("expected no wait after a long run, got %s", wait)
	}
}

func TestRestartPacerBacksOffOnErrors(t *testing.T) {
	var p pg.RestartPacer

	errFail := errors.New("dependency down")

	// The minimum runtime pad dominates until the backoff exceeds it,
	// then the backoff takes over and caps at the ceiling.
	var previous time.Duration

	for i := range 10 {
		wait := p.Pace(time.Millisecond, errFail)

		if wait < previous/2 {
			t.Fatalf("iteration %d: wait %s dropped below half of previous %s",
				i, wait, previous)
		}

		if wait > pg.RestartBackoffCeil {
			t.Fatalf("iteration %d: wait %s exceeds the cap %s",
				i, wait, pg.RestartBackoffCeil)
		}

		previous = wait
	}

	// After ten consecutive fast failures the backoff must have grown
	// past the minimum runtime pad.
	if previous <= pg.RestartMinRuntime {
		t.Fatalf("expected backoff to exceed %s after repeated failures, got %s",
			pg.RestartMinRuntime, previous)
	}

	// A healthy run resets the backoff to the floor.
	wait := p.Pace(pg.RestartMinRuntime+time.Second, errFail)
	if wait < pg.RestartBackoffFloor/2 || wait >= pg.RestartBackoffFloor {
		t.Fatalf("expected wait in [%s, %s) after reset, got %s",
			pg.RestartBackoffFloor/2, pg.RestartBackoffFloor, wait)
	}
}

func TestRestartPacerJitterBounds(t *testing.T) {
	errFail := errors.New("dependency down")

	// Drive the backoff to the ceiling, then check that jittered delays
	// stay within [ceil/2, ceil).
	var p pg.RestartPacer

	for range 10 {
		p.Pace(time.Millisecond, errFail)
	}

	for range 100 {
		wait := p.Pace(time.Millisecond, errFail)

		if wait < pg.RestartBackoffCeil/2 || wait >= pg.RestartBackoffCeil {
			t.Fatalf("expected wait in [%s, %s), got %s",
				pg.RestartBackoffCeil/2, pg.RestartBackoffCeil, wait)
		}
	}
}
