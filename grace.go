package elephantine

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// GracefulShutdown is a helper that can be used to listen for SIGINT and
// SIGTERM to gracefully shut down your application.
//
// SIGTERM will trigger a stop, followed by quit after the specified
// timeout. SIGINT will trigger a immediate quit.
type GracefulShutdown struct {
	logger  *slog.Logger
	m       sync.Mutex
	signals chan os.Signal
	stop    chan struct{}
	quit    chan struct{}
}

// NewGracefulShutdown creates a new GracefulShutdown that will wait for
// `timeout` between "stop" and "quit".
func NewGracefulShutdown(logger *slog.Logger, timeout time.Duration) *GracefulShutdown {
	return newGracefulShutdown(logger, timeout, true)
}

// NewManualGracefulShutdown creates a GracefulShutdown instance that doesn't
// listen to OS signals.
func NewManualGracefulShutdown(logger *slog.Logger, timeout time.Duration) *GracefulShutdown {
	return newGracefulShutdown(logger, timeout, false)
}

func newGracefulShutdown(
	logger *slog.Logger, timeout time.Duration,
	listenToSignals bool,
) *GracefulShutdown {
	gs := GracefulShutdown{
		logger:  logger,
		signals: make(chan os.Signal, 1),
		stop:    make(chan struct{}),
		quit:    make(chan struct{}),
	}

	if listenToSignals {
		signal.Notify(gs.signals, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			for {
				if !gs.poll() {
					break
				}
			}

			// Stop subscription.
			signal.Stop(gs.signals)
		}()
	}

	go func() {
		<-gs.stop

		select {
		case <-gs.quit:
			return
		default:
			logger.Warn("asked to stop, waiting for cleanup",
				LogKeyDelay, timeout)
		}

		time.Sleep(timeout)

		logger.Warn("shutting down")
		gs.safeClose(gs.quit)
	}()

	return &gs
}

func (gs *GracefulShutdown) poll() bool {
	select {
	case sig := <-gs.signals:
		gs.handleSignal(sig)

		return true
	case <-gs.quit:
		return false
	}
}

func (gs *GracefulShutdown) safeClose(ch chan struct{}) {
	gs.m.Lock()
	defer gs.m.Unlock()

	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (gs *GracefulShutdown) handleSignal(sig os.Signal) {
	switch sig.String() {
	case syscall.SIGINT.String():
		gs.logger.Warn("shutting down")
		gs.safeClose(gs.quit)
		gs.safeClose(gs.stop)
	case syscall.SIGTERM.String():
		gs.safeClose(gs.stop)
	}
}

// Stop triggers a stop, which will trigger quit after the configured timeout.
func (gs *GracefulShutdown) Stop() {
	gs.safeClose(gs.stop)
}

// ShouldStop returns a channel that will be closed when stop is triggered.
func (gs *GracefulShutdown) ShouldStop() <-chan struct{} {
	return gs.stop
}

// ShouldQuit returns a channel that will be closed when quit is triggered.
func (gs *GracefulShutdown) ShouldQuit() <-chan struct{} {
	return gs.quit
}

// CancelOnStop returns a child context that will be cancelled when stop is
// triggered.
func (gs *GracefulShutdown) CancelOnStop(ctx context.Context) context.Context {
	cCtx, cancel := context.WithCancel(ctx)

	go func() {
		<-gs.stop
		cancel()
	}()

	return cCtx
}

// CancelOnQuit returns a child context that will be cancelled when quit is
// triggered.
func (gs *GracefulShutdown) CancelOnQuit(ctx context.Context) context.Context {
	cCtx, cancel := context.WithCancel(ctx)

	go func() {
		<-gs.quit
		cancel()
	}()

	return cCtx
}
