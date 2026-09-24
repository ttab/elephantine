# Job lock restart semantics

`joblock.Run` runs a function on one instance at a time, supervising it
for the lifetime of the process. It is the entry point for a background
worker that must not run concurrently in a multi-replica deployment: an
eventlog follower, an indexer, a pruner, a scheduler.

```go
err := joblock.Run(ctx, db, logger,
	"indexer",  // service name, used in log messages
	"indexer",  // lock name, shared by every instance and used as a metric label
	joblock.Options{
		MetricsRegisterer: reg,
		GiveUpAfter:       30 * time.Minute,
	},
	func(ctx context.Context) error {
		// Blocks until ctx is cancelled.
	})
```

## The contract

The function is expected to **block until its context is cancelled**. The
context it receives is tied to the held lock, so it is cancelled when the
lock is lost, and returning — with or without an error — releases the lock.
`joblock.Run` then re-acquires the lock and starts the function again, until
the outer context is cancelled or the failure budget runs out.

This is not a way to run something exactly once. A function that finishes its
work and returns nil is restarted, the same as one that fails; there is no
"success" return.

Under the hood `joblock.Lock.RunWithContext` is one-shot — it acquires the lock,
runs the function once, and releases the lock on return — and `joblock.Run`
is the loop around it that constructs a fresh lock per iteration. Since an
instance that returns has just deleted its own `job_lock` row, re-acquisition
would otherwise be instant, which is why the loop paces itself.

## Restart pacing

Every return is paced, and the lock stays released while waiting so another
instance can take over — possibly one whose network path to the failing
dependency works.

- **Minimum runtime.** A run that returns within `Options.MinRuntime` of
  starting — 10 seconds by default — is padded out to it before the lock is
  re-acquired. This bounds `job_lock` row churn and log volume for a function
  that returns immediately, whether it errors or not.
- **Backoff on errors.** An error return is retried with exponential backoff
  from `Options.BackoffFloor` to `Options.BackoffCeil`, 1 second to a 60
  second ceiling by default, with equal jitter (the delay is uniform in
  `[backoff/2, backoff)`). The backoff resets after any run that reached the
  minimum runtime: it exists to pace a job that is failing right now, not to
  judge whether the job is healthy.

Since the restart is padded out to `MinRuntime` either way, the backoff is
invisible until it has doubled past it, around the fifth or sixth consecutive
fast failure. **Lowering `BackoffFloor` on its own therefore does nothing**; a
job that should be retried faster than that needs `MinRuntime` lowered with
it.

The same pacer runs behind `elephantine.ErrGroup.GoWithRetries`, configured
with the same five fields under the name `elephantine.RetryOptions`.

## The failure budget

Pacing makes a persistently failing job survivable, not healthy. A job paced
at the ceiling can keep failing indefinitely with only a metric to show it, so
the loop can be bounded:

- `joblock.Options.GiveUpAfter` — how much time the function may spend
  failing. When the budget runs out `joblock.Run` returns an error wrapping
  the last failure instead of restarting. Zero, the default, restarts forever.
- `joblock.Options.HealthyRuntime` — how long a run must last to count as a
  success, five minutes by default.

**The budget is the time the job has spent broken**: the failing runs and the
waits between them, starting at the first failure of the streak. It is a
duration rather than a count of failures because the count could not be read
without doing the exponential sum backwards at the call site. Time the job
spent doing something other than failing is not spent from it — a run that
returned nil too early to be healthy, or one ended by the loss of the lock,
leaves the streak open but costs it nothing.

**`GiveUpAfter` must be longer than `HealthyRuntime`, and wants to be several
times it.** Nothing shorter than a healthy run clears the budget, so a budget
of about one healthy run degenerates into "give up on the second failure more
than `GiveUpAfter` apart", however well the job ran in between. `joblock.Run`
returns an error for a shorter one rather than accepting a limit that cannot
mean what it reads. Pick `HealthyRuntime` first — how long the job has to keep
running before you would call it working — and then a budget that is a
handful of those.

The definition of a success is deliberately not "returned nil" but "ran for at
least `HealthyRuntime`". A function that blocks as the contract asks reaches
that within its first run; a function that fails fast never does, so the
budget runs out however slowly its failures arrive instead of them being
forgiven one at a time. A run that lasted at least `HealthyRuntime` clears the
streak even if it ended in an error, and a run shorter than that clears
nothing even if it returned nil.

Two details keep the budget honest:

- **The runtime is measured inside the lock**, not around the acquisition, so
  the time a follower spends waiting for its turn is not credited as healthy
  runtime. A replica that polls for twenty minutes and then fails instantly
  would otherwise look healthy on every cycle.
- **Losing the lock is not a failure, and does not spend the budget.** Lock
  loss cancels the function's context, so it surfaces as `context.Canceled`,
  which is not counted — and neither is the time that run held the lock for,
  since the job was not failing while it ran. A lock that ping-pongs between
  replicas therefore cannot exhaust the budget and take the service down,
  which it could if the budget were wall clock: the length of a lock hold is
  not the caller's to choose, so no `GiveUpAfter` would be safe.

The returned error is the caller's to handle, which in practice means the
`elephantine.ErrGroup` the worker runs under: a `Go` task restarts with the
group's own backoff and counts in `task_restarts_total`, and a `Required` task
brings the service down to be restarted and noticed.

## Panics

The function is run through `elephantine.CallWithRecover`, so a panic becomes
an `ErrPanicRecovered` error return: logged, counted, paced and subject to the
failure budget like any other failure, rather than taking the process down and
leaving the lock row to go stale until it is stolen. A worker does not need its
own panic containment to avoid crashing the service.

## Observability

- `pg_job_lock_held{name}` — whether this instance holds the lock.
- `pg_job_lock_transitions_total{name,state}` — lock state changes. A
  sustained rate is lock churn, which also catches a lock ping-ponging between
  replicas.
- `pg_job_lock_restarts_total{name}` — restarts after an error return, the
  direct signal that a job is failing. Alert on a sustained non-zero
  `rate(pg_job_lock_restarts_total[5m])`; see
  [job lock alerting](metrics.md#job-lock-alerting).

## Notes for call sites

- **Prefer `joblock.Run` to `joblock.Lock.RunWithContext`.** The one-shot API gives
  the caller no way to distinguish lock loss from a shutdown — both arrive as a
  cancelled context — so a task that treats cancellation as a graceful stop
  exits silently on lock loss and leaves the service running without that
  subsystem.
- **Retry in place for transient failures.** Returning an error costs a lock
  release and re-acquisition, so a worker that can retry a failed batch
  without giving up the lock should still do so. The error return is for
  failures the worker cannot handle itself, and it is now a safe path rather
  than one that spins.
- **Set `GiveUpAfter`** for any job whose continued failure should page
  someone rather than accumulate quietly. The value is how long the job may
  stay broken before it is somebody's problem.

## Pointers

- `pg/joblock/run.go` — `joblock.Run`, and `internal/pacer` the restart pacer
  it shares with `elephantine.ErrGroup.GoWithRetries`.
- `pg/joblock/lock.go` — `joblock.Options`, `RunWithContext`, the lock loop, ping
  and stale-lock stealing.
- [`docs/metrics.md`](metrics.md#job-lock-alerting) — job lock alerting.
- [`docs/migrations/v0.30.md`](migrations/v0.30.md#errgroupgowithretries-and-joblockoptions)
  — migrating from the count-based failure limit.
