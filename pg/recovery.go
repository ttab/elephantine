package pg

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// defaultBounceThreshold is how many consecutive poll-saved findings (without
// a notification arriving) trigger a Subscriber bounce by default.
const defaultBounceThreshold = 5

// FanOutRecoveryOption configures the recovery behaviour wired up by
// FanOut.EnableRecovery.
type FanOutRecoveryOption func(*fanOutRecoveryConfig)

type fanOutRecoveryConfig struct {
	threshold int
}

// WithBounceThreshold sets how many consecutive non-empty fallback-poll
// drains (calls to FanOut.Polled with found > 0) without an intervening
// notification trigger a bounce. The count is of *calls*, not items: a
// single chunky drain that catches up a backlog counts as one, so a busy
// publisher does not trip the threshold with one mistimed poll. Zero or
// negative disables the bounce; the metric is still maintained. Defaults
// to 5.
func WithBounceThreshold(n int) FanOutRecoveryOption {
	return func(c *fanOutRecoveryConfig) {
		c.threshold = n
	}
}

// recoveryTracker is the per-FanOut state for the recovery pattern. It is
// internal to the pg package; consumers interact with it through
// FanOut.EnableRecovery and FanOut.Polled.
type recoveryTracker struct {
	bounce      func()
	threshold   int
	pollCounter prometheus.Counter
	streakGauge prometheus.Gauge

	mu     sync.Mutex
	streak int
}

func newRecoveryTracker(
	bounce func(), threshold int,
	pollCounter prometheus.Counter, streakGauge prometheus.Gauge,
) *recoveryTracker {
	return &recoveryTracker{
		bounce:      bounce,
		threshold:   threshold,
		pollCounter: pollCounter,
		streakGauge: streakGauge,
	}
}

// polled is called by FanOut.Polled when a consumer reports `found` items
// from a fallback-poll iteration. The streak counts non-empty *calls*, not
// items: each non-empty drain adds 1, regardless of how much backlog the
// drain caught up. The per-item counter still tallies items, since that
// remains useful as a long-term volume signal.
func (t *recoveryTracker) polled(found int) {
	if found <= 0 {
		return
	}

	t.mu.Lock()

	t.streak++
	streak := t.streak

	bounce := t.threshold > 0 && streak >= t.threshold
	if bounce {
		t.streak = 0
	}

	t.mu.Unlock()

	if t.pollCounter != nil {
		t.pollCounter.Add(float64(found))
	}

	if t.streakGauge != nil {
		if bounce {
			t.streakGauge.Set(0)
		} else {
			t.streakGauge.Set(float64(streak))
		}
	}

	if bounce {
		t.bounce()
	}
}

// notified is called by FanOut.NotifyWithPayload when a real wire-side
// notification has been received and dispatched.
func (t *recoveryTracker) notified() {
	t.mu.Lock()
	had := t.streak > 0
	t.streak = 0
	t.mu.Unlock()

	if had && t.streakGauge != nil {
		t.streakGauge.Set(0)
	}
}

// sanitizeMetricSegment converts a channel name into a Prometheus-name-safe
// segment by replacing any character outside [a-zA-Z0-9_] with '_'.
func sanitizeMetricSegment(s string) string {
	out := make([]byte, 0, len(s))

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}

	return string(out)
}
