package elephantine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine/internal/pacer"
	"golang.org/x/sync/errgroup"
)

// ErrTaskDisabled can be returned by a Required task to signal that it is
// disabled (typically by configuration) and should not run. The group treats
// it as if the task was never registered: it does not cancel the group, and
// Wait does not report it as an error. This lets callers register a task
// unconditionally and opt out from inside it, instead of wrapping the
// registration in a conditional.
var ErrTaskDisabled = errors.New("task disabled")

// ErrGroupOption customises the behaviour of an ErrGroup.
type ErrGroupOption func(o *errGroupOptions)

type errGroupOptions struct {
	reg prometheus.Registerer
}

// WithErrGroupMetricsRegisterer overrides the registerer used for the task
// metrics. Defaults to prometheus.DefaultRegisterer.
func WithErrGroupMetricsRegisterer(reg prometheus.Registerer) ErrGroupOption {
	return func(o *errGroupOptions) {
		o.reg = reg
	}
}

func NewErrGroup(
	ctx context.Context, logger *slog.Logger, opts ...ErrGroupOption,
) *ErrGroup {
	o := errGroupOptions{
		reg: prometheus.DefaultRegisterer,
	}

	for _, opt := range opts {
		opt(&o)
	}

	restarts := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "task_restarts_total",
		Help: "Times a task has been restarted after a failure.",
	}, []string{"task"})

	registered, err := RegisterOrReuse(o.reg, restarts)
	if err != nil {
		// Fall back to the unregistered vector rather than failing
		// group creation over a metrics conflict.
		logger.Error("failed to register task restart metric",
			LogKeyError, err.Error())
	} else {
		restarts = registered
	}

	// Derive our own cancellation before handing the context to the
	// errgroup, so that the group context tasks observe is cancelled both
	// by the errgroup (on a task error) and by us (when a Required() task
	// returns, even without an error).
	ctx, cancel := context.WithCancel(ctx)

	grp, gCtx := errgroup.WithContext(ctx)

	eg := ErrGroup{
		logger:   logger,
		grp:      grp,
		gCtx:     gCtx,
		cancel:   cancel,
		restarts: restarts,
	}

	return &eg
}

// ErrGroup is meant to be used when we run "top level" subsystems in a
// service. If a task panics it will be handled as a ErrPanicRecovered error.
type ErrGroup struct {
	logger   *slog.Logger
	grp      *errgroup.Group
	gCtx     context.Context
	cancel   context.CancelFunc
	restarts *prometheus.CounterVec
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

// Defaults for the restart pacing that ErrGroup.GoWithRetries and
// joblock.Options configure. Every one of them is exported so that a call
// site can name the default instead of restating the number.
const (
	// DefaultBackoffFloor is the delay before the first restart after a
	// failure, when RetryOptions.BackoffFloor is left at zero.
	DefaultBackoffFloor = pacer.DefaultBackoffFloor
	// DefaultBackoffCeil is the longest the backoff grows to, when
	// RetryOptions.BackoffCeil is left at zero.
	DefaultBackoffCeil = pacer.DefaultBackoffCeil
	// DefaultMinRuntime is the minimum interval between two starts of a
	// task, when RetryOptions.MinRuntime is left at zero.
	DefaultMinRuntime = pacer.DefaultMinRuntime
	// DefaultHealthyRuntime is how long a run must last to count as a
	// success, when RetryOptions.HealthyRuntime is left at zero.
	DefaultHealthyRuntime = pacer.DefaultHealthyRuntime
)

// RetryOptions controls how ErrGroup.GoWithRetries paces the restarts of a
// failing task, and when it stops restarting it. The zero value restarts
// forever with the default curve.
//
// Restarts are paced with exponential backoff from BackoffFloor to
// BackoffCeil, with equal jitter, and every restart is padded out to
// MinRuntime so that a task that fails immediately can't spin. The curve is
// the pacer's own; there is nothing for a caller to supply.
type RetryOptions struct {
	// GiveUpAfter is how much time the task may spend failing before it
	// gives up and fails the group: the failing runs and the waits between
	// them, starting at the first failure of the streak and cleared by a
	// run that reaches HealthyRuntime. Time spent in a run that failed for
	// neither reason — a nil return too short to be healthy — is not
	// counted. Zero, the default, means that it restarts forever.
	// Replaces the maxRetries argument, which counted failures instead of
	// timing them.
	//
	// It must be longer than HealthyRuntime, and wants to be several times
	// it: nothing shorter than a healthy run clears the budget, so a
	// budget of about one healthy run means giving up on the second
	// failure however far apart the two are. A shorter one fails the
	// group, and fails joblock.Run, rather than being quietly accepted.
	GiveUpAfter time.Duration
	// HealthyRuntime is how long a run must last to count as a success
	// and clear the failure budget — how long it has to keep running
	// before you would call the task working again. Defaults to
	// DefaultHealthyRuntime. Replaces the resetAfter argument.
	HealthyRuntime time.Duration
	// BackoffFloor is the delay before the first restart after a failure.
	// Defaults to DefaultBackoffFloor. Setting it below MinRuntime does
	// nothing, since the restart is padded out to MinRuntime either way —
	// a faster first retry means lowering both. Replaces the backoff
	// argument, along with BackoffCeil.
	BackoffFloor time.Duration
	// BackoffCeil is the longest the backoff grows to. Defaults to
	// DefaultBackoffCeil.
	BackoffCeil time.Duration
	// MinRuntime is the minimum interval between two starts of the task.
	// A run that returns faster is padded out to it, and a run that
	// reaches it resets the backoff. Defaults to DefaultMinRuntime, which
	// dominates the first restarts of a task that fails immediately: the
	// backoff only becomes visible once it has doubled past this.
	MinRuntime time.Duration
}

// GoWithRetries runs a task in a retry loop, restarting it after a failure
// with exponential backoff.
//
// The task gives up, failing the group, once it has spent
// RetryOptions.GiveUpAfter failing. That budget is the failing runs and the
// waits between them, starting at the first failure of the streak, and only a
// run that lasts RetryOptions.HealthyRuntime clears it. A task that returns
// nil is done: the loop returns without restarting it.
//
// A GiveUpAfter that is not longer than HealthyRuntime fails the group
// immediately, since only a run of that length clears the budget — see
// RetryOptions.GiveUpAfter.
func (eg *ErrGroup) GoWithRetries(
	task string,
	opts RetryOptions,
	fn func(ctx context.Context) error,
) {
	eg.grp.Go(func() error {
		// Initialise the restart series at zero so that increases can
		// be detected even for a task's first restart.
		restarts := eg.restarts.WithLabelValues(task)

		restartPacer, err := pacer.New(pacer.Options{
			GiveUpAfter:    opts.GiveUpAfter,
			HealthyRuntime: opts.HealthyRuntime,
			BackoffFloor:   opts.BackoffFloor,
			BackoffCeil:    opts.BackoffCeil,
			MinRuntime:     opts.MinRuntime,
		})
		if err != nil {
			return fmt.Errorf("%s: invalid retry options: %w", task, err)
		}

		for {
			started := time.Now()

			err := CallWithRecover(eg.gCtx, fn)
			if err == nil {
				return nil
			}

			// Bail immediately if the group has been cancelled.
			if eg.gCtx.Err() != nil {
				return fmt.Errorf("%s: %w", task, eg.gCtx.Err())
			}

			wait, giveUp := restartPacer.Pace(time.Since(started), err)
			if giveUp != nil {
				return fmt.Errorf("%s: %w", task, giveUp)
			}

			restarts.Inc()

			eg.logger.ErrorContext(eg.gCtx,
				"task failure, restarting",
				LogKeyName, task,
				LogKeyError, err,
				LogKeyAttempts, restartPacer.Failures(),
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

	err := eg.grp.Wait()
	if err != nil {
		return fmt.Errorf("run task group: %w", err)
	}

	return nil
}

// ErrPanicRecovered is the error a task fails with when CallWithRecover
// recovers a panic. PanicValue is the value that was passed to panic().
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
