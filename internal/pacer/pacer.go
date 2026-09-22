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
	// GiveUpAfter is how long a failure streak is allowed to last before
	// Pace gives up. Zero means that it never does.
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
	// count as neither a failure nor a success. joblock.Run sets it: there
	// a cancellation means the lock was lost, not that the job failed.
	IgnoreCancellation bool
}

// New creates a pacer, applying the default for every duration left at zero.
func New(opts Options) *Pacer {
	if opts.HealthyRuntime == 0 {
		opts.HealthyRuntime = DefaultHealthyRuntime
	}

	if opts.BackoffFloor == 0 {
		opts.BackoffFloor = DefaultBackoffFloor
	}

	if opts.BackoffCeil == 0 {
		opts.BackoffCeil = DefaultBackoffCeil
	}

	if opts.MinRuntime == 0 {
		opts.MinRuntime = DefaultMinRuntime
	}

	opts.BackoffCeil = max(opts.BackoffCeil, opts.BackoffFloor)

	return &Pacer{opts: opts, now: time.Now}
}

// Pacer decides how long to wait before restarting a supervised function, and
// when to stop restarting it altogether.
type Pacer struct {
	opts Options
	// now reads the clock the failure budget is measured against.
	// Replaced by the tests so that a budget can be exhausted without
	// waiting for it.
	now func() time.Time

	backoff      time.Duration
	failingSince time.Time
	failures     int
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
// When the current failure streak has lasted longer than GiveUpAfter, Pace
// returns an error wrapping the last one instead, and the function should not
// be restarted. The streak starts at its first failure and is only cleared by
// a run that reached HealthyRuntime, so failures accrue towards the budget
// however slowly they arrive, and the waits between them count towards it —
// the question the budget answers is how long the task has been broken, not
// how much of that time it spent executing. The backoff, in contrast, resets
// after any run that reached MinRuntime: it is there to pace a task that is
// failing right now, not to judge its health.
func (p *Pacer) Pace(runtime time.Duration, err error) (time.Duration, error) {
	if p.backoff == 0 || runtime >= p.opts.MinRuntime {
		p.backoff = p.opts.BackoffFloor
	}

	failed := err != nil &&
		(!p.opts.IgnoreCancellation || !isCancellation(err))

	switch {
	case runtime >= p.opts.HealthyRuntime:
		// A run that lasted long enough to be considered healthy
		// clears the failure history, even if it ended in an error.
		p.failingSince = time.Time{}
		p.failures = 0
	case failed:
		if p.failingSince.IsZero() {
			p.failingSince = p.now()
		}

		p.failures++
	}

	// A healthy run clears the streak even when it ended in an error, so
	// the budget is only live if this failure left one open.
	if failed && p.opts.GiveUpAfter > 0 && !p.failingSince.IsZero() {
		failingFor := p.now().Sub(p.failingSince)

		if failingFor >= p.opts.GiveUpAfter {
			return 0, fmt.Errorf(
				"stopping after %v of consecutive failures (%d runs), none of them lasting %v: %w",
				failingFor.Round(time.Second), p.failures,
				p.opts.HealthyRuntime, err)
		}
	}

	var delay time.Duration

	if err != nil {
		delay = p.jitter()

		p.backoff = min(2*p.backoff, p.opts.BackoffCeil)
	}

	return max(delay, p.opts.MinRuntime-runtime), nil
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
