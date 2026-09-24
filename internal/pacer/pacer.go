// Package pacer holds the restart pacer shared by elephantine.ErrGroup's
// retry loop and joblock.Run. It lives here rather than in the root package
// so that joblock can use it without it becoming public API: the curve is the
// pacer's own behaviour, not something a caller supplies.
package pacer

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Defaults for the options below. They are re-exported by the root package
// and documented there.
const (
	DefaultBackoffFloor   = 1 * time.Second
	DefaultBackoffCeil    = 60 * time.Second
	DefaultMinRuntime     = 10 * time.Second
	DefaultHealthyRuntime = 5 * time.Minute
)

// Options configures a Pacer. Every duration defaults to the matching
// Default constant, except GiveUpAfter where zero means "never give up".
type Options struct {
	// GiveUpAfter is how much time the supervised function may spend
	// failing before Pace gives up. Zero means that it never does. It must
	// be longer than HealthyRuntime; see Validate.
	GiveUpAfter time.Duration
	// HealthyRuntime is the runtime a run must reach to count as a
	// success, clearing the failure streak.
	HealthyRuntime time.Duration
	// BackoffFloor is the delay before the first restart after a failure.
	BackoffFloor time.Duration
	// BackoffCeil is the longest the backoff grows to.
	BackoffCeil time.Duration
	// MinRuntime is the minimum interval between two starts. A run that
	// returns faster is padded out to it, and a run that reaches it
	// resets the backoff.
	MinRuntime time.Duration
	// IgnoreCancellation makes a run that ended in a context cancellation
	// count as neither a failure nor a success, and keeps the time it
	// lasted out of the failure budget. joblock.Run sets it: there a
	// cancellation means the lock was lost, not that the job failed.
	IgnoreCancellation bool
}

// withDefaults substitutes the default for every duration that isn't a usable
// value. A negative one is a mistake rather than a request, and taking it
// literally would silently disable the budget: every run is at least zero long,
// so a negative HealthyRuntime makes every run healthy.
func (o Options) withDefaults() Options {
	if o.HealthyRuntime <= 0 {
		o.HealthyRuntime = DefaultHealthyRuntime
	}

	if o.BackoffFloor <= 0 {
		o.BackoffFloor = DefaultBackoffFloor
	}

	if o.BackoffCeil <= 0 {
		o.BackoffCeil = DefaultBackoffCeil
	}

	if o.MinRuntime <= 0 {
		o.MinRuntime = DefaultMinRuntime
	}

	if o.GiveUpAfter < 0 {
		o.GiveUpAfter = 0
	}

	o.BackoffCeil = max(o.BackoffCeil, o.BackoffFloor)

	return o
}

// Validate rejects a budget that cannot mean what it reads. Only a run that
// reaches HealthyRuntime clears a failure streak, so a budget shorter than
// that degenerates into "give up on the second failure more than GiveUpAfter
// apart", however healthy the runs in between were.
func (o Options) Validate() error {
	o = o.withDefaults()

	if o.GiveUpAfter == 0 {
		return nil
	}

	if o.GiveUpAfter <= o.HealthyRuntime {
		return fmt.Errorf(
			"the failure budget must be longer than the healthy runtime that clears it, give up after: %s, healthy runtime: %s",
			o.GiveUpAfter, o.HealthyRuntime)
	}

	return nil
}

// New creates a pacer, applying the default for every duration left at zero.
func New(opts Options) (*Pacer, error) {
	err := opts.Validate()
	if err != nil {
		return nil, err
	}

	return &Pacer{opts: opts.withDefaults()}, nil
}

// Pacer decides how long to wait before restarting a supervised function, and
// when to stop restarting it altogether.
type Pacer struct {
	opts Options

	backoff time.Duration
	// lastWait is the delay returned by the previous Pace call, which has
	// elapsed by the time the next one arrives.
	lastWait time.Duration
	// failing reports whether a failure streak is open, and failingFor is
	// the time it has run for so far.
	failing    bool
	failingFor time.Duration
	failures   int
}

// Failures is the number of runs in the current failure streak, for logging.
func (p *Pacer) Failures() int {
	return p.failures
}

// Pace returns the delay before the next restart given how long the previous
// run lasted and the error it returned. Errors are subject to exponential
// backoff with jitter, and all returns are padded so that runs start at most
// once per Options.MinRuntime.
//
// When the supervised function has spent GiveUpAfter failing, Pace returns an
// error wrapping the last one instead, and the function should not be
// restarted. The budget is the time the task has been broken: the failing runs
// and the waits between them, starting at the first failure of the streak, and
// cleared only by a run that reached HealthyRuntime. A run that is neither —
// a nil return too short to be healthy, or, with IgnoreCancellation, one ended
// by the loss of a lock — leaves the streak open but spends nothing, since the
// task was not failing while it ran. The backoff, in contrast, resets after any
// run that reached MinRuntime: it is there to pace a task that is failing right
// now, not to judge its health.
func (p *Pacer) Pace(runtime time.Duration, err error) (time.Duration, error) {
	if p.backoff == 0 || runtime >= p.opts.MinRuntime {
		p.backoff = p.opts.BackoffFloor
	}

	failed := err != nil &&
		(!p.opts.IgnoreCancellation || !isCancellation(err))

	// A run that lasted long enough to be considered healthy clears the
	// streak, even if it ended in an error. If it did end in one, that
	// error opens a new streak below rather than being forgiven with the
	// history it just cleared.
	if runtime >= p.opts.HealthyRuntime {
		p.failing = false
		p.failingFor = 0
		p.failures = 0
	}

	if failed {
		if p.failing {
			// The wait before this run and the run itself were
			// spent failing.
			p.failingFor += p.lastWait + runtime
		}

		p.failing = true
		p.failures++
	}

	if failed && p.opts.GiveUpAfter > 0 && p.failingFor >= p.opts.GiveUpAfter {
		return 0, fmt.Errorf(
			"stopping after %v spent failing (%d runs), none of them lasting %v: %w",
			p.failingFor.Round(time.Second), p.failures,
			p.opts.HealthyRuntime, err)
	}

	var delay time.Duration

	if err != nil {
		delay = p.jitter()

		p.backoff = min(2*p.backoff, p.opts.BackoffCeil)
	}

	wait := max(delay, p.opts.MinRuntime-runtime)

	p.lastWait = wait

	return wait, nil
}

// jitter returns the current backoff with equal jitter applied: a delay in
// [backoff/2, backoff).
func (p *Pacer) jitter() time.Duration {
	half := p.backoff / 2
	if half <= 0 {
		return p.backoff
	}

	//nolint:gosec // G404: jitter is not security sensitive.
	return half + rand.N(half)
}

// isCancellation reports whether the error is a context cancellation.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}
