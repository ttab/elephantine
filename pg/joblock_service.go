package pg

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ttab/elephantine"
)

// RunInJobLock will attempt to acquire a job lock and run the provided function
// until the context is cancelled.
func RunInJobLock(
	ctx context.Context,
	db *pgxpool.Pool,
	logger *slog.Logger,
	serviceName string,
	lockName string,
	options JobLockOptions,
	fn func(ctx context.Context) error,
) error {
	for {
		lock, err := NewJobLock(db, logger, lockName, options)
		if err != nil {
			return fmt.Errorf("create job lock: %w", err)
		}

		err = lock.RunWithContext(ctx, func(ctx context.Context) error {
			return fn(ctx)
		})
		if err != nil {
			logger.ErrorContext(ctx,
				fmt.Sprintf("failed to run %s in job lock", serviceName),
				elephantine.LogKeyError, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err() //nolint: wrapcheck
		default:
		}
	}
}
