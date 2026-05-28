package pg_test

import (
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
)

func TestFanOutRecoveryBouncesAtThreshold(t *testing.T) {
	var bounced atomic.Int32

	reg := prometheus.NewRegistry()
	fo := pg.NewFanOut[string]("test_channel")

	err := fo.EnableRecovery(
		elephantine.NewMetricsHelper(reg),
		func() { bounced.Add(1) },
		pg.WithBounceThreshold(3),
	)
	if err != nil {
		t.Fatalf("EnableRecovery: %v", err)
	}

	fo.Polled(1)
	fo.Polled(1)

	if got := bounced.Load(); got != 0 {
		t.Fatalf("expected no bounce before threshold, got %d", got)
	}

	fo.Polled(1)

	if got := bounced.Load(); got != 1 {
		t.Fatalf("expected one bounce at threshold, got %d", got)
	}

	// Streak resets after bouncing.
	fo.Polled(1)

	if got := bounced.Load(); got != 1 {
		t.Fatalf("expected streak to reset after bounce, got %d bounces", got)
	}
}

func TestFanOutRecoveryResetsOnWireNotification(t *testing.T) {
	var bounced atomic.Int32

	reg := prometheus.NewRegistry()
	fo := pg.NewFanOut[string]("test_channel")

	err := fo.EnableRecovery(
		elephantine.NewMetricsHelper(reg),
		func() { bounced.Add(1) },
		pg.WithBounceThreshold(3),
	)
	if err != nil {
		t.Fatalf("EnableRecovery: %v", err)
	}

	fo.Polled(1)
	fo.Polled(1)

	// Simulate a wire-side delivery (the Subscriber would call this on
	// each PG NOTIFY).
	err = fo.NotifyWithPayload([]byte(`"hello"`))
	if err != nil {
		t.Fatalf("NotifyWithPayload: %v", err)
	}

	fo.Polled(1)
	fo.Polled(1)

	if got := bounced.Load(); got != 0 {
		t.Fatalf("expected no bounce after wire reset, got %d", got)
	}
}

func TestFanOutRecoveryDirectNotifyDoesNotReset(t *testing.T) {
	var bounced atomic.Int32

	reg := prometheus.NewRegistry()
	fo := pg.NewFanOut[string]("test_channel")

	err := fo.EnableRecovery(
		elephantine.NewMetricsHelper(reg),
		func() { bounced.Add(1) },
		pg.WithBounceThreshold(2),
	)
	if err != nil {
		t.Fatalf("EnableRecovery: %v", err)
	}

	fo.Polled(1)

	// A direct in-process Notify is not a wire signal and must not reset
	// the recovery streak. The next Polled should trip the threshold.
	err = fo.Notify("synthetic")
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	fo.Polled(1)

	if got := bounced.Load(); got != 1 {
		t.Fatalf("expected bounce despite intervening direct Notify, got %d", got)
	}
}

func TestFanOutRecoveryStreakCountsCallsNotItems(t *testing.T) {
	var bounced atomic.Int32

	reg := prometheus.NewRegistry()
	fo := pg.NewFanOut[string]("test_channel")

	err := fo.EnableRecovery(
		elephantine.NewMetricsHelper(reg),
		func() { bounced.Add(1) },
		pg.WithBounceThreshold(3),
	)
	if err != nil {
		t.Fatalf("EnableRecovery: %v", err)
	}

	// A single fat backlog drain must not trip the threshold; only
	// consecutive non-empty calls count, not items.
	fo.Polled(1000)

	if got := bounced.Load(); got != 0 {
		t.Fatalf("expected no bounce from a single chunky drain, got %d", got)
	}

	fo.Polled(1)

	if got := bounced.Load(); got != 0 {
		t.Fatalf("expected no bounce at 2 consecutive drains, got %d", got)
	}

	fo.Polled(1)

	if got := bounced.Load(); got != 1 {
		t.Fatalf("expected bounce at 3 consecutive drains, got %d", got)
	}
}

func TestFanOutPolledNoopWithoutRecovery(t *testing.T) {
	fo := pg.NewFanOut[string]("test_channel")
	// Must not panic or count anything when EnableRecovery has not been
	// called; consumers may call Polled unconditionally.
	fo.Polled(5)
}

func TestFanOutRecoveryRegistersMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	fo := pg.NewFanOut[string]("outbox.new") // exercise sanitization

	err := fo.EnableRecovery(
		elephantine.NewMetricsHelper(reg),
		func() {},
	)
	if err != nil {
		t.Fatalf("EnableRecovery: %v", err)
	}

	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	names := map[string]bool{}
	for _, mf := range gathered {
		names[mf.GetName()] = true
	}

	for _, want := range []string{
		"pg_fanout_outbox_new_poll_saved_total",
		"pg_fanout_outbox_new_poll_saved_streak",
	} {
		if !names[want] {
			t.Fatalf("expected metric %q registered, got %v", want, names)
		}
	}
}
