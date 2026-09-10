# Elephantine

[![Go Reference](https://pkg.go.dev/badge/github.com/ttab/elephantine.svg)](https://pkg.go.dev/github.com/ttab/elephantine)

Shared functionality for Elephant systems. It's most likely not something anyone outside of Elephant would be interested in.

## What's in the box

### Root package

- **HTTP/API server** — production-ready server with graceful shutdown, TLS, CORS (with public, CDN-friendly path prefixes), request body limits, health/readiness probes, and pprof
- **JWT & OIDC** — JWT claims parsing, OIDC discovery, and OAuth2 client credentials
- **RPC** — protocol-neutral authentication middleware, and the Twirp hooks and Connect interceptors that give a service the same logging, metrics and error behaviour on both stacks. See [Serving Connect and Twirp](#serving-connect-and-twirp) below, and [docs/connect.md](docs/connect.md) for the fleet-wide reference
- **HTTP client** — configurable client with timeouts, connection limits, oauth2 token injection, and Prometheus instrumentation
- **Graceful shutdown** — signal-based (SIGINT/SIGTERM) shutdown coordination
- **Error groups** — panic-recovering error groups with retry and backoff support, restarts are counted in the `task_restarts_total` metric
- **Prometheus helpers** — `MetricsHelper` for registering counters, gauges, and histograms, and `RegisterOrReuse` for metrics that are shared between components. Metric conventions for elephant services are documented in [docs/metrics.md](docs/metrics.md)
- **Feature flags** — context-based feature flag propagation
- **Vault** — HashiCorp Vault client with Kubernetes auth

### `rpc/` — RPC errors and interceptors

- The error vocabulary every elephant service returns: `rpc.NotFound`, `rpc.InvalidArgument`, `rpc.FailedPreconditionf` and the rest, producing `*connect.Error` with the metadata Twirp carried in its meta map
- `rpc.ToTwirp`/`rpc.FromTwirp` and the interceptors that install them, so one implementation answers both protocols identically
- `rpc.RequireAnyScope`, `rpc.LoggingInterceptor`, `rpc.MetricsInterceptor`, `rpc.WithOutgoingHeaders`/`rpc.PropagateHeaders`

### `pg/` — PostgreSQL

- Type conversion helpers for `pgtype` (`Text`, `Int32`, `UUID`, `Time`, and nullable pointer variants)
- Transaction helpers (`WithTX`, `Rollback`)
- NOTIFY/LISTEN pub/sub with ping-based health checking, reconnection, and generic fan-out
- `PoolStatCollector` for exposing `pgxpool` connection pool statistics (saturation, acquire waits, connection churn) as Prometheus metrics

### `pg/joblock/` — Job locks

- Distributed job locking via a `job_lock` table row, instrumented with the `pg_job_lock_held`, `pg_job_lock_transitions_total` and `pg_job_lock_restarts_total` metrics. `joblock.Run` supervises a worker that must run on one instance at a time; see [docs/joblock-restart-semantics.md](docs/joblock-restart-semantics.md)
- The table is created by the tern migration in `pg/joblock/schema`, which a service vendors into its own `./schema` (see below). `pg/joblock/schema.sql` is generated from it for sqlc and must not be edited

### `test/` — Test utilities

- `Must`/`MustNot` assertions and generic equality checks with diff output
- Golden file testing for JSON and protobuf
- Test helpers for JWT auth, RPC errors (`IsRPCError`, `ErrorParity`) and structured logging

### `cmd/protoc-gen-elephant-rpc` — Connect adapters

- A protobuf compiler plugin that generates the adapters that let a service keep the plain interface Twirp gives it while serving Connect, and the interface itself once Twirp generation stops. See [Generating the RPC adapters](#generating-the-rpc-adapters)
- Only unary RPCs. A native Connect service skips the adapters and implements connect-go's own handler interface, which is what lets it declare streaming methods — see [docs/connect.md](docs/connect.md)

## Generating the RPC adapters

`protoc-gen-elephant-rpc` keeps the plain service interface —
`Get(ctx, *GetRequest) (*GetResponse, error)`, the one `protoc-gen-twirp`
generates — as the contract a service implements and a client is handed, with
Connect underneath. It is run through buf, at a version `github.com/ttab/mage`
pins, alongside `protoc-gen-go` and `protoc-gen-connect-go`:

```json
{
  "version": "v2",
  "plugins": [
    {"local": ["go", "run", "github.com/ttab/elephantine/cmd/protoc-gen-elephant-rpc@<version>"], "out": "."}
  ]
}
```

`<version>` is not written out by hand: `github.com/ttab/mage/rpc` pins it and
passes the template to buf, so a bump of `ttab/mage` is what moves the
generator, exactly as an image tag was before. The template above is what that
bump produces.

For each service in a file it writes `<proto base>.elephant.go` into the
`<pkg>connect` package `protoc-gen-connect-go` generates, next to
`<proto base>.connect.go`, holding two constructors:

- `New<Service>ServiceHandler(svc <pkg>.<Service>, opts ...connect.HandlerOption) (string, http.Handler)` serves an implementation of the plain interface over Connect, and returns the path to mount it on together with the handler, like `New<Service>Handler` does
- `New<Service>ServiceClient(httpClient connect.HTTPClient, baseURL string, opts ...connect.ClientOption) <pkg>.<Service>` is a client with that same plain interface, so it is a drop-in for `New<Service>ProtobufClient`

Errors pass through both adapters untouched, so an implementation that wants to
control the response code returns a `*connect.Error`.

The options, given as `opt` entries in the buf template:

| Option | Default | Effect |
|---|---|---|
| `package_suffix` | `connect` | The suffix `protoc-gen-connect-go` generates with. The adapters go into the same package, so the two have to agree |
| `interface` | `false` | Also emit the plain interface itself into the message package, as `<proto base>.rpc.go`. Turn it on the day `protoc-gen-twirp` stops generating it: the name, method set, signatures and doc comments are the ones Twirp emits, so implementations compile unchanged |

Only unary RPCs are supported — a streaming method fails generation with an
error naming it, since a stream has no place in the plain interface.

The generated code imports `connectrpc.com/connect`, `context`, `net/http` and
the message package, and nothing else. It never imports elephantine, so a
declarations module like `elephant-api` can generate with this plugin without
taking on elephantine's dependencies; header propagation on the client side
comes from an interceptor the caller passes in rather than from the generated
client.

`cmd/protoc-gen-elephant-rpc/testdata` holds a fixture proto generated with
both settings of `interface`, committed and compared by
`go test ./cmd/protoc-gen-elephant-rpc`. Run that test with `REGENERATE=true`
after changing the plugin, and note that the generated fixture packages are
compiled by the test rather than by `go build ./...`, since the go command
skips `testdata`.

## Serving Connect and Twirp

This section is the API tour. [docs/connect.md](docs/connect.md) is the fleet
reference for how Connect is served, called, tested and generated, and the
migration playbooks are [docs/migration-service.md](docs/migration-service.md)
(moving a service, step by step) and
[docs/migration-client.md](docs/migration-client.md) (moving a Go, TypeScript
or raw HTTP client).

A service implements the plain protobuf interface — `Get(ctx, *GetRequest)
(*GetResponse, error)` — once, and mounts it on both protocols. Twirp serves
`/twirp/<pkg>.<Service>/<Method>`, Connect serves `/<pkg>.<Service>/<Method>`
plus gRPC and gRPC-Web on the same path, so the two mounts never collide. gRPC
and gRPC-Web are reachable inside the cluster only: the fleet's ingress speaks
HTTP/1.1 to its targets and external gRPC access is deliberately not provided.

```go
opt, err := elephantine.NewDefaultServiceOptions(
    logger, authParser, reg, elephantine.ServiceAuthRequired)
if err != nil {
    return fmt.Errorf("set up service options: %w", err)
}

server.RegisterAPI(repository.NewDocumentsServer(
    svc, opt.ServerOptions()), opt)

path, handler := repositoryconnect.NewDocumentsServiceHandler(
    svc, opt.HandlerOptions()...)

server.RegisterConnect(path, handler, opt)
```

The plaintext listener serves HTTP/1.1 and HTTP/2 side by side, told apart by
the HTTP/2 connection preface, which is what makes gRPC reachable without TLS
from inside the cluster: Go negotiates HTTP/2 through the TLS ALPN handshake and
nowhere else, so a listener that does not say so answers HTTP/1.1 only and a
gRPC client cannot connect to it at all. It does not make gRPC reachable from
outside; the ingress speaks HTTP/1.1 to its targets and no gRPC target group is
provided.

A service that serves its RPCs from an `http.Server` of its own says the same
thing with `Protocols: elephantine.PlaintextProtocols()`.

`NewDefaultServiceOptions` fills in both stacks, so a service gets logging,
metrics and authentication parity by construction. A service whose handlers
still return Twirp errors adds `rpc.LegacyTwirpErrors()` to
`opt.Interceptors`, and removes it in the change that moves the handlers to
the helpers below.

### Errors

`*connect.Error` is the neutral error type, and the Twirp mount translates on
the way out. Error metadata travels as an `elephantine.rpc.ErrorMeta` detail,
declared in `rpc/errormeta.proto`, because Connect has no free-form meta map in
the response body; the Twirp translation flattens it back into one.

| Twirp | `rpc` |
|---|---|
| `twirp.NewErrorf(code, ...)` | `rpc.Errorf(code, ...)` |
| `twirp.InvalidArgumentError(arg, msg)` | `rpc.InvalidArgument(arg, msg)` |
| `elephantine.InvalidArgumentf(arg, ...)` | `rpc.InvalidArgumentf(arg, ...)` |
| `twirp.RequiredArgumentError(arg)` | `rpc.RequiredArgument(arg)` |
| `twirp.NotFoundError(msg)` | `rpc.NotFound(msg)` |
| `twirp.InternalErrorf(...)` | `rpc.Internalf(...)` |
| `twirp.FailedPrecondition.Errorf(...)` | `rpc.FailedPreconditionf(...)` |
| `twirp.PermissionDenied.Errorf(...)` | `rpc.PermissionDeniedf(...)` |
| `twirp.Unauthenticated.Error(msg)` | `rpc.Unauthenticated(msg)` |
| `twirp.AlreadyExists.Error(msg)` | `rpc.AlreadyExists(msg)` |
| `err.WithMeta(k, v)` | `rpc.WithMeta(err, k, v)` |
| `err.MetaMap()` | `rpc.Meta(err)` |
| `elephantine.IsTwirpErrorCode(err, code)` | `rpc.IsCode(err, code)` |
| `elephantine.RequireAnyScope(ctx, ...)` | `rpc.RequireAnyScope(ctx, ...)` |
| `test.IsTwirpError(t, err, code)` | `test.IsRPCError(t, err, code)` |

`rpc.IsCode` accepts both error types, so a caller can move its checks before
it moves its client constructor. `test.ErrorParity(t, twirpErr, connectErr)`
asserts that the same call answered the two stacks with the same code, message
and metadata, which is what makes a service's move checkable.

The codes are identical, and so are the HTTP statuses except three:
`canceled` is `499` rather than `408`, `deadline_exceeded` is `504` rather than
`408`, and `failed_precondition` is `400` rather than `412`. Anything keyed on
`412` for lock conflicts reads `rpc_protocol_responses_total{code=...}`
instead.

### Authentication

Authentication is protocol-neutral HTTP middleware, and it fails closed.
`SetAuthInfoValidation` parses the `Authorization` header and puts the
`AuthInfo` on the request context, where both stacks read it with
`GetAuthInfo`, and answers a request it could not authenticate itself, before
the request reaches a handler: `connect.NewErrorWriter` renders the Connect,
gRPC and gRPC-Web error bodies and `twirp.WriteError` the Twirp one. A missing
authorization and an invalid one are both `unauthenticated` (401);
`permission_denied` is for a caller we identified and that is not allowed to
make the call. `ServiceAuthOptional` lets a missing authorization through as an
anonymous caller; an invalid one always fails.

The Twirp hook and the `rpc.AuthInfoInterceptor` the same call installs are the
safety net for a mount that does not run the middleware: they refuse a call
that reaches a handler with no authenticated caller on its context. Every
interceptor in `rpc` wraps streaming handlers and streaming clients as well as
unary calls, since `connect.UnaryInterceptorFunc` would pass a stream through
unauthenticated and uncounted.

### Client headers

A caller that needs per-call headers, which `twirp.WithHTTPRequestHeaders`
gave it, sets them on the context and adds one client interceptor:

```go
client := repositoryconnect.NewDocumentsServiceClient(
    httpClient, endpoint,
    connect.WithInterceptors(rpc.PropagateHeaders()))

ctx = rpc.WithOutgoingHeaders(ctx, http.Header{
    "X-Forwarded-For": []string{addr},
})
```

### Generating the protobuf in this repository

`mage proto:generate` compiles `rpc/errormeta.proto` and the fixture service in
`internal/testservice`, with buf and the plugin versions
`github.com/ttab/mage/rpc` pins. `rpc:generate` in ttab/mage cannot be used
here, since it discovers services as `<proto root>/*/service.proto` and neither
source is laid out that way.

## CORS and request bodies

`APIServer` wraps the request mux in the CORS middleware and a request body
limit before handing it to the listeners, so both the plain and the TLS server
get the same treatment.

### CORS

Origins are checked against an allowlist: `Hosts` entries match a hostname
exactly or as a parent domain, `HostPatterns` entries are globs, and the scheme
must be `https` unless the host is `localhost`. The defaults allow `localhost`
and `tt.se`; `APIServerCORSHosts(...)` replaces the host list. The default
allowed headers are `Authorization`, `Content-Type`, `Connect-Protocol-Version`
and `Connect-Timeout-Ms`, the last two because a browser Connect client sends
them on every call. An allowed origin
is echoed back in `Access-Control-Allow-Origin` together with `Vary: Origin`.

An anonymous read surface served through a CDN wants the opposite of that: the
response is the same for everyone, and varying on `Origin` gives the CDN one
cache entry per embedding site. Mark such paths public:

```go
srv := elephantine.NewAPIServer(logger, addr, profileAddr,
    elephantine.APIServerPublicCORS("/public/"),
)
```

A request whose path starts with a public prefix is answered with
`Access-Control-Allow-Origin: *` and no `Vary` header, and its preflight is
answered with a `204` whatever the `Origin` header says. Everything outside the
prefixes keeps the allowlist behaviour unchanged. Prefixes are matched
literally, so pass `"/public/"` rather than `"/public"`.

Do not mark a path that reads the caller's `Authorization` header or cookies as
public. The browser will not send credentials to a wildcard origin, but a shared
cache in front of the service is still free to hand one caller's response to
another.

### Request body limit

Request bodies are capped at `DefaultMaxBodyBytes` (8 MiB). A request that
declares a larger `Content-Length` is refused with `413` before it reaches a
handler; a body of unknown length fails when the handler reads past the limit.
The cap matters because Twirp buffers the whole body in memory before
unmarshalling it, so the limit is what a single caller can make a replica hold.

Override it per service, and only turn it off (`0` or less) for a listener that
has to take genuinely large uploads:

```go
srv := elephantine.NewAPIServer(logger, addr, profileAddr,
    elephantine.APIServerMaxBodyBytes(64<<20),
)
```

## Reporting the application version

`APIServer` exposes two build-info endpoints:

- `GET /version` on the public API server — JSON summary with the application name, version, VCS stamp, and a curated module list (defaults: `github.com/ttab/elephantine`, `github.com/ttab/elephant-api`, `github.com/ttab/elephant-tt-api`). Pass `APIServerModules(...)` to report additional modules.
- `GET /debug/bom` on the health/metrics server — the full `debug.BuildInfo` in the canonical `go version -m` format, for SBOM/forensic use. The health server must stay internal.

### Setting the application version

Our services are built as Docker images triggered by git tags. Wire the tag through the pipeline in three places.

**1. Service `main` package** — declare a package-level `version` variable and pass it to `NewAPIServer`:

```go
package main

var version string // set via -ldflags at build time

func main() {
    // ...
    srv := elephantine.NewAPIServer(logger, addr, profileAddr,
        elephantine.APIServerVersion(version),
    )
}
```

**2. `Dockerfile`** — accept a `VERSION` build-arg and pass it to `go build` via `-ldflags`:

```dockerfile
ARG TARGETOS TARGETARCH
ARG VERSION=v0.0.0-dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build \
      -ldflags "-X main.version=$VERSION" \
      -o /build/myservice ./cmd/myservice
```

**3. `.github/workflows/build.yaml`** — forward the git tag (`github.ref_name`) to the build-arg:

```yaml
- name: Build and push release image
  uses: docker/build-push-action@v7
  with:
    context: .
    platforms: linux/amd64,linux/arm64
    push: true
    tags: ghcr.io/${{ github.repository }}:${{ github.ref_name }}
    build-args: |
      VERSION=${{ github.ref_name }}
    cache-from: type=gha
    cache-to: type=gha,mode=max
```

The workflow already triggers on `v*` tags, so `github.ref_name` is the tag (`v1.2.3`).

If `APIServerVersion` is not set, or if the binary is built locally without the ldflag, the endpoint reports `v0.0.0-dev`.

VCS revision, timestamp, and dirty state are stamped automatically by the Go toolchain (`-buildvcs=auto`, default) as long as `.git` is present in the build context — which it is with the standard `ADD . ./` step. Dependency versions come from the build graph with no extra flags.

## Vendoring the job lock migration

Neither `mage sql:migrate` nor elephant-platform's `setup db migrate` looks
inside a dependency for a migration, so a service that uses `pg/joblock` has to
carry the `job_lock` table in its own `./schema`. Declare the library once and
let mage copy the migration in:

```shell
mage sql:vendorAdd github.com/ttab/elephantine pg/joblock/schema
mage sql:vendor
mage sql:migrate
```

`mage sql:vendorCheck` fails when elephantine ships a migration the service has
not vendored yet, so wire it into lint or a test. A service that created the
table by hand before the migration existed asserts that in the migration that
did the work instead of vendoring:

```sql
-- covers: github.com/ttab/elephantine pg/joblock/schema/001_job_lock.sql
```

Each feature that needs a table of its own gets its own schema directory under
its package, so a service only ever vendors the migrations for the features it
uses. The path is part of the contract: a service that vendored the job lock
from the old `pg/schema` location updates the `dir` in `schema/vendor.json` and
the `-- vendored-from:` or `-- covers:` line in its migration to
`pg/joblock/schema/001_job_lock.sql`. The file itself is unchanged, so its
checksum and applied state are unaffected.

Adding a migration to the library:

```shell
mage sql:librarySchema pg/joblock/schema pg/joblock/schema.sql
mage sql:generate
```

The first regenerates the flat schema sqlc reads, the second the query code in
`pg/joblock/internal/postgres`. `pg/joblock/schema_test.go` fails if the flat
schema is left behind.
