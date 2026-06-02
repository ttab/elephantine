package elephantine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ttab/elephantine"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestErrGroupRequiredCancelsOnCleanReturn verifies that a Required task
// returning a nil error still cancels the group context, so a long-running
// sibling stops and Wait returns instead of blocking forever.
func TestErrGroupRequiredCancelsOnCleanReturn(t *testing.T) {
	grp := elephantine.NewErrGroup(context.Background(), discardLogger())

	siblingStopped := make(chan struct{})

	grp.Go("sibling", func(ctx context.Context) error {
		<-ctx.Done()
		close(siblingStopped)

		return nil
	})

	grp.Required("required", func(_ context.Context) error {
		// Exit cleanly and immediately.
		return nil
	})

	done := make(chan error, 1)
	go func() {
		done <- grp.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error from Wait, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after required task exited (zombie group)")
	}

	select {
	case <-siblingStopped:
	default:
		t.Fatal("sibling was not cancelled when required task returned")
	}
}

// TestErrGroupRequiredDisabledDoesNotCancel verifies that a Required task
// returning ErrTaskDisabled is treated as if it was never registered: the
// group is not cancelled and Wait returns nil once the remaining tasks finish.
func TestErrGroupRequiredDisabledDoesNotCancel(t *testing.T) {
	grp := elephantine.NewErrGroup(context.Background(), discardLogger())

	releaseSibling := make(chan struct{})

	grp.Go("sibling", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-releaseSibling:
			return nil
		}
	})

	grp.Required("disabled", func(_ context.Context) error {
		return elephantine.ErrTaskDisabled
	})

	waitReturned := make(chan error, 1)
	go func() {
		waitReturned <- grp.Wait()
	}()

	// The disabled task returned, but it must not have cancelled the
	// group: the sibling should still be running.
	select {
	case <-waitReturned:
		t.Fatal("Wait returned while a sibling was still running; disabled task cancelled the group")
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseSibling)

	select {
	case err := <-waitReturned:
		if err != nil {
			t.Fatalf("expected nil error from Wait, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after sibling completed")
	}
}

// TestErrGroupRequiredPropagatesError verifies that an error from a Required
// task is surfaced by Wait.
func TestErrGroupRequiredPropagatesError(t *testing.T) {
	grp := elephantine.NewErrGroup(context.Background(), discardLogger())

	sentinel := errors.New("boom")

	grp.Go("sibling", func(ctx context.Context) error {
		<-ctx.Done()

		return nil
	})

	grp.Required("required", func(_ context.Context) error {
		return sentinel
	})

	err := grp.Wait()
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error from Wait, got: %v", err)
	}
}

// TestErrGroupGoDoesNotCancelOnCleanReturn documents the contrast: a plain Go
// task returning nil must NOT stop its siblings. This is the zombie behaviour
// that Required exists to avoid.
func TestErrGroupGoDoesNotCancelOnCleanReturn(t *testing.T) {
	grp := elephantine.NewErrGroup(context.Background(), discardLogger())

	releaseSibling := make(chan struct{})

	grp.Go("sibling", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-releaseSibling:
			return nil
		}
	})

	grp.Go("short", func(_ context.Context) error {
		return nil
	})

	waitReturned := make(chan error, 1)
	go func() {
		waitReturned <- grp.Wait()
	}()

	// The short task has returned nil, but the sibling should still be
	// running: Wait must not have returned yet.
	select {
	case <-waitReturned:
		t.Fatal("Wait returned while a sibling was still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseSibling)

	select {
	case err := <-waitReturned:
		if err != nil {
			t.Fatalf("expected nil error from Wait, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after sibling completed")
	}
}
