package elephantine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

type BackoffFunction func(retry int) time.Duration

func NewErrGroup(ctx context.Context, logger *slog.Logger) *ErrGroup {
	grp, gCtx := errgroup.WithContext(ctx)

	eg := ErrGroup{
		logger: logger,
		grp:    grp,
		gCtx:   gCtx,
	}

	return &eg
}

type ErrGroup struct {
	logger *slog.Logger
	grp    *errgroup.Group
	gCtx   context.Context
}

func (eg *ErrGroup) Go(task string, fn func(ctx context.Context) error) {
	eg.grp.Go(func() error {
		eg.logger.Info("starting task",
			LogKeyName, task)

		defer eg.logger.Info("stopped task",
			LogKeyName, task)

		err := fn(eg.gCtx)
		if err != nil {
			return fmt.Errorf("%s: %w", task, err)
		}

		return nil
	})
}

// GoWithRetries runs a task in a retry look. The retry counter will reset to
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
			err := fn(eg.gCtx)
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
	return eg.grp.Wait()
}

func StaticBackoff(wait time.Duration) BackoffFunction {
	return func(_ int) time.Duration {
		return wait
	}
}
