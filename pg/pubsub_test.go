package pg_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/ttab/elephantine/pg"
)

func TestFanOutOverflowDrop(t *testing.T) {
	fo := pg.NewFanOut[string]("test")

	ch := make(chan string, 1)

	ctx, cancel := context.WithCancel(context.Background())

	go fo.ListenAll(ctx, ch)

	// Give the listener time to register.
	time.Sleep(10 * time.Millisecond)

	// Fill the channel.
	err := fo.Notify("first")
	if err != nil {
		t.Fatalf("unexpected error from first Notify: %v", err)
	}

	// This should be silently dropped (default policy).
	err = fo.Notify("second")
	if err != nil {
		t.Fatalf("unexpected error from second Notify with drop policy: %v", err)
	}

	msg := <-ch
	if msg != "first" {
		t.Fatalf("expected 'first', got %q", msg)
	}

	// Channel should be empty now.
	select {
	case got := <-ch:
		t.Fatalf("expected empty channel, got %q", got)
	default:
	}

	cancel()
}

func TestFanOutOverflowError(t *testing.T) {
	fo := pg.NewFanOut[string]("test", pg.WithOverflowPolicy(pg.OverflowError))

	ch := make(chan string, 1)

	ctx, cancel := context.WithCancel(context.Background())

	go fo.ListenAll(ctx, ch)

	time.Sleep(10 * time.Millisecond)

	err := fo.Notify("first")
	if err != nil {
		t.Fatalf("unexpected error from first Notify: %v", err)
	}

	err = fo.Notify("second")
	if !errors.Is(err, pg.ErrListenerOverflow) {
		t.Fatalf("expected ErrListenerOverflow, got %v", err)
	}

	cancel()
}

func TestFanOutNotifyWithPayloadRoundTrip(t *testing.T) {
	type testMsg struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	fo := pg.NewFanOut[testMsg]("test")

	ch := make(chan testMsg, 1)

	ctx, cancel := context.WithCancel(context.Background())

	go fo.ListenAll(ctx, ch)

	time.Sleep(10 * time.Millisecond)

	data, err := json.Marshal(testMsg{Name: "hello", Value: 42})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	err = fo.NotifyWithPayload(data)
	if err != nil {
		t.Fatalf("NotifyWithPayload: %v", err)
	}

	msg := <-ch
	if msg.Name != "hello" || msg.Value != 42 {
		t.Fatalf("unexpected message: %+v", msg)
	}

	cancel()
}

func TestFanOutNotifyWithPayloadInvalidJSON(t *testing.T) {
	fo := pg.NewFanOut[string]("test")

	err := fo.NotifyWithPayload([]byte("not json{{{"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestFanOutNotifyWithPayloadOverflowError(t *testing.T) {
	fo := pg.NewFanOut[string]("test", pg.WithOverflowPolicy(pg.OverflowError))

	ch := make(chan string, 1)

	ctx, cancel := context.WithCancel(context.Background())

	go fo.ListenAll(ctx, ch)

	time.Sleep(10 * time.Millisecond)

	// Fill the channel.
	err := fo.NotifyWithPayload([]byte(`"first"`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should propagate overflow error.
	err = fo.NotifyWithPayload([]byte(`"second"`))
	if !errors.Is(err, pg.ErrListenerOverflow) {
		t.Fatalf("expected ErrListenerOverflow, got %v", err)
	}

	cancel()
}

func TestSubscriberDefaults(t *testing.T) {
	// We can't call Run without a real pool, but we can verify that
	// NewSubscriber returns a non-nil value with sensible defaults by
	// constructing one with a nil pool (we won't call Run).
	s := pg.NewSubscriber(
		slog.Default(),
		nil,
		nil,
	)

	if s == nil {
		t.Fatal("expected non-nil subscriber")
	}
}

func TestSubscriberWithOptions(t *testing.T) {
	s := pg.NewSubscriber(
		slog.Default(),
		nil,
		nil,
		pg.WithPingInterval(30*time.Second),
		pg.WithPingGrace(1*time.Minute),
		pg.WithOnReconnect(func(_ context.Context) error {
			return nil
		}),
	)

	if s == nil {
		t.Fatal("expected non-nil subscriber")
	}
}

