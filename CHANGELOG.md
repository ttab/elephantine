# Changelog

All notable changes to this library from v0.26.0 onwards are documented here.
The entries below are derived from release tags; see the linked PRs for full
detail.

## [v0.28.0] - Unreleased

**Behaviour change (request bodies):** `APIServer` now caps request bodies at
`DefaultMaxBodyBytes` (8 MiB) on both the plain and the TLS listener. A request
that declares a larger `Content-Length` is refused with `413` before it reaches
a handler, and a body of unknown length fails on the read that passes the limit.
Previously bodies were unbounded, and since Twirp buffers the whole body before
unmarshalling, so was the allocation per in-flight request. A service that has
to accept larger uploads raises the cap with `APIServerMaxBodyBytes(n)`; `0` or
less turns it off. elephant-hub's multipart CI publish endpoint is the known
case that needs a higher limit.

**Breaking (job lock API):** the job lock moved out of the `pg` package into
`github.com/ttab/elephantine/pg/joblock`, and the names lost their stutter:
`pg.RunInJobLock` is `joblock.Run`, `pg.NewJobLock` is `joblock.New`,
`pg.JobLockOptions` is `joblock.Options`, `pg.JobLock` is `joblock.Lock` and
`pg.JobLockState` with its `JobLockState*` constants is `joblock.State` with
`joblock.State*`. Signatures, behaviour, metric names and log keys are
unchanged, so the upgrade is a rename at every call site. The `pg/postgres`
package is gone; nothing outside the library used it.

**Breaking (job lock migration path):** the tern migration a service vendors
now lives in `pg/joblock/schema` instead of `pg/schema`, so each future
feature that needs a table can ship its own migration directory and a service
vendors only the features it uses. A service that vendored or covered the
migration from v0.27.6 changes the `dir` in `schema/vendor.json` to
`pg/joblock/schema` and the `-- vendored-from:` or `-- covers:` line in its
migration to `github.com/ttab/elephantine pg/joblock/schema/001_job_lock.sql`.
Until it does, `mage sql:vendorCheck` fails because the old directory no longer
exists in the module. The migration file itself is byte-identical, so its
checksum and applied state are untouched. New consumers run
`mage sql:vendorAdd github.com/ttab/elephantine pg/joblock/schema`.

Changes:

- `APIServerPublicCORS(prefixes...)` marks path prefixes as open to any
  origin: requests under them are answered with `Access-Control-Allow-Origin:
  *` and no `Vary`, and their preflights succeed regardless of `Origin`, so an
  anonymous read surface behind a CDN is cached once per URL instead of once
  per embedding site. Paths outside the prefixes keep the allowlist behaviour.
- `APIServerMaxBodyBytes(n)` and `DefaultMaxBodyBytes` configure the request
  body limit described above.
- The job lock moved to `pg/joblock` with its migration, flat schema and
  generated queries, as described above. The README documents vendoring the
  migration and the upgrade step for existing consumers.
- The `Makefile` is gone; `mage sql:generate` and `mage sql:librarySchema` run
  sqlc through the image pinned by `github.com/ttab/mage`.

## [v0.27.6] - 2026-09-01

**Migrations:** the `job_lock` table is now shipped as a tern migration a
service vendors into its own `./schema` with `mage sql:vendor`, instead of DDL
each service hand-copied from `pg/schema.sql`. `mage sql:vendorCheck` fails when
the library ships a migration the service has not taken. A service that already
created the table by hand asserts that with a `-- covers:` comment in the
migration that did the work rather than vendoring a duplicate. (#269)

- `pg/schema/001_job_lock.sql` creates the `job_lock` table. Run it before
  deploying a service that uses the job lock and does not already have the
  table; it is a single `CREATE TABLE` and needs no maintenance window.

Changes:

- The job lock migration is shipped for vendoring, and `pg/schema.sql` is
  generated from it by `mage sql:librarySchema` with a test that fails when the
  two drift. (#269)
- Dependency upgrades: `github.com/ttab/mage` to v0.11.2 for the terminated
  vendored-file header.

## [v0.27.5] - 2026-08-26

**Behaviour change (job lock restarts):** a panic in a function run by
`RunInJobLock` is now recovered and treated as a failed run — logged, counted in
`pg_job_lock_restarts_total`, paced and restarted — instead of taking the
process down and leaving the lock row to go stale. A worker that relied on a
panic to crash the service and trigger a restart no longer gets one; use
`MaxConsecutiveFailures` below to make persistent failure fatal. (#268)

Changes:

- `JobLockOptions.MaxConsecutiveFailures` bounds the restart loop:
  `RunInJobLock` returns an error wrapping the last failure once that many
  consecutive runs have failed without any of them lasting
  `JobLockOptions.HealthyRuntime` (five minutes by default). Zero, the default,
  keeps restarting forever as before. The runtime is measured inside the held
  lock, and a run cut short by lock loss does not count as a failure. (#268)
- `docs/joblock-restart-semantics.md` describes the contract, pacing, failure
  limit, panic handling and metrics as they are, replacing the handover note it
  used to be. (#268)
- The `/version` endpoint tests no longer depend on the toolchain's build info.
  (#268)

## [v0.27.4] - 2026-08-04

**Behaviour change (job lock restarts):** `RunInJobLock` no longer restarts
its function immediately when it returns. An error return is retried with
exponential backoff and equal jitter from 1 s to a 60 s cap, and any return is
padded so that the function starts at most once every ten seconds; a run lasting
at least ten seconds resets the backoff. The lock stays released while waiting,
so another replica can take over. A worker whose dependency fails fast
previously turned into a tight loop of lock release and re-acquire against the
shared database. (#265)

**Behaviour change (job lock pings):** failed pings on a held lock are paced
from the last attempt rather than the last successful ping. A dead database
connection previously retried as fast as the failure surfaced, producing
hundreds of ping attempts and error log lines per second for the whole stale
window. (#265)

Changes:

- `pg_job_lock_restarts_total{name}` counts restarts of a `RunInJobLock`
  function after an error return. Alert on a sustained non-zero rate. (#265)
- `docs/metrics.md` documents the metric conventions for elephant services:
  registerer injection, naming, the standard instrumentation checklist and job
  lock alerting.
- Dependency upgrades: `github.com/prometheus/client_golang` to v1.24.1 and
  `github.com/MicahParks/keyfunc/v3` to v3.8.1.

## [v0.27.3] - 2026-07-30

Changes:

- `pg.NewPoolStatCollector(pool, name)` exposes `pgxpool` connection pool
  statistics (saturation, acquire waits, connection churn) as Prometheus
  metrics.
- The job lock reports `pg_job_lock_held{name}` and
  `pg_job_lock_transitions_total{name,state}` so lost locks and leadership
  flapping are visible. `JobLockOptions.MetricsRegisterer` chooses the
  registry, defaulting to `prometheus.DefaultRegisterer`.
- `ErrGroup.GoWithRetries` counts restarts in `task_restarts_total{name}`.
- `RegisterOrReuse` lets a metric vector shared between components be
  registered by every user of it, and `MetricsHelper` gains a `Collector`
  method.

## [v0.27.2] - 2026-07-20

**Behaviour change (test helpers):** the golden-file and printf-style
assertion helpers in `test` were renamed to lint-clean names:
`TestAgainstGolden` is `AgainstGolden`, `Must` with a format string is `Mustf`,
and so on. The old names remain as deprecated inline shims, so nothing breaks,
and `go fix -inline ./...` rewrites call sites to the new names. (#262)

Changes:

- `JWTClaims` carries the `email` claim. (#262)
- Prometheus metric labels use dedicated constants instead of reusing the
  `LogKey*` attribute keys; the label values are unchanged. (#262)
- Every exported identifier has a godoc comment, and the repository lints
  clean under a `.golangci.yml` aligned with the other Elephant projects.
  (#262)
- Dependency upgrades. (#262)

## [v0.27.1] - 2026-06-04

Changes:

- `Subscriber.Bounce` tears down the current LISTEN connection and reconnects,
  for the case where the connection has gone silently bad while the ping-based
  health check stays green. Bounces during one outage coalesce. (#251)
- `FanOut.EnableRecovery` folds the recovery bookkeeping into the fan-out:
  consumers report fallback-poll findings with `FanOut.Polled`, wire-side
  deliveries reset the streak, and the fan-out bounces the subscriber once the
  streak of consecutive non-empty polls crosses a threshold. It registers a
  per-channel poll-saved counter and streak gauge. See
  `docs/fanout-recovery.md` for the pattern. (#251)

## [v0.27.0] - 2026-06-02

**Behaviour change (authorization errors):** `RequireAnyScope` no longer lists
the accepted scopes in the `permission_denied` error message. They are returned
as Twirp error meta under the key `required_any_of_scopes` instead, and an empty
subject is treated as anonymous. A client that parsed the scope list out of the
message text has to read the meta key. (#252)

Changes:

- `ErrGroup.Required` runs a task whose exit, even with a nil error, cancels
  the group so that sibling tasks stop and `Wait` unblocks. It is for
  subsystems that must run for the whole lifetime of a service, where lingering
  with a subset of subsystems is worse than restarting. Plain `Go` tasks keep
  their behaviour. (#253)
- `ErrTaskDisabled` lets a `Required` task that is disabled by configuration
  opt out from inside: the group is left as if the task was never registered.
  (#253)
- Dependency upgrades: `github.com/urfave/cli/v3` to v3.9.0.

## [v0.26.3] - 2026-05-20

Changes:

- `CORSOptions.AllowOrigin` exposes the origin check for callers that need it
  outside the CORS middleware, such as WebSocket upgrade handlers.

## [v0.26.2] - 2026-05-06

Changes:

- `HealthServer.AddOptionalReadyFunction` registers a readiness check whose
  failure is reported in the response body but does not turn
  `/health/ready` into a 500. A `health_check_up` gauge reports the result of
  every check. (#250)
- `NewHealthServer` with an empty address produces a server with its readiness
  machinery but no bound socket, and `APIServer.ListenAndServe` skips the
  health goroutine in that case, for processes that share a health endpoint
  with another listener. (#250)

## [v0.26.1] - 2026-04-20

Changes:

- Dependency upgrades: `github.com/jackc/pgx/v5` to v5.9.2.

## [v0.26.0] - 2026-04-18

Changes:

- `APIServer` serves `GET /version` on the public API port with the
  application name, version, VCS stamp and a curated module list, and
  `GET /debug/bom` on the internal health port with the full build info in
  `go version -m` format. `APIServerVersion(version)` sets the version string,
  typically from `-ldflags "-X main.version=..."`, and
  `APIServerModules(...)` adds modules to the `/version` list. Without a
  version the endpoint reports `v0.0.0-dev`. The README documents the tag to
  build-arg to ldflag pipeline. (#249)
- Twirp `not_found` responses are logged at info level, as `400` responses
  already were.
