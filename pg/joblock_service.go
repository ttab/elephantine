package pg

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
)

const (
	// restartMinRuntime is the minimum time between two starts of the
	// function run by RunInJobLock. A run that returns faster than this is
	// padded out to it before the lock is re-acquired, and a run that
	// lasts at least this long is considered healthy and resets the error
	// backoff.
	restartMinRuntime = 10 * time.Second
	// restartBackoffFloor and restartBackoffCeil bound the exponential
	// backoff applied when the function returns an error.
	restartBackoffFloor = 1 * time.Second
	restartBackoffCeil  = 60 * time.Second
)

// restartPacer decides how long RunInJobLock should wait before restarting
// the guarded function.
type restartPacer struct {
	backoff time.Duration
}

// Pace returns the delay before the next restart given how long the previous
// run lasted and the error it returned. Errors are subject to exponential
// backoff with jitter, and all returns are padded so that runs start at most
// once per restartMinRuntime.
func (p *restartPacer) Pace(runtime time.Duration, err error) time.Duration {
	if p.backoff == 0 || runtime >= restartMinRuntime {
		p.backoff = restartBackoffFloor
	}

	var delay time.Duration

	if err != nil {
		// Equal jitter: delay in [backoff/2, backoff).
		//nolint:gosec // G404: jitter is not security sensitive.
		delay = p.backoff/2 + rand.N(p.backoff/2)

		p.backoff = min(2*p.backoff, restartBackoffCeil)
	}

	return max(delay, restartMinRuntime-runtime)
}

// RunInJobLock runs fn under the named job lock, restarting it until the
// context is cancelled.
//
// The function is expected to block until its context is cancelled: the
// context it receives is tied to the held lock, and returning — with or
// without an error — releases the lock, after which RunInJobLock re-acquires
// it and starts the function again. This is not a way to run something
// exactly once.
//
// Restarts are paced: an error return is retried with exponential backoff
// (capped at a minute, reset after a healthy run), and any return is padded
// so that the function starts at most once every ten seconds. While waiting
// the lock stays released, so another instance can take over.
func RunInJobLock(
	ctx context.Context,
	db *pgxpool.Pool,
	logger *slog.Logger,
	serviceName string,
	lockName string,
	options JobLockOptions,
	fn func(ctx context.Context) error,
) error {
	reg := options.MetricsRegisterer
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	restartsVec, err := elephantine.RegisterOrReuse(reg,
		prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pg_job_lock_restarts_total",
			Help: "Restarts of a job lock guarded function after " +
				"an error return. A sustained rate means the " +
				"job is failing persistently and only being " +
				"kept alive by restart backoff.",
		}, []string{jobLockNameLabel}))
	if err != nil {
		return fmt.Errorf("register job lock restarts metric: %w", err)
	}

	restarts := restartsVec.WithLabelValues(lockName)

	var pacer restartPacer

	for {
		lock, err := NewJobLock(db, logger, lockName, options)
		if err != nil {
			return fmt.Errorf("create job lock: %w", err)
		}

		started := time.Now()

		err = lock.RunWithContext(ctx, fn)
		if err != nil {
			restarts.Inc()

			logger.ErrorContext(ctx,
				fmt.Sprintf("failed to run %s in job lock", serviceName),
				elephantine.LogKeyError, err)
		}

		wait := pacer.Pace(time.Since(started), err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		case <-timer.C:
		}
	}
}
