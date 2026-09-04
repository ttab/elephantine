// Package joblock coordinates which instance of a service runs a background
// task, using a row in the job_lock table as the lock.
//
// The table is created by the tern migration in schema/, which a consuming
// service vendors into its own ./schema with
// `mage sql:vendorAdd github.com/ttab/elephantine pg/joblock/schema`. Nothing
// here applies it: a service migrates its own database, never a library.
package joblock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/ttab/elephantine/pg/joblock/internal/postgres"
)

// jobLockNameLabel is the metric label holding the job lock name.
const jobLockNameLabel = "name"

// State describes whether a job lock is held, lost, or released. It is
// sent on the channel a Lock uses to notify its holder of state changes.
type State string

const (
	StateNone     = ""
	StateHeld     = "held"
	StateLost     = "lost"
	StateReleased = "released"
)

// Options controls how a job lock should behave.
type Options struct {
	// PingInterval controls how often the job locked should be
	// pinged/renewed. Defaults to 10s.
	PingInterval time.Duration
	// StaleAfter controls after how long a time a held lock should be
	// considered stale and other clients will start attempting to steal
	// it. Must be longer than the ping interval. Defaults to four times the
	// ping interval.
	StaleAfter time.Duration
	// CheckInterval controls how often clients should check if a held lock
	// has become stale. Defaults to twice the ping interval.
	CheckInterval time.Duration
	// Timeout is the timeout that should be used for all lock
	// operations. Must be shorter than the ping interval. Defaults to half
	// the ping interval.
	Timeout time.Duration
	// MetricsRegisterer is used to register the job lock metrics.
	// Defaults to prometheus.DefaultRegisterer.
	MetricsRegisterer prometheus.Registerer
	// MaxConsecutiveFailures is the number of consecutive failed runs
	// Run tolerates before it gives up and returns an error
	// instead of restarting the function. Zero, the default, means that
	// it keeps restarting forever. Only used by Run.
	MaxConsecutiveFailures int
	// HealthyRuntime is how long a run must last to count as a success
	// for the purposes of MaxConsecutiveFailures. A run that returns
	// earlier than this counts as a failure regardless of how much work
	// it did, so that failures that accrue slowly still reach the limit.
	// Defaults to five minutes. Only used by Run.
	HealthyRuntime time.Duration
}

// Lock helps separate processes coordinate who should be performing a
// (background) task through postgres.
type Lock struct {
	logger        *slog.Logger
	db            *pgxpool.Pool
	state         State
	lastPing      time.Time
	lastAttempt   time.Time
	out           chan State
	abort         chan struct{}
	cleanedUp     chan struct{}
	name          string
	identity      string
	iteration     int64
	pingInterval  time.Duration
	staleAfter    time.Duration
	checkInterval time.Duration
	timeout       time.Duration
	held          prometheus.Gauge
	transitions   *prometheus.CounterVec

	once sync.Once
}

// New creates a new job lock.
func New(
	db *pgxpool.Pool, logger *slog.Logger, name string,
	opts Options,
) (*Lock, error) {
	if opts.PingInterval == 0 {
		opts.PingInterval = 10 * time.Second
	}

	if opts.StaleAfter == 0 {
		opts.StaleAfter = opts.PingInterval * 4
	}

	if opts.CheckInterval == 0 {
		opts.CheckInterval = opts.PingInterval * 2
	}

	if opts.Timeout == 0 {
		opts.Timeout = opts.PingInterval / 2
	}

	if opts.PingInterval >= opts.StaleAfter {
		return nil, fmt.Errorf(
			"the ping interval must be shorter than stale after, stale after: %s, ping interval %s",
			opts.StaleAfter, opts.PingInterval)
	}

	if opts.Timeout >= opts.PingInterval {
		return nil, fmt.Errorf(
			"the timeout must be shorter than the ping interval, timeout: %s, ping interval %s",
			opts.Timeout, opts.PingInterval)
	}

	if opts.MetricsRegisterer == nil {
		opts.MetricsRegisterer = prometheus.DefaultRegisterer
	}

	heldVec, err := elephantine.RegisterOrReuse(opts.MetricsRegisterer,
		prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pg_job_lock_held",
			Help: "Whether this instance currently holds the " +
				"named job lock.",
		}, []string{jobLockNameLabel}))
	if err != nil {
		return nil, fmt.Errorf(
			"register job lock held metric: %w", err)
	}

	transitionsVec, err := elephantine.RegisterOrReuse(
		opts.MetricsRegisterer,
		prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pg_job_lock_transitions_total",
			Help: "Job lock state transitions observed by this " +
				"instance.",
		}, []string{jobLockNameLabel, "state"}))
	if err != nil {
		return nil, fmt.Errorf(
			"register job lock transitions metric: %w", err)
	}

	transitions, err := transitionsVec.CurryWith(prometheus.Labels{
		jobLockNameLabel: name,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"curry job lock transitions metric: %w", err)
	}

	id := uuid.New()

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to get hostname: %w", err)
	}

	identity := fmt.Sprintf("%s.%s", id, hostname)

	logger = logger.With(
		elephantine.LogKeyJobLock, name,
		elephantine.LogKeyJobLockID, identity)

	jl := Lock{
		logger:        logger,
		db:            db,
		name:          name,
		identity:      identity,
		pingInterval:  opts.PingInterval,
		staleAfter:    opts.StaleAfter,
		checkInterval: opts.CheckInterval,
		timeout:       opts.Timeout,
		out:           make(chan State, 1),
		abort:         make(chan struct{}),
		cleanedUp:     make(chan struct{}),
		held:          heldVec.WithLabelValues(name),
		transitions:   transitions,
	}

	jl.held.Set(0)

	return &jl, nil
}

// observeState updates the job lock metrics on a state transition.
func (jl *Lock) observeState(state State) {
	if state == StateHeld {
		jl.held.Set(1)
	} else {
		jl.held.Set(0)
	}

	jl.transitions.WithLabelValues(string(state)).Inc()
}

func (jl *Lock) Identity() string {
	return jl.identity
}

// Stop releases the job lock if held and stops all polling.
func (jl *Lock) Stop() {
	close(jl.abort)

	select {
	case <-jl.cleanedUp:
	case <-time.After(jl.timeout):
	}
}

func (jl *Lock) run() {
	jl.once.Do(jl.loop)
}

// RunWithContext runs the provided function once the job lock has been
// acquired. The context provided to the function will be cancelled if the job
// lock is lost.
//
// The function is run at most once: when it returns the lock is released,
// and the Lock cannot be reused. Use Run for supervised
// restarts.
func (jl *Lock) RunWithContext(
	ctx context.Context,
	fn func(ctx context.Context) error,
) error {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	acquiredLock := make(chan struct{})

	go func() {
		go jl.run()

		defer jl.Stop()
		defer cancel()

		for {
			select {
			case <-jl.abort:
				return
			case state := <-jl.out:
				switch state {
				case StateNone:
				case StateLost, StateReleased:
					return
				case StateHeld:
					close(acquiredLock)
				}
			case <-waitCtx.Done():
				return
			}
		}
	}()

	select {
	case <-acquiredLock:
		return fn(waitCtx)
	case <-waitCtx.Done():
		return nil
	}
}

func (jl *Lock) loop() {
	var nextState State

	defer close(jl.out)

	// Always attempt to release before returning.
	defer jl.release()

	for {
		switch jl.state {
		case StateNone:
			change := jl.attemptAcquire()

			if change.Ok {
				nextState = StateHeld

				jl.lastPing = change.Ping
				jl.iteration = change.Iteration
			}
		case StateHeld:
			if time.Since(jl.lastPing) > jl.pingInterval {
				nextState = jl.ping()
			}
		case StateReleased:
			return
		}

		if nextState != jl.state {
			jl.state = nextState

			jl.observeState(jl.state)

			jl.logger.Debug("job lock state change",
				elephantine.LogKeyState, jl.state)

			// Notify the lock holder of the change. If the lock
			// holder doesn't consume the message we will bail and
			// release the lock.
			select {
			case jl.out <- jl.state:
			default:
				jl.logger.Error("state change channel buffer is full, aborting")

				return
			}
		}

		var wait <-chan time.Time

		switch jl.state {
		case StateLost:
			return
		case StateHeld:
			wait = time.After(jl.nextPingWait())
		default:
			wait = time.After(jl.checkInterval)
		}

		select {
		case <-jl.abort:
			return
		case <-wait:
		}
	}
}

type acquireChange struct {
	Ok        bool
	Ping      time.Time
	Iteration int64
}

func (jl *Lock) attemptAcquire() acquireChange {
	ctx, cancel := context.WithTimeout(context.Background(), jl.timeout)
	defer cancel()

	var change acquireChange

	err := pg.WithTX(ctx, jl.db, func(tx pgx.Tx) error {
		c, err := jl.acquire(ctx, postgres.New(tx))
		if err != nil {
			return err
		}

		change = c

		return nil
	})
	if err != nil {
		jl.logger.Error("failed to acquire job lock",
			elephantine.LogKeyError, err.Error())

		return acquireChange{}
	}

	return change
}

func (jl *Lock) acquire(ctx context.Context, q *postgres.Queries) (acquireChange, error) {
	state, err := q.GetJobLock(ctx, jl.name)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return acquireChange{}, fmt.Errorf("failed to read job lock: %w", err)
	}

	isHeld := !errors.Is(err, pgx.ErrNoRows)

	if isHeld && time.Since(state.Touched.Time) < jl.staleAfter {
		return acquireChange{}, nil
	}

	if isHeld {
		return jl.steal(ctx, q, state)
	}

	iteration, err := q.InsertJobLock(ctx, postgres.InsertJobLockParams{
		Name:   jl.name,
		Holder: jl.identity,
	})
	if pg.IsConstraintError(err, "job_lock_pkey") {
		return acquireChange{}, nil
	} else if err != nil {
		return acquireChange{}, fmt.Errorf("failed to insert job lock: %w", err)
	}

	return acquireChange{
		Ok:        true,
		Ping:      time.Now(),
		Iteration: iteration,
	}, nil
}

func (jl *Lock) steal(
	ctx context.Context, q *postgres.Queries, state postgres.GetJobLockRow,
) (acquireChange, error) {
	jl.logger.Debug("attempt to steal job lock")

	affected, err := q.StealJobLock(ctx, postgres.StealJobLockParams{
		Name:           jl.name,
		NewHolder:      jl.identity,
		PreviousHolder: state.Holder,
		Iteration:      state.Iteration,
	})
	if err != nil {
		return acquireChange{}, fmt.Errorf("failed to steal job lock: %w", err)
	}

	if affected == 0 {
		return acquireChange{}, fmt.Errorf("out of sync: failed to steal job lock")
	}

	return acquireChange{
		Ok:        true,
		Ping:      time.Now(),
		Iteration: state.Iteration + 1,
	}, nil
}

func (jl *Lock) release() {
	defer close(jl.cleanedUp)

	if jl.state != StateHeld {
		return
	}

	// We stop holding the lock regardless of whether the release
	// call succeeds.
	jl.observeState(StateReleased)

	jl.logger.Debug("releasing job lock")

	ctx, cancel := context.WithTimeout(context.Background(), jl.timeout)
	defer cancel()

	updated, err := postgres.New(jl.db).ReleaseJobLock(ctx,
		postgres.ReleaseJobLockParams{
			Name:   jl.name,
			Holder: jl.identity,
		})

	switch {
	case err != nil:
		jl.logger.Error("failed to release job lock",
			elephantine.LogKeyError, err.Error())
	case updated == 0:
		jl.logger.Error("out of sync: no matching job lock to release")
	}

	select {
	case jl.out <- StateReleased:
	default:
	}
}

// nextPingWait returns how long to wait before the next ping attempt. Pings
// are paced from the last attempt, not the last success: lastPing only
// advances on successful pings, since staleness is measured from it, so a
// wait computed from lastPing alone goes negative as soon as a ping fails
// and turns the retries into a busy-loop for the rest of the stale window.
func (jl *Lock) nextPingWait() time.Duration {
	since := jl.lastPing

	if jl.lastAttempt.After(since) {
		since = jl.lastAttempt
	}

	return time.Until(since.Add(jl.pingInterval))
}

func (jl *Lock) ping() State {
	jl.lastAttempt = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), jl.timeout)
	defer cancel()

	updated, err := postgres.New(jl.db).PingJobLock(ctx,
		postgres.PingJobLockParams{
			Name:      jl.name,
			Holder:    jl.identity,
			Iteration: jl.iteration,
		})

	switch {
	case err != nil:
		jl.logger.Error("failed to ping job lock",
			elephantine.LogKeyError, err.Error())

		if time.Since(jl.lastPing) > jl.staleAfter {
			return StateLost
		}

		return StateHeld

	case updated == 0:
		jl.logger.Error("out of sync: no matching job lock to ping")

		return StateLost
	}

	jl.iteration++
	jl.lastPing = time.Now()

	return StateHeld
}
