package test_test

import (
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ttab/elephantine/test"
)

// blockingLogger is a TestingLogger whose Log blocks until it is released, so
// that a write can be held in flight between the log handler's done check and
// the end of the Log call. That is the window where the real *testing.T
// panics with "Log in goroutine after TestX has completed".
type blockingLogger struct {
	entered  chan struct{}
	release  chan struct{}
	cleanups []func()

	mu sync.Mutex
	// complete models testing's own done flag: it is set once the cleanups
	// have returned, which is the point where a further Log would panic.
	complete bool
	late     int
	calls    int
}

func (l *blockingLogger) Log(_ ...any) {
	l.mu.Lock()
	l.calls++

	if l.complete {
		l.late++
	}

	l.mu.Unlock()

	l.entered <- struct{}{}

	<-l.release
}

func (l *blockingLogger) Cleanup(fn func()) {
	l.cleanups = append(l.cleanups, fn)
}

// runCleanups runs the registered cleanups last in first out the way testing
// does, and then marks the test complete.
func (l *blockingLogger) runCleanups() {
	for _, fn := range slices.Backward(l.cleanups) {
		fn()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.complete = true
}

func (l *blockingLogger) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.calls, l.late
}

func TestLogHandlerCleanupWaitsForInFlightWrite(t *testing.T) {
	tl := &blockingLogger{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	logger := slog.New(test.NewLogHandler(tl, slog.LevelInfo))

	// Records from a derived handler go through the same guarded Write, and
	// a derived handler is what the panic in the wild was logged through.
	derived := logger.With("component", "worker")

	writeDone := make(chan struct{})

	go func() {
		defer close(writeDone)

		derived.Info("in flight")
	}()

	// The write is now inside Log, past the done check.
	<-tl.entered

	cleanupDone := make(chan struct{})

	go func() {
		defer close(cleanupDone)

		tl.runCleanups()
	}()

	// A cleanup that returns here would let the test be marked complete
	// with a Log call still in flight, which is the panic. The wait cannot
	// expire spuriously, since the cleanup stays blocked on the handler
	// mutex until the write below is released.
	select {
	case <-cleanupDone:
		t.Fatal("cleanup returned while a write was in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(tl.release)

	<-writeDone
	<-cleanupDone

	// Everything logged after the cleanup must be dropped, both on the
	// handler itself and on handlers derived from it.
	logger.Info("late")
	derived.Error("late")
	logger.WithGroup("g").Warn("late")

	calls, late := tl.counts()

	if late != 0 {
		t.Errorf("got %d Log calls after completion, want none", late)
	}

	if calls != 1 {
		t.Errorf("got %d Log calls, want exactly 1", calls)
	}
}
