package joblock

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/internal/pacer"
)

// Run runs fn under the named job lock, restarting it until the
// context is cancelled.
//
// The function is expected to block until its context is cancelled: the
// context it receives is tied to the held lock, and returning — with or
// without an error — releases the lock, after which Run re-acquires
// it and starts the function again. This is not a way to run something
// exactly once.
//
// A panic in the function is recovered and handled as an
// elephantine.ErrPanicRecovered error return, so a panicking job is restarted
// and counted like a failing one instead of taking the process down.
//
// Restarts are paced: an error return is retried with exponential backoff
// (from Options.BackoffFloor to Options.BackoffCeil, a second to a minute by
// default, reset after a healthy run), and any return is padded so that the
// function starts at most once every Options.MinRuntime, ten seconds by
// default. While waiting the lock stays released, so another instance can
// take over.
//
// Restarts are not necessarily unlimited. If Options.GiveUpAfter is set, Run
// gives up and returns an error once the function has been failing for that
// long without any run lasting Options.HealthyRuntime (five minutes by
// default). The clock starts at the first failure and includes the waits
// between restarts, so a job that fails fast reaches the budget in about the
// time it names, rather than accruing failures forever. A run cut short by
// the loss of the lock is not a failure.
func Run(
	ctx context.Context,
	db *pgxpool.Pool,
	logger *slog.Logger,
	serviceName string,
	lockName string,
	options Options,
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

	// A run cut short by the loss of the lock surfaces as a cancellation,
	// and must not count as a failure: a lock that ping-pongs between
	// replicas would otherwise take the service down with it.
	restartPacer := pacer.New(pacer.Options{
		GiveUpAfter:        options.GiveUpAfter,
		HealthyRuntime:     options.HealthyRuntime,
		BackoffFloor:       options.BackoffFloor,
		BackoffCeil:        options.BackoffCeil,
		MinRuntime:         options.MinRuntime,
		IgnoreCancellation: true,
	})

	for {
		lock, err := New(db, logger, lockName, options)
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

		wait, giveUp := restartPacer.Pace(runtime, err)
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
