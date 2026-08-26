# Job lock restart semantics

`pg.RunInJobLock` runs a function on one instance at a time, supervising it
for the lifetime of the process. It is the entry point for a background
worker that must not run concurrently in a multi-replica deployment: an
eventlog follower, an indexer, a pruner, a scheduler.

```go
err := pg.RunInJobLock(ctx, db, logger,
	"indexer",  // service name, used in log messages
	"indexer",  // lock name, shared by every instance and used as a metric label
	pg.JobLockOptions{
		MetricsRegisterer:      reg,
		MaxConsecutiveFailures: 10,
	},
	func(ctx context.Context) error {
		// Blocks until ctx is cancelled.
	})
```

## The contract

The function is expected to **block until its context is cancelled**. The
context it receives is tied to the held lock, so it is cancelled when the
lock is lost, and returning — with or without an error — releases the lock.
`RunInJobLock` then re-acquires the lock and starts the function again, until
the outer context is cancelled or the failure limit is reached.

This is not a way to run something exactly once. A function that finishes its
work and returns nil is restarted, the same as one that fails; there is no
"success" return.

Under the hood `JobLock.RunWithContext` is one-shot — it acquires the lock,
runs the function once, and releases the lock on return — and `RunInJobLock`
is the loop around it that constructs a fresh lock per iteration. Since an
instance that returns has just deleted its own `job_lock` row, re-acquisition
would otherwise be instant, which is why the loop paces itself.

## Restart pacing

Every return is paced, and the lock stays released while waiting so another
instance can take over — possibly one whose network path to the failing
dependency works.

- **Minimum runtime.** A run that returns within 10 seconds of starting is
  padded out to it before the lock is re-acquired. This bounds `job_lock` row
  churn and log volume for a function that returns immediately, whether it
  errors or not.
- **Backoff on errors.** An error return is retried with exponential backoff
  from 1 second to a 60 second ceiling, with equal jitter (the delay is
  uniform in `[backoff/2, backoff)`). The backoff resets after any run that
  reached the 10 second minimum runtime: it exists to pace a job that is
  failing right now, not to judge whether the job is healthy.

## The failure limit

Pacing makes a persistently failing job survivable, not healthy. A job paced
at the ceiling can keep failing indefinitely with only a metric to show it, so
the loop can be bounded:

- `JobLockOptions.MaxConsecutiveFailures` — how many consecutive failures to
  tolerate. When the limit is reached `RunInJobLock` returns an error wrapping
  the last failure instead of restarting. Zero, the default, restarts forever.
- `JobLockOptions.HealthyRuntime` — how long a run must last to count as a
  success, five minutes by default.

The definition of a success is deliberately not "returned nil" but "ran for at
least `HealthyRuntime`". A function that blocks as the contract asks reaches
that within its first run; a function that fails fast never does, so its
failures accrue towards the limit however slowly they arrive instead of being
forgiven one at a time. A run that lasted at least `HealthyRuntime` clears the
count even if it ended in an error, and a run shorter than that clears nothing
even if it returned nil.

Two details keep the count honest:

- **The runtime is measured inside the lock**, not around the acquisition, so
  the time a follower spends waiting for its turn is not credited as healthy
  runtime. A replica that polls for twenty minutes and then fails instantly
  would otherwise look healthy on every cycle.
- **Losing the lock is not a failure.** Lock loss cancels the function's
  context, so it surfaces as `context.Canceled`, which is not counted. A lock
  that ping-pongs between replicas therefore cannot exhaust the limit and take
  the service down.

The returned error is the caller's to handle, which in practice means the
`elephantine.ErrGroup` the worker runs under: a `Go` task restarts with the
group's own backoff and counts in `task_restarts_total`, and a `Required` task
brings the service down to be restarted and noticed.

## Panics

The function is run through `elephantine.CallWithRecover`, so a panic becomes
an `ErrPanicRecovered` error return: logged, counted, paced and subject to the
failure limit like any other failure, rather than taking the process down and
leaving the lock row to go stale until it is stolen. A worker does not need its
own panic containment to avoid crashing the service.

## Observability

- `pg_job_lock_held{name}` — whether this instance holds the lock.
- `pg_job_lock_transitions_total{name,state}` — lock state changes. A
  sustained rate is lock churn, which also catches a lock ping-ponging between
  replicas.
- `pg_job_lock_restarts_total{name}` — restarts after an error return, the
  direct signal that a job is failing. Alert on a sustained non-zero
  `rate(pg_job_lock_restarts_total[5m])`; see `docs/metrics.md`.

## Notes for call sites

- **Prefer `RunInJobLock` to `JobLock.RunWithContext`.** The one-shot API gives
  the caller no way to distinguish lock loss from a shutdown — both arrive as a
  cancelled context — so a task that treats cancellation as a graceful stop
  exits silently on lock loss and leaves the service running without that
  subsystem.
- **Retry in place for transient failures.** Returning an error costs a lock
  release and re-acquisition, so a worker that can retry a failed batch
  without giving up the lock should still do so. The error return is for
  failures the worker cannot handle itself, and it is now a safe path rather
  than one that spins.
- **Set `MaxConsecutiveFailures`** for any job whose continued failure should
  page someone rather than accumulate quietly.

## Pointers

- `pg/joblock_service.go` — `RunInJobLock` and `restartPacer`.
- `pg/joblock.go` — `JobLockOptions`, `RunWithContext`, the lock loop, ping
  and stale-lock stealing.
- `docs/metrics.md` — job lock alerting.
