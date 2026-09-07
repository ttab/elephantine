# Changelog

All notable changes to this library from v0.26.0 onwards are documented here.
The entries below are derived from release tags; see the linked PRs for full
detail.

## [v0.29.1] - Unreleased

**Test flake fix (`test.NewLogHandler`):** a package whose tests log through the
test log handler could have its whole test binary killed by `panic: Log in
goroutine after TestX has completed`, or the `panic: Write called after TestX
has completed` variant, naming whichever test happened to finish last. Every
test in the package failed with it, and the named test was rarely the one at
fault, so it read as unrelated flakiness. The handler dropped records logged
after the test by checking a flag its `t.Cleanup` set, but that check and the
`t.Log` it guarded were not atomic with respect to the cleanup: a goroutine that
read the flag as unset, was preempted, and resumed after the test had been
marked complete then called `t.Log` on a finished test. `Write` now serialises
against the cleanup with a mutex, so a record either reaches `t.Log` before the
cleanup returns, while logging is still legal, or is dropped. Upgrading is the
whole fix; no consumer code changes. The panics understated how often this
fired: only a straggler from the last test to complete panics, and one from any
other test was silently appended to the root test's output instead, so a
consumer that logs from a worker on `t.Context()` cancellation has been hitting
this window without seeing it.

Changes:

- `test.LogHandler.Handle` and `test.LogHandler.Enabled` no longer short-circuit
  once the test has completed. The guard lives in `Write`, which every record
  passes through, including records from handlers derived with `WithAttrs` and
  `WithGroup` — which the early exits never covered, since those return bare
  `slog.TextHandler` clones. A straggling record is now formatted and then
  dropped rather than dropped before formatting.
- `test.LogHandler` holds a mutex, so a `LogHandler` value must not be copied;
  create it with `NewLogHandler`, which returns a pointer. A `TestingLogger`
  whose `Log` blocks now delays the test's cleanup for as long as it blocks
  instead of racing past it, and one that logs back through the same handler
  would deadlock — `*testing.T` and `*testing.B` do neither.

## [v0.29.0] - 2026-09-06

**Breaking (authentication):** `ServiceOptions.SetAuthInfoValidation` is
protocol-neutral HTTP middleware rather than a Twirp `RequestRouted` hook, and
it fails closed: it answers a request it could not authenticate itself, before
the request reaches a handler, an interceptor or a hook, rendering the error in
the protocol the caller is speaking with `connect.NewErrorWriter` for Connect,
gRPC and gRPC-Web and `twirp.WriteError` for Twirp. **An invalid token is now
answered `unauthenticated` (401) where it was answered `permission_denied`
(403)**: a caller we could not identify is unauthenticated whichever way the
authorization failed, and `permission_denied` is left to mean a caller we did
identify and that lacks a scope. Anything keyed on 403 for a bad token — an
ingress rule, a dashboard panel, a client's retry logic — reads 401 after the
upgrade. `ServiceAuthOptional` still lets a request without an `Authorization`
header through as an anonymous caller, and an authorization the parser rejects
still always fails.

Two holes are closed with it. A Connect handler built without
`opt.HandlerOptions()`, or mounted by a service that composes its options by
hand, no longer runs unauthenticated: the middleware refuses the request before
the handler is reached. And every interceptor in `rpc` now implements
`WrapStreamingClient` and `WrapStreamingHandler` as well as `WrapUnary`, where
`connect.UnaryInterceptorFunc` passes streaming calls straight through, so a
streaming handler is authenticated, logged and counted like a unary one rather
than running with no check at all. The interceptor
`rpc.AuthInfoInterceptor(required)` is the remaining safety net: it refuses a
call that reaches a handler with no authenticated caller on its context, which
is what a mount that does not run the middleware would otherwise do silently.
The request-scoped "could not authenticate" marker the middleware used to set
is gone, since nothing lets such a request through any more.

Because the middleware answers before the request body is read, an
unauthenticated caller can no longer make a replica unmarshal a request body or
probe its parsing on the Connect stack, where connect-go unmarshals the body
before it runs an interceptor. What is also gone is the
`twirp.WithHTTPRequestHeaders` smuggling the middleware used to do: a handler
that read the `Authorization` header back out of the Twirp context with
`twirp.HTTPRequestHeaders` no longer finds it, and reads `GetAuthInfo(ctx)`
instead.

**Wire format (Connect JSON field names):** a Connect JSON response spells its
fields in lowerCamelCase (`documentUuid`), where a Twirp JSON response spells
them the way the `.proto` declares them (`document_uuid`). Connect's codec is
`protojson` with its default options and Twirp's sets `UseProtoNames`; nothing
else about the encoding differs, and both stacks still omit unpopulated fields.
The `ServiceOptions.JSONSkipDefaults` documentation said the two stacks produced
the same JSON, which was only ever true of that omission.

It reaches the callers that read JSON by hand, with `fetch` or `curl`: one that
changes only the path prefix gets a `200` and reads `undefined` for every
multi-word field. Generated clients — Go, `@protobuf-ts`, `connect-es` — are
unaffected, and requests are unaffected on both stacks, since `protojson`
accepts either spelling when unmarshalling. This is deliberate: the standard
encoding is what every Connect runtime and every generated client assumes, so a
service must not install a `UseProtoNames` codec to make Connect look like
Twirp. Pin it with a success-body golden per stack instead.

**Behaviour change (request bodies over the limit):** the two stacks answer a
body that streams past `DefaultMaxBodyBytes` differently. A request that
declares a larger `Content-Length` is still refused with a plain `413` on both,
before either framework sees it, but a chunked request, or one that lies about
its length, fails on the read that passes the limit: Twirp answers `malformed`
with `400` and Connect answers `resource_exhausted` with `429`. Neither is a
handler error, so a service cannot normalise them; a client that has to tell
"too big" from "too many" reads the status and the code together.

**Behaviour change (RPC metrics):** `rpc_requests_total`,
`rpc_duration_seconds` and `rpc_responses_total` keep their names, labels, help
texts and label values, but the collectors are now declared once and shared
between the Twirp hooks and the Connect interceptor, so a dual-stack service
reports one set of series and registers each metric once. As a consequence
`NewTwirpMetricsHooks` reuses an already registered collector where it used to
fail with a registration error. A new counter,
`rpc_protocol_responses_total{service,method,protocol,code,client_id}`, is
reported by both stacks: `protocol="twirp"` going to zero for a method is what
says its Twirp mount can be removed, `client_id` (the token's client id claim,
falling back to the authorized party, and empty for an anonymous caller) names
the applications that have to move before it can, and the `code` label is the
error breakdown `rpc_responses_total` cannot give, since Connect answers
`failed_precondition` with `400` rather than Twirp's `412` and so a lock
conflict no longer has a status of its own. `ServiceOptions.AddMetricsHooks`
takes the `TwirpMetricOptionFunc` options as a variadic second argument and
passes the customer function on to the Connect interceptor.

Three things about the label values are worth knowing before a panel is built
on them. `status` is the status actually sent, so a gRPC or gRPC-Web response
is `200` whatever the outcome was — those protocols answer 200 and carry the
code in the trailers — and only `rpc_protocol_responses_total` says what
happened. A handler error that is not a `*connect.Error` but that wraps
`context.Canceled` or a deadline is reported as `canceled` or
`deadline_exceeded` rather than `unknown`, because connect-go returns a bare
context error from its own handler wrapper when the request context is already
done, and counting those as `unknown` would have made every disconnected client
look like a server fault. And framework-level failures on the Connect stack are
not counted at all: connect-go answers a malformed body, an unsupported content
type, an unknown method and a body that streams past the request limit before
it calls an interceptor, where Twirp reports the same failures through its
hooks as `malformed` and `bad_route`. `docs/metrics.md` says so next to the
series.

A request the authentication middleware refuses is counted and logged by the
middleware, since it never reaches a hook or an interceptor: as a response and
not as a request, which is what the Twirp hooks have always reported, and with
the same log keys and levels. That also removes the dependence on hook order —
a service that chained `NewTwirpMetricsHooks` ahead of the authentication hook
used to count a refused call as a request as well.

**Build (Go 1.27.1):** the module's `go` directive is `1.27.1`, the latest
patch of the 1.27 line, where v0.28.0 declared `1.27.0`. Every consumer builds
elephantine with a toolchain at least that new, which `GOTOOLCHAIN=auto`
downloads by itself and `GOTOOLCHAIN=local` does not: a build box pinned to an
older toolchain fails on the upgrade rather than falling back. CI reads the
version from `go.mod`, so nothing else has to be told.

**Behaviour change (the plaintext listener):** `APIServer` serves HTTP/2
without TLS alongside HTTP/1.1 on its plain listener, which is what makes the
gRPC protocol Connect mounts on the same path reachable at all: Go negotiates
HTTP/2 through the TLS ALPN handshake and nowhere else, so the listener
answered HTTP/1.1 only before and a gRPC client could not connect. The two
protocols are told apart by the HTTP/2 connection preface, so Twirp, SSE, the
websocket upgrade and every other HTTP/1.1 caller are unaffected.

**gRPC and gRPC-Web are reachable inside the cluster only.** They are served on
the Connect paths and are a supported way for one service to call another, but
the fleet's ingress speaks HTTP/1.1 to its targets, and a gRPC target group per
service — its own listener, host and health check — was judged more work than
the benefit is worth. So no elephant API offers gRPC externally, none is
documented as doing so, and gRPC-Web is not a browser protocol here: its errors
arrive as trailers, and the CORS defaults deliberately expose no headers for
reading them. A browser client uses Connect. External access stays open as a
later iteration.

A service that mounts Connect on an `http.Server` of its own, rather than on
`APIServer`, sets the same protocols with `elephantine.PlaintextProtocols()`,
which is exported for exactly that; without it gRPC cannot be spoken to that
listener even from inside the cluster, and nothing reports the difference — the
connection is refused as HTTP/1.1.

**Behaviour change (CORS):** the default allowed request headers gain
`Connect-Protocol-Version` and `Connect-Timeout-Ms`, which a browser Connect
client sends on every call. A service that replaced `CORS.AllowedHeaders`
outright has to add them itself.

**Deprecations:** the Twirp-only error helpers are deprecated in favour of the
`rpc` package, and go away with the last Twirp mount in the fleet, which is
years rather than months away — nothing has to move today.
`elephantine.InvalidArgumentf` becomes `rpc.InvalidArgumentf`,
`IsTwirpErrorCode` becomes `rpc.IsCode`, `TwirpErrorToHTTPStatusCode` becomes
`rpc.HTTPStatus`, `RequireAnyScope` becomes `rpc.RequireAnyScope`,
`LoggingHooks` and `NewTwirpMetricsHooks` become `rpc.LoggingInterceptor` and
`rpc.MetricsInterceptor` (both installed by `NewDefaultServiceOptions`), and
`test.IsTwirpError` becomes `test.IsRPCError`. The root `RequireAnyScope` now
delegates to the `rpc` one and translates the error, so its behaviour is
unchanged by construction.

Changes:

- New package `github.com/ttab/elephantine/rpc`, the protocol-neutral RPC
  vocabulary. `rpc.Errorf`, `rpc.InvalidArgument`, `rpc.RequiredArgument`,
  `rpc.NotFound`, `rpc.AlreadyExists`, `rpc.Unauthenticated`, `rpc.Internalf`,
  `rpc.FailedPreconditionf` and `rpc.PermissionDeniedf` produce
  `*connect.Error`; `rpc.WithMeta` and `rpc.Meta` carry the metadata Twirp
  carried in its meta map, as an `elephantine.rpc.ErrorMeta` detail declared in
  `rpc/errormeta.proto`; and `rpc.IsCode` recognises both error types, so a
  caller can move its error checks before it moves its client constructor.
- `rpc.ToTwirp` and `rpc.FromTwirp` translate in both directions, preserving the
  code, the message, the metadata and the cause chain, so `errors.Is` keeps
  reaching a `pgx` or `context` error through either. `rpc.TwirpInterceptor`,
  which `ServiceOptions.ServerOptions` now always installs, is what lets a
  handler that returns Connect errors answer a Twirp caller unchanged, and
  `rpc.LegacyTwirpErrors` is the transitional interceptor for a service that
  serves Connect before its handlers have moved.
- A call that authentication refuses is counted in `rpc_responses_total` and
  `rpc_protocol_responses_total` but not in `rpc_requests_total`, whichever
  protocol the caller used, so the difference between the two series means the
  same thing on both stacks. The authentication middleware reports it, since it
  answers the request before either stack's instrumentation runs, and logs it
  with the keys and levels a refused call was logged with before.
- `AuthInfo.ClientID()` returns the client id claim of the caller's token,
  falling back to the authorized party, and is the value the `client_id` label
  on `rpc_protocol_responses_total` carries. It is safe to call on a nil
  `AuthInfo`, so an anonymous caller reports an empty client id.
- `rpc.AuthInfoInterceptor(required)` refuses a call that reaches a handler with
  no authenticated caller on its context, and `rpc.LogErrorResponse(ctx,
  logger, err)` logs an error response with the keys and levels both stacks
  use. `rpc.ResponseCode(err)`, `rpc.ResponseStatus(err, protocol)` and
  `rpc.ProtocolLabel(protocol)` are the label values the interceptors report, so
  a service that answers an RPC request in its own middleware can report it the
  same way.
- `APIServer.RegisterConnect(path, handler, opt)` mounts a Connect handler
  behind the same authentication middleware as the Twirp services, and
  `ServiceOptions.HandlerOptions()` is the Connect counterpart of
  `ServerOptions()`. `NewDefaultServiceOptions` fills in both, so a service that
  mounts both protocols gets logging, metrics, authentication and error
  behaviour parity by construction.
- `rpc.WithOutgoingHeaders(ctx, h)` and the `rpc.PropagateHeaders()` client
  interceptor replace `twirp.WithHTTPRequestHeaders` for the callers that set
  per-call headers.
- Documentation for driving the migration lives here: `docs/connect.md` is the
  fleet reference for serving, calling, testing and generating Connect,
  `docs/migration-service.md` the per-service playbook and
  `docs/migration-client.md` the per-client one. The master plan and the
  record of decisions stay in `CONNECT_MIGRATION.md` in elephant-repository.
- `test.IsRPCError(t, err, code)` accepts either error type, and
  `test.ErrorParity(t, twirpErr, connectErr)` asserts that the same call
  answered the two stacks with the same code, message and metadata, which is
  what makes a service's move to the Connect error vocabulary checkable.
- `cmd/protoc-gen-elephant-rpc` is a protobuf compiler plugin that keeps the
  plain service interface — `Get(ctx, *GetRequest) (*GetResponse, error)`, the
  one `protoc-gen-twirp` generates — available on top of Connect. For every
  service it emits `New<Service>ServiceHandler(svc, opts...) (string,
  http.Handler)` and `New<Service>ServiceClient(httpClient, baseURL, opts...)`
  into the `<pkg>connect` package `protoc-gen-connect-go` generates, so an
  implementation written against the plain interface is served over Connect,
  and a caller gets the same interface `New<Service>ProtobufClient` returns
  today. Errors are passed through untouched in both directions.
- The plugin's `interface=true` option also emits the plain interface itself,
  into the message package and with the name, method set, signatures and doc
  comments Twirp gives it, for the day Twirp generation is switched off. It is
  off by default. `package_suffix` mirrors the `protoc-gen-connect-go` option
  of the same name, and a streaming RPC fails generation with an error naming
  the method: only unary methods can have a plain interface.
- The plugin is run through buf as `go run
  github.com/ttab/elephantine/cmd/protoc-gen-elephant-rpc@<version>` at a
  version `github.com/ttab/mage` pins, so a repository that generates with it
  gains no dependency on elephantine. The code it emits imports only
  `connectrpc.com/connect`, `context`, `net/http` and the message package,
  which is what keeps `elephant-api` free of elephantine.
- `mage proto:generate` compiles the protobuf sources in this repository with
  buf and the plugin versions `github.com/ttab/mage/rpc` pins. `google.golang.org/protobuf`
  moves to v1.36.12 to match the `protoc-gen-go` the committed code is generated
  with. The generated code is a function of those pins and not of the machine
  that ran them: the generators are invoked under the pinned toolchain, and
  `protoc-gen-twirp` runs out of the module in `internal/protogen/twirpgen`
  rather than at a bare version, because it has no `go.mod` of its own and the
  gzipped descriptor it embeds changed with `compress/flate` between Go 1.26 and
  Go 1.27. `TestTwirpModulePins` fails when that module drifts from the fleet's
  pins.
- Dependency upgrades: `MicahParks/keyfunc` to v3.8.2, `urfave/cli` to v3.11.0,
  `github.com/ttab/mage` to the `rpc` namespace it generates with, and the
  Prometheus and `golang.org/x` support modules.
- The README gained a "Serving Connect and Twirp" section with the error helper
  table and the mount, and `docs/metrics.md` an "RPC metrics" section saying
  what a change in each of the four series means.

## [v0.28.0] - 2026-09-04

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
