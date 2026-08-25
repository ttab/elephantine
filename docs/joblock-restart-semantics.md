# pg.RunInJobLock restart semantics — handover

Status: recommendation in §6 implemented in elephantine (restart pacing,
rewritten contract, `pg_job_lock_restarts_total`); the follow-ups in other
repos (§6 last paragraphs) remain open. §8 (2026-08-04) adds two related
findings: both fixed, on `feature/joblock-retries` (ping pacing) and in
elephant-everysport (`feature/syncer-run-in-job-lock`). §9 (2026-08-25) adds
a failure limit and panic recovery to `RunInJobLock`.
Origin: adversarial review of the elephant-distribution indexer (2026-08-01).
The reviewer classified this as one of two high-severity findings in that
changeset; the service-side workaround is described in §4 and the underlying
design question is elephantine's to answer.

## 1. What the code does today

`pg.RunInJobLock` (`pg/joblock_service.go`) is an unbounded loop:

```go
for {
    lock, err := NewJobLock(db, logger, lockName, options)
    ...
    err = lock.RunWithContext(ctx, fn)
    if err != nil {
        logger.ErrorContext(ctx, "failed to run ...", ...)
    }

    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }
}
```

`JobLock.RunWithContext` is **one-shot**: it starts the lock loop, waits for
acquisition, runs `fn` exactly once, and the deferred `Stop()` releases the
lock when `fn` returns — for any reason. `RunInJobLock` then immediately
constructs a fresh lock and re-acquires. Since this instance just deleted its
own `job_lock` row, `attemptAcquire` finds no holder and wins on the first
try, with no delay anywhere in the path.

Three properties fall out of this, none of them stated in the doc comment
("run the provided function until the context is cancelled"):

1. **There is no pacing.** A returned error restarts `fn` after ~three
   Postgres round trips (release, acquire-tx, insert) and a log line.
   Nothing else.
2. **Success and failure are indistinguishable.** `fn` returning `nil` also
   restarts immediately. There is no "run once under the lock" mode; any
   `fn` that can return quickly — with or without an error — spins.
3. **The lock is released on every restart.** The iteration counter churns
   and, in a multi-replica deployment, the lock can ping-pong between
   instances on each failure, each time with a cold in-process cache.

The only cases that self-pace are the ones where *Postgres itself* is the
problem: a failed acquire waits `CheckInterval` (default 20 s) between
attempts, and each lock operation carries the `Timeout` (default 5 s). The
pathological case is precisely the inverse — **Postgres healthy, `fn`
persistently failing fast** — which is also the common shape of a worker
whose downstream dependency (object storage, OpenSearch, an HTTP API) is
down.

## 2. The concrete failure scenario

The elephant-distribution indexer consumes the distribution eventlog and
writes to OpenSearch, under a job lock on the Postgres database it shares
with the delivery pipeline. Its natural loop shape was: read batch, index,
on error return and let `RunInJobLock` restart it — the shape every other
worker in that service uses.

With OpenSearch down, `fn` fails in milliseconds. The result is a tight
loop of lock delete/insert row churn against the **shared** database,
an `Error`-level log line per iteration (log volume amplifier), and
`pg_job_lock_transitions_total` counting thousands of transitions — all
while the worker does no useful work. The service's own design doc promises
that an indexer outage degrades to *lag*, never to load on the shared
pipeline; the restart loop silently converts a dependency outage into
exactly that load. A worker with a poison input that fails deterministically
before its first blocking call has the same effect and no outage to blame.

The backoff the indexer author wrote for this was **dead code**: it lived
after the `RunInJobLock` call, on the theory that the call returns between
restarts. It never returns until context cancellation.

## 3. How call sites cope today (survey, 2026-08-01)

- `elephant-distribution` `eventlogLoop` and `syncLoop` return batch errors
  to the lock — both are exposed to the hot loop for any persistent non-DB
  failure inside a batch. They have simply never hit one that lasts.
- `elephant-distribution` `runArchivedChunk`/pruner: same pattern.
- `elephant-assets` key rotation documents the reliance explicitly: "If the
  lock is lost the loop exits and RunInJobLock re-acquires it" — correct for
  lock loss (that path *is* paced by acquisition), wrong for its own errors.
- `elephant-repository` has a **copied twin**: `Scheduler.RunInJobLock`
  (`repository/scheduler.go:90`) reproduces the identical zero-delay loop,
  so the pattern is propagating by example.
- `elephant-index`'s indexer avoids the trap by accident of shape: it
  retries internally with a 5 s sleep and rewinds its follower, so it rarely
  returns.
- `elephant-everysport` (added 2026-08-04, see §8) doesn't use
  `RunInJobLock` at all: `internal/app.go` calls one-shot
  `lock.RunWithContext` directly inside an errgroup task and swallows
  `context.Canceled` on the theory that cancellation means graceful
  shutdown. Lock loss produces the identical error, so the task returns
  nil and the service lingers with the API server up and the syncer dead —
  the inverse failure of the spin: silent death instead of churn.
- `elephant-distribution`'s new indexer (`index/indexer.go`, `loop`) is the
  first call site to treat the semantics as load-bearing: it **never
  returns an error**, and implements retry-in-place with exponential
  backoff (1 s → 60 s), panic containment, and gauge cleanup inside `fn`,
  with a comment explaining that returning would cause lock churn.

The pattern in that last bullet works, but it means the error return of
`fn` is a trap that every author must know not to use, and every worker
must reimplement its own supervision. An API whose error path must never
be exercised is mis-shaped.

## 4. The service-side workaround (reference implementation)

`elephant-distribution/index/indexer.go` — `loop` plus the recovery wrapper
in `Run`:

- batch errors: log, sleep `delay`, `delay = min(2*delay, max)`, reset on
  success; never returned.
- panics: recovered in `Run`, converted to an error for the errgroup
  supervisor which itself restarts with backoff (the errgroup member also
  never returns).
- metric hygiene: the position gauge is set to NaN on loop exit so a
  follower that lost the lock doesn't report a frozen cursor.

If `RunInJobLock` gains sane restart semantics, most of this scaffolding
stays (retry-in-place is still cheaper than a full release/re-acquire per
batch failure), but it stops being *mandatory* for correctness, and the
simpler workers (`eventlogLoop`, key rotation, the repository scheduler)
are fixed without being touched.

## 5. Design options

**(a) Backoff in the restart loop.** Exponential with jitter, capped
(e.g. 1 s → 60 s), applied when `fn` returned an error; reset when the
previous run lasted longer than some healthy-runtime threshold. Fixes every
caller at once, no API change, strictly gentler behaviour. The lock stays
released during the backoff window, which is arguably a feature: another
replica — possibly one whose network path to the failing dependency works —
gets a chance to take over.

**(b) Minimum-runtime guard.** The cheap version of (a): if `fn` returned
(error or not) within N seconds of starting, sleep the remainder before
re-acquiring. Catches the `nil`-return spin too, which (a) alone does not.

**(c) Retry-in-place while holding the lock.** Restart `fn` without
releasing for the first N failures, then fall back to release + backoff.
Avoids lock churn and multi-replica ping-pong entirely, keeps caches warm.
Requires restructuring: `JobLock` is one-shot per instance (`sync.Once` on
the loop, `Stop` closes channels), so `RunWithContext` would need to own an
inner restart loop with the held lock's context. More invasive; the payoff
over (a)+(b) is modest. A wedged-but-not-erroring holder is unaffected
either way (`StaleAfter` stealing covers that).

**(d) Decide what `fn == nil` means.** Today it means "restart immediately
forever". Either document that `fn` must block for the lifetime of the
context (and maybe log a warning on fast nil returns), or add a run-once
variant for jobs that want "at most one replica executes this, then done".
Both are defensible; the current silent spin is not.

**(e) Observability regardless of fix.** Restart pacing makes the loop
survivable but a persistently failing worker should still page someone.
`rate(pg_job_lock_transitions_total[5m])` already exposes churn — a
documented alert on it (in `docs/metrics.md`) may be all that's needed,
optionally plus a `pg_job_lock_restarts_total{name}` counter incremented
when `fn` returns an error, which is the direct signal rather than the
side effect.

## 6. Recommendation

(a) + (b) + (d)-as-documentation + (e), in one change:

- backoff-with-jitter on error returns, minimum-runtime guard on all
  returns, both capped and reset on healthy runs;
- doc comment rewritten to state the actual contract: `fn` is expected to
  block until its context is cancelled; returning — with or without an
  error — releases the lock and restarts after a pause;
- the restarts counter, and an alerting note in `docs/metrics.md`.

Then fold `elephant-repository`'s `Scheduler.RunInJobLock` back onto the
library version (it predates whatever made it diverge — the diff is the
`recheckSignal` plumbing, which fits through the closure), and delete the
now-redundant halves of the elephant-distribution indexer's private
supervision if (c) is ever adopted.

Compatibility: every current caller either blocks forever (unaffected) or
relied on instant restarts only accidentally (now paced — strictly less
load, marginally slower recovery from *transient* failures, bounded by the
backoff floor). No signature changes.

## 7. Pointers

- `pg/joblock_service.go` — the loop.
- `pg/joblock.go` — `RunWithContext` (one-shot), `loop`, `attemptAcquire`
  (why re-acquisition after self-release is instant), `ping` (why DB-down
  self-paces).
- `elephant-distribution/index/indexer.go` — `Run`/`loop`, the reference
  workaround and its rationale comment.
- `elephant-distribution/docs/distribution-index.md` §2.1 — the isolation
  requirement this violated.
- `elephant-repository/repository/scheduler.go:90` — the copied twin.

## 8. Addendum 2026-08-04 — the everysport zombie and the ping hot loop

Two findings from a stage incident (elephant-everysport, sports calendar
sync silently stopped for 24h+ after a postgres failover on 2026-08-03),
both in the same code but neither covered above.

**Silent death: the inverse of the spin.** §1–§3 treat the failure mode
where restarts are too eager. everysport had the opposite: it called the
one-shot `RunWithContext` directly (no `RunInJobLock`) inside an errgroup
task, swallowing `context.Canceled` as graceful shutdown. Lock loss cancels
`fn`'s context the same way a shutdown does — `RunWithContext` gives the
caller no way to tell them apart — so on lock loss the task returned nil
and the errgroup happily kept the rest of the service running: API server
up, pod Ready, syncer gone. Fixed by moving everysport onto `RunInJobLock`
(`feature/syncer-run-in-job-lock` in that repo). For the library, the open
question is whether any other call site uses `RunWithContext` directly; a
sentinel like `ErrJobLockLost` would make the distinction expressible, but
with `RunInJobLock` as the documented entry point it may be enough to
steer callers away from the one-shot API. `ErrGroup.Required` is *not* the
right guard for tasks whose siblings use staged shutdown: its group-wide
cancel on return collapses the stop→quit drain window.

**The ping hot loop.** §1's claim that "Postgres itself being the problem
self-paces" was wrong for held locks: a failed ping left `lastPing`
untouched (staleness is measured from it), so the held-state wait
`time.Until(lastPing.Add(pingInterval))` was already negative and pings
retried as fast as the failure surfaced. With an instantly-failing dial
(dead primary IP after failover) that produced hundreds of ping attempts
and Error log lines per second for the whole stale window. Fixed on
`feature/joblock-retries` by pacing retries from the last attempt
(`nextPingWait`), keeping `lastPing` as the staleness anchor.

## 9. Addendum 2026-08-25 — bounded restarts and panic recovery

Restart pacing (§6) made a persistently failing job survivable but still
unbounded: paced at the 60 s ceiling a job can fail for weeks without
anything but a metric noticing, and the restart loop is the only supervisor
that ever sees the error. Two additions close that.

**A consecutive failure limit.** `JobLockOptions.MaxConsecutiveFailures`
bounds the loop: once that many runs have failed in a row, `RunInJobLock`
returns an error wrapping the last one instead of restarting, handing the
failure to the caller's `ErrGroup` — restart-with-backoff for a `Go` task,
service shutdown for a `Required` one. Zero, the default, keeps the old
unbounded behaviour, so no existing caller changes.

The definition of a success is deliberately *not* "returned nil": it is
"ran for at least `HealthyRuntime`", five minutes by default. A job whose
`fn` blocks as the contract asks reaches that within one run; a job that
fails fast never does, so its failures accrue to the limit however slowly
they arrive — which is the whole point, given that a fast nil return is
just as anomalous as an error under the documented contract. Only a run of
that length clears the count, and a run that lasted that long clears it
even if it ended in an error.

Two attribution details matter. The runtime is measured *inside* the lock,
not around `RunWithContext`, so a follower's acquisition wait is not
credited as healthy runtime — otherwise a replica that polls for twenty
minutes and then fails instantly would look healthy every time. And a run
cut short by lock loss surfaces as `context.Canceled` from `fn`; that is
not counted as a failure, so a lock ping-ponging between replicas cannot
take the service down. The backoff keeps its own, shorter reset threshold
(`restartMinRuntime`, 10 s): it paces a job that is failing now, it does
not judge health.

**Panics as errors.** `fn` is now run through
`elephantine.CallWithRecover`, so a panic becomes an
`ErrPanicRecovered` return: logged, counted in
`pg_job_lock_restarts_total`, paced, and subject to the failure limit,
instead of taking the process down and leaving the lock row to go stale.
This makes the panic containment that the elephant-distribution indexer
implements privately (§4) redundant for callers that only had it to avoid
a crash.