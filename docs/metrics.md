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

- job locks via `joblock.Options{MetricsRegisterer: ...}`
- RPC hooks and interceptors via `elephantine.ServiceOptions.AddMetricsHooks(reg, ...)`
- error groups via `elephantine.WithErrGroupMetricsRegisterer(...)`
- FanOut recovery via the shared `MetricsHelper`

Tests inject `prometheus.NewRegistry()` (or `NewPedanticRegistry`) so
instrumented code can run without colliding on the default registry.

## Standard instrumentation checklist

Every service should have:

1. **Connection pools** — create them with `pg.NewPools(ctx, reg,
   connString, maxConns, pg.WithBouncer(...), pg.WithPubSub())`, which
   registers a `pg.NewPoolStatCollector` for each pool it opens: `"main"`
   for the pool queries run on, and `"pubsub"` for the direct pool kept for
   LISTEN behind a bouncer. A pool outside that pair uses `pg.NewPool` with a
   name for its role. A service whose connections need per-connection setup,
   such as registering pgvector's types, passes `pg.WithAfterConnect` to
   either; `NewPools` applies it to every pool it creates.
2. **Job locks** — every `joblock.New`/`joblock.Run` call passes
   `MetricsRegisterer` so `pg_job_lock_held`,
   `pg_job_lock_transitions_total`, and `pg_job_lock_restarts_total` land
   on the service registry.
3. **Outbound HTTP** — one `elephantine.NewHTTPClientInstrumentation` per
   binary, and every outbound client instrumented under its own name
   (`repository`, `assets`, `s3`, `oidc`, `jwks`, ...), one name per
   dependency.
4. **RPC** — `ServiceOptions.AddMetricsHooks(reg,
   elephantine.WithTwirpMetricsCustomerFunc(...))` with the customer function
   returning the caller's org claim. The org is bounded; the subject would
   put one label value per API client into every RPC series. The function is
   passed on to the Connect interceptor, so the label means the same thing on
   both stacks.
5. **Task groups** — top-level subsystems run under
   `elephantine.NewErrGroup` so panics are recovered and restarts are
   counted in `task_restarts_total`.

## RPC metrics

The RPC server metrics are declared by the library, once, and shared between
the Twirp hooks and the Connect interceptor, so a service serving both
protocols reports one set of series whichever stack registers first.

| Metric | Labels | What a change means |
|---|---|---|
| `rpc_requests_total` | `service`, `method`, `customer` | Requests that reached a handler. A drop for a method that normally sees steady traffic is a caller that has stopped calling, or an ingress that has stopped routing. A call refused by authentication is counted as a response and not as a request, on both stacks, so a gap between the two series is callers being turned away at the door. |
| `rpc_duration_seconds` | `service`, `method`, `customer` | Handler runtime of a **unary** call. A rising high percentile on one method is that method's dependency, not the service as a whole. |
| `rpc_stream_duration_seconds` | `service`, `method`, `customer` | Lifetime of a streaming call, observed when the stream ends. The buckets run out to about four and a half hours, so a subscription that lives for an hour is readable rather than piled into `+Inf`. |
| `rpc_streams_active` | `service`, `method` | Streaming calls currently open, incremented when a stream opens and decremented when it closes. This is the one an alert wants: a subscription count that does not fall after a deploy is a leak, and one that does not rise again is a client that has stopped reconnecting. |
| `rpc_responses_total` | `service`, `method`, `status`, `customer` | Responses by HTTP status. Note that Connect answers `failed_precondition` with `400` where Twirp answered `412`, so a lock conflict is not visible as a status any more. |
| `rpc_protocol_responses_total` | `service`, `method`, `protocol`, `code`, `client_id` | Responses by protocol, RPC code and calling client. `protocol="twirp"` going to zero for a method is what says its Twirp mount can be removed, and `client_id` names the applications that still have to move before it can; a rising `code` share is the error breakdown `rpc_responses_total` cannot give, since several codes share a status. |

`service` is the short service name (`Documents`), the same value on both
stacks. `protocol` is one of `twirp`, `connect`, `grpc`, `grpc-web`, or
`other` for a protocol connect-go adds later. `code` is the RPC code string
(`not_found`, `failed_precondition`) or `ok`. `client_id` is the `client_id`
claim of the caller's token, falling back to the authorized party (`azp`), and
is empty for an anonymous caller and for a call authentication refused.

**`rpc_duration_seconds` is unary only.** A stream's "duration" is the lifetime
of a subscription, and the histogram's top bucket is about thirty seconds, so a
single long-lived stream would land in `+Inf` and drag every quantile computed
over the service with it — a p99 latency panel for a service that serves one
subscription reads nothing. Streams are observed in
`rpc_stream_duration_seconds` instead, and counted while they are open in
`rpc_streams_active`. They are still counted in `rpc_requests_total` when the
stream opens and in `rpc_responses_total` and `rpc_protocol_responses_total`
when it closes, with the code of whatever ended the stream: one request, one
response. There is deliberately no per-message counter — it is a real cost on a
hot stream and nothing has asked for one.

`status` is the HTTP status the response is answered with, so
`failed_precondition` is `412` on Twirp and `400` on Connect, and every gRPC
and gRPC-Web response is `200`:
those protocols answer 200 and carry the code in the trailers, so their outcome
is only readable in `rpc_protocol_responses_total`.

What the series do not cover:

- **Framework-level failures on the Connect stack are uncounted.** connect-go
  answers a malformed body, an unsupported content type, an unknown method and
  a body that streams past the request limit itself, before it calls an
  interceptor, so none of them reach the metrics. The Twirp stack counts the
  same failures through its hooks, as `malformed` and `bad_route`, which means
  the two stacks are comparable for the calls that reached a method and not for
  the requests that never named one. A Connect mount that is being probed shows
  up in the ingress logs rather than here.
- **A call the authentication middleware refuses is counted as a response and
  not as a request**, on both stacks, so a gap between the two series is
  callers being turned away at the door. The middleware answers those requests
  itself and reports them itself; the `client_id` label is empty for them,
  since the caller was never identified.
- **A stream that failed after its first message is a `200` in
  `rpc_responses_total` on the wire but is reported under the status its code
  maps to**, since the label is computed from the error the handler returned
  and not from the bytes on the wire. The response status was written with the
  first message, so nothing on the wire says the call failed;
  `rpc_protocol_responses_total`'s `code` label is the one that names what
  ended it.
- **A handler error that wraps a context cancellation or a deadline without
  being a `*connect.Error`** is reported as `canceled` or `deadline_exceeded`,
  the code Connect answers it with, rather than `unknown`. Growth in
  `code="unknown"` is therefore a real server fault and not a client that hung
  up.

## Job lock alerting

`pg_job_lock_restarts_total{name}` counts restarts of a
`joblock.Run`-supervised function after an error return. Restart
pacing keeps a failing worker from hammering the shared database, but it
does not make the worker healthy — alert on a sustained non-zero
`rate(pg_job_lock_restarts_total[5m])`.
`rate(pg_job_lock_transitions_total[5m])` is the indirect signal for the
same condition (lock churn) and additionally catches a lock ping-ponging
between replicas.

A job that must not fail indefinitely should also set
`joblock.Options.GiveUpAfter`, so that a job that has been failing for that
long without any run lasting `HealthyRuntime` (five minutes by default) makes
`joblock.Run` return an error rather than restart forever. That surfaces as
a task failure in the supervising `elephantine.ErrGroup` — `task_restarts_total`
or, for a `Required` task, service shutdown — instead of only as a restart
rate to notice.

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
