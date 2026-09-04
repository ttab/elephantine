# Elephantine

[![Go Reference](https://pkg.go.dev/badge/github.com/ttab/elephantine.svg)](https://pkg.go.dev/github.com/ttab/elephantine)

Shared functionality for Elephant systems. It's most likely not something anyone outside of Elephant would be interested in.

## What's in the box

### Root package

- **HTTP/API server** — production-ready server with graceful shutdown, TLS, CORS (with public, CDN-friendly path prefixes), request body limits, health/readiness probes, and pprof
- **JWT & OIDC** — JWT claims parsing, OIDC discovery, and OAuth2 client credentials
- **Twirp RPC** — logging hooks, Prometheus metrics, and auth middleware for Twirp services
- **HTTP client** — configurable client with timeouts, connection limits, oauth2 token injection, and Prometheus instrumentation
- **Graceful shutdown** — signal-based (SIGINT/SIGTERM) shutdown coordination
- **Error groups** — panic-recovering error groups with retry and backoff support, restarts are counted in the `task_restarts_total` metric
- **Prometheus helpers** — `MetricsHelper` for registering counters, gauges, and histograms, and `RegisterOrReuse` for metrics that are shared between components. Metric conventions for elephant services are documented in [docs/metrics.md](docs/metrics.md)
- **Feature flags** — context-based feature flag propagation
- **Vault** — HashiCorp Vault client with Kubernetes auth

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
- Test helpers for JWT auth, Twirp services, and structured logging

## CORS and request bodies

`APIServer` wraps the request mux in the CORS middleware and a request body
limit before handing it to the listeners, so both the plain and the TLS server
get the same treatment.

### CORS

Origins are checked against an allowlist: `Hosts` entries match a hostname
exactly or as a parent domain, `HostPatterns` entries are globs, and the scheme
must be `https` unless the host is `localhost`. The defaults allow `localhost`
and `tt.se`; `APIServerCORSHosts(...)` replaces the host list. An allowed origin
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
