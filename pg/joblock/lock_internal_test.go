package joblock

import (
	"testing"
	"time"
)

// TestNextPingWaitPacesFailedPings verifies that ping retries are paced from
// the last attempt rather than the last successful ping. lastPing is frozen
// while pings fail, so a wait computed from it goes negative and busy-loops.
func TestNextPingWaitPacesFailedPings(t *testing.T) {
	interval := 10 * time.Second

	jl := Lock{
		pingInterval: interval,
		// Last successful ping long overdue, as during a database
		// outage.
		lastPing: time.Now().Add(-3 * interval),
		// A ping attempt just failed.
		lastAttempt: time.Now(),
	}

	wait := jl.nextPingWait()

	if wait < interval-time.Second || wait > interval {
		t.Fatalf("expected wait close to %s after a failed ping, got %s",
			interval, wait)
	}
}

// TestNextPingWaitHealthy verifies the normal cadence: with a fresh
// successful ping the next one is due a ping interval later.
func TestNextPingWaitHealthy(t *testing.T) {
	interval := 10 * time.Second

	jl := Lock{
		pingInterval: interval,
		lastPing:     time.Now(),
	}

	wait := jl.nextPingWait()

	if wait < interval-time.Second || wait > interval {
		t.Fatalf("expected wait close to %s, got %s", interval, wait)
	}
}
