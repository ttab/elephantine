package pg

import (
	"context"
	"errors"
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
	// defaultHealthyRuntime is the default runtime a run must reach to
	// count as a success for JobLockOptions.MaxConsecutiveFailures.
	defaultHealthyRuntime = 5 * time.Minute
)

// restartPacer decides how long RunInJobLock should wait before restarting
// the guarded function, and when to stop restarting it altogether.
type restartPacer struct {
	// healthyRuntime is the runtime a run must reach to count as a
	// success, clearing the consecutive failure count.
	healthyRuntime time.Duration
	// maxFailures is the number of consecutive failures tolerated before
	// Pace gives up. Zero means that it never does.
	maxFailures int

	backoff  time.Duration
	failures int
}

// Pace returns the delay before the next restart given how long the previous
// run lasted and the error it returned. Errors are subject to exponential
// backoff with jitter, and all returns are padded so that runs start at most
// once per restartMinRuntime.
//
// When maxFailures consecutive failures have been seen Pace returns an error
// wrapping the last one instead, and the function should not be restarted. A
// run only clears the failure count by lasting healthyRuntime, so failures
// accrue towards the limit however slowly they arrive. The backoff, in
// contrast, resets after any run that reached restartMinRuntime: it is there
// to pace a job that is failing right now, not to judge its health.
func (p *restartPacer) Pace(runtime time.Duration, err error) (time.Duration, error) {
	if p.backoff == 0 || runtime >= restartMinRuntime {
		p.backoff = restartBackoffFloor
	}

	switch {
	case p.healthyRuntime > 0 && runtime >= p.healthyRuntime:
		// A run that lasted long enough to be considered healthy
		// clears the failure history, even if it ended in an error.
		p.failures = 0
	case err != nil && !isLockLoss(err):
		p.failures++
	}

	if p.maxFailures > 0 && p.failures >= p.maxFailures {
		return 0, fmt.Errorf(
			"stopping after %d consecutive failures, none of them running for %v: %w",
			p.failures, p.healthyRuntime, err)
	}

	var delay time.Duration

	if err != nil {
		// Equal jitter: delay in [backoff/2, backoff).
		//nolint:gosec // G404: jitter is not security sensitive.
		delay = p.backoff/2 + rand.N(p.backoff/2)

		p.backoff = min(2*p.backoff, restartBackoffCeil)
	}

	return max(delay, restartMinRuntime-runtime), nil
}

// isLockLoss reports whether the error is the cancellation of the context
// that was tied to the held lock. That means that the lock was lost, not that
// the job itself failed, so it must not count towards the failure limit: a
// lock that ping-pongs between replicas would otherwise take the service down
// with it.
func isLockLoss(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
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
// A panic in the function is recovered and handled as an
// elephantine.ErrPanicRecovered error return, so a panicking job is restarted
// and counted like a failing one instead of taking the process down.
//
// Restarts are paced: an error return is retried with exponential backoff
// (capped at a minute, reset after a healthy run), and any return is padded
// so that the function starts at most once every ten seconds. While waiting
// the lock stays released, so another instance can take over.
//
// Restarts are not necessarily unlimited. If
// JobLockOptions.MaxConsecutiveFailures is set, RunInJobLock gives up and
// returns an error once that many runs have failed in a row without any of
// them lasting JobLockOptions.HealthyRuntime (five minutes by default). Since
// only a run of that length clears the count, a job that fails fast reaches
// the limit however long it takes to get there, rather than accruing failures
// forever. A run cut short by the loss of the lock is not a failure.
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

	healthyRuntime := options.HealthyRuntime
	if healthyRuntime == 0 {
		healthyRuntime = defaultHealthyRuntime
	}

	pacer := restartPacer{
		healthyRuntime: healthyRuntime,
		maxFailures:    options.MaxConsecutiveFailures,
	}

	for {
		lock, err := NewJobLock(db, logger, lockName, options)
		if err != nil {
			return fmt.Errorf("create job lock: %w", err)
		}

		// The runtime is measured inside the closure so that it covers
		// the function itself, not the time spent waiting to acquire
		// the lock. It stays zero if the lock was never acquired.
		var runtime time.Duration

		err = lock.RunWithContext(ctx, func(ctx context.Context) error {
			started := time.Now()

			defer func() {
				runtime = time.Since(started)
			}()

			return elephantine.CallWithRecover(ctx, fn)
		})
		if err != nil {
			restarts.Inc()

			logger.ErrorContext(ctx,
				fmt.Sprintf("failed to run %s in job lock", serviceName),
				elephantine.LogKeyError, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		wait, giveUp := pacer.Pace(runtime, err)
		if giveUp != nil {
			return fmt.Errorf("run %s in job lock: %w",
				serviceName, giveUp)
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
