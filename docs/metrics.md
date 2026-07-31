# Metric conventions for elephant services

These are the conventions for Prometheus metrics in elephant services. New
instrumentation should follow them, and existing metrics in pre-production
services should be brought in line when touched.

**Production services are the exception: renaming a metric or changing its
labels breaks dashboards, alerts, and recording rules. There, adding metrics
is always fine, but never rename or relabel an existing metric without an
explicit decision to do so.**

## One declaration site

Every collector a service owns is declared in a single metrics file and
registered by one `NewMetrics(reg prometheus.Registerer)` constructor using
`elephantine.NewMetricsHelper`, with a single `Err()` check that fails
startup. Subsystems get their own metric group struct that is passed to
their constructor.

- No `promauto`, no `MustRegister`, no package-level collector variables,
  and no registration outside the metrics file (the pgxpool collector,
  registered where the pool is created, is the accepted exception).
- A registration clash should fail startup, not silently go missing.

## Naming

- Prefix every service-owned metric with the short service name:
  `repository_`, `hub_`, `assets_`, `live_`, `distribution_`. Not
  `elephant_<service>_` — the service name is the namespace.
- Library metrics keep their own prefixes (`rpc_`, `client_`, `pgxpool_`,
  `pg_job_lock_`, `pg_fanout_`, `task_`, `eventlog_follower_`,
  `health_check_up`). Never duplicate in service metrics what a library
  already reports.
- Unit suffixes: `_total` for counters, `_seconds` for durations,
  `_timestamp_seconds` for unix-time gauges, `_bytes` for sizes.
  Instantaneous gauges (`live_blogs`, `assets_warm_queue_depth`) take no
  unit suffix.
- Prefer a timestamp gauge over an age gauge where both would work, and
  leave a timestamp gauge unset rather than reporting the epoch.

## Labels

- Label names and label values are constants. The value constants double as
  the documented label space for the metric.
- Label values are bounded vocabularies in snake_case. Bucket anything that
  cannot resolve to a known value as `unknown` (value not known yet) or
  `other` (known but outside the enumerated set).
- Never use unbounded values: document/blog/post ids, request paths, or
  anything caller-controlled. Route labels are handler names, never URL
  paths.

## Help text

A complete sentence ending in a period. Say what a change in the value means
to an operator during an incident, not just what the metric counts:

> "Validator reloads by trigger and outcome. A reload pattern dominated by
> the periodic trigger means the configuration LISTEN is not delivering."

## Registerer injection

The application package takes a `prometheus.Registerer` parameter; only
`main` chooses `prometheus.DefaultRegisterer`. Everything downstream uses
the injected registerer:

- job locks via `pg.JobLockOptions{MetricsRegisterer: ...}`
- Twirp hooks via `elephantine.WithTwirpMetricsRegisterer(...)`
- error groups via `elephantine.WithErrGroupMetricsRegisterer(...)`
- FanOut recovery via the shared `MetricsHelper`

Tests inject `prometheus.NewRegistry()` (or `NewPedanticRegistry`) so
instrumented code can run without colliding on the default registry.

## Standard instrumentation checklist

Every service should have:

1. **Connection pools** — `pg.NewPoolStatCollector(pool, "main")` for the
   primary pool; additional pools are named for their role (e.g.
   `"pubsub"`).
2. **Job locks** — every `pg.NewJobLock`/`pg.RunInJobLock` call passes
   `MetricsRegisterer` so `pg_job_lock_held` and
   `pg_job_lock_transitions_total` land on the service registry.
3. **Outbound HTTP** — one `elephantine.NewHTTPClientIntrumentation` per
   binary, and every outbound client instrumented under its own name
   (`repository`, `assets`, `s3`, `oidc`, `jwks`, ...), one name per
   dependency.
4. **RPC** — Twirp metrics hooks with `WithTwirpMetricsCustomerFunc`
   returning the caller's org claim. The org is bounded; the subject would
   put one label value per API client into every RPC series. Hook order
   matters: auth hooks must run before the metrics hooks.
5. **Task groups** — top-level subsystems run under
   `elephantine.NewErrGroup` so panics are recovered and restarts are
   counted in `task_restarts_total`.

## Enforcement

Each service has a metrics test that:

- constructs the full metric set (everything switched on) against a fresh
  registry, and fails on registration errors, and
- runs `prometheus/testutil.GatherAndLint` over the registry so naming
  violations fail the build.

Cardinality rules that matter (e.g. "request labels only come from resolved
configuration, never from the URL") deserve their own regression test.

## Documentation

Document the exported metric surface in the service repo — a
`docs/metrics.md` or a README "Observability" section — including how to
read the important series during an incident, and keep it in sync when
metrics change.
