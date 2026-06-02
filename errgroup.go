package elephantine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

// ErrTaskDisabled can be returned by a Required task to signal that it is
// disabled (typically by configuration) and should not run. The group treats
// it as if the task was never registered: it does not cancel the group, and
// Wait does not report it as an error. This lets callers register a task
// unconditionally and opt out from inside it, instead of wrapping the
// registration in a conditional.
var ErrTaskDisabled = errors.New("task disabled")

type BackoffFunction func(retry int) time.Duration

func NewErrGroup(ctx context.Context, logger *slog.Logger) *ErrGroup {
	// Derive our own cancellation before handing the context to the
	// errgroup, so that the group context tasks observe is cancelled both
	// by the errgroup (on a task error) and by us (when a Required() task
	// returns, even without an error).
	ctx, cancel := context.WithCancel(ctx)

	grp, gCtx := errgroup.WithContext(ctx)

	eg := ErrGroup{
		logger: logger,
		grp:    grp,
		gCtx:   gCtx,
		cancel: cancel,
	}

	return &eg
}

// ErrGroup is meant to be used when we run "top level" subsystems in a
// service. If a task panics it will be handled as a ErrPanicRecovered error.
type ErrGroup struct {
	logger *slog.Logger
	grp    *errgroup.Group
	gCtx   context.Context
	cancel context.CancelFunc
}

func (eg *ErrGroup) Go(task string, fn func(ctx context.Context) error) {
	eg.grp.Go(func() error {
		eg.logger.Info("starting task",
			LogKeyName, task)

		defer eg.logger.Info("stopped task",
			LogKeyName, task)

		err := CallWithRecover(eg.gCtx, fn)
		if err != nil {
			return fmt.Errorf("%s: %w", task, err)
		}

		return nil
	})
}

// Required runs a task that the rest of the group depends on. Unlike Go, the
// group context is cancelled as soon as the task returns — even if it returns
// a nil error — which stops the sibling tasks and unblocks Wait.
//
// Use it for subsystems that must run for the entire lifetime of the service:
// if one exits for any reason we want the whole service to stop and be
// restarted, rather than linger with only a subset of its subsystems running.
// A nil return still yields a nil Wait result unless a sibling reports an
// error, so a clean shutdown stays clean.
//
// A task that is disabled by configuration can return ErrTaskDisabled to opt
// out: the group is then left untouched, as if the task had never been
// registered.
func (eg *ErrGroup) Required(task string, fn func(ctx context.Context) error) {
	eg.grp.Go(func() error {
		eg.logger.Info("starting task",
			LogKeyName, task)

		defer eg.logger.Info("stopped task",
			LogKeyName, task)

		err := CallWithRecover(eg.gCtx, fn)

		// A disabled task opted out of running, so leave the rest of
		// the group alone — it's as if the task was never registered.
		if errors.Is(err, ErrTaskDisabled) {
			eg.logger.Info("task disabled",
				LogKeyName, task)

			return nil
		}

		// The group can't continue without this task, so cancel the
		// group context now that it has returned, regardless of
		// whether it failed. This stops the sibling tasks instead of
		// leaving them running without a subsystem they depend on.
		eg.cancel()

		if err != nil {
			return fmt.Errorf("%s: %w", task, err)
		}

		return nil
	})
}

// GoWithRetries runs a task in a retry loop. The retry counter will reset to
// zero if more time than `resetAfter` has passed since the last error. This is
// used to avoid creeping up on a retry limit over long periods of time.
func (eg *ErrGroup) GoWithRetries(
	task string,
	maxRetries int,
	backoff BackoffFunction,
	resetAfter time.Duration,
	fn func(ctx context.Context) error,
) {
	eg.grp.Go(func() error {
		var tries int

		// Count starting as a state change.
		lastStateChange := time.Now()

		for {
			err := CallWithRecover(eg.gCtx, fn)
			if err == nil {
				return nil
			}

			// Bail immediately if the group has ben cancelled.
			if eg.gCtx.Err() != nil {
				return fmt.Errorf("%s: %w", task, eg.gCtx.Err())
			}

			// If it's been a long time since we last failed we
			// don't want to creep up on a retry limit over the
			// course of days, weeks, or months.
			if time.Since(lastStateChange) > resetAfter {
				tries = 0
			}

			lastStateChange = time.Now()
			tries++

			if maxRetries != 0 && tries > maxRetries {
				return fmt.Errorf(
					"%s: stopping after %d tries:  %w",
					task, tries, err)
			}

			wait := backoff(tries)

			eg.logger.ErrorContext(eg.gCtx,
				"task failure, restarting",
				LogKeyName, task,
				LogKeyError, err,
				LogKeyAttempts, tries,
				LogKeyDelay, slog.DurationValue(wait),
			)

			select {
			case <-time.After(wait):
			case <-eg.gCtx.Done():
				return fmt.Errorf("%s: %w", task, eg.gCtx.Err())
			}
		}
	})
}

func (eg *ErrGroup) Wait() error {
	// Release the context we derived in NewErrGroup once every task has
	// returned.
	defer eg.cancel()

	return eg.grp.Wait()
}

type ErrPanicRecovered struct {
	PanicValue any
}

func (err ErrPanicRecovered) Error() string {
	return fmt.Sprintf("recovered from panic: %v", err.PanicValue)
}

func CallWithRecover(ctx context.Context, fn func(ctx context.Context) error) (outErr error) {
	defer func() {
		if r := recover(); r != nil {
			outErr = ErrPanicRecovered{PanicValue: r}
		}
	}()

	return fn(ctx)
}

func StaticBackoff(wait time.Duration) BackoffFunction {
	return func(_ int) time.Duration {
		return wait
	}
}
