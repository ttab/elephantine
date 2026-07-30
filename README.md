# Elephantine

[![Go Reference](https://pkg.go.dev/badge/github.com/ttab/elephantine.svg)](https://pkg.go.dev/github.com/ttab/elephantine)

Shared functionality for Elephant systems. It's most likely not something anyone outside of Elephant would be interested in.

## What's in the box

### Root package

- **HTTP/API server** — production-ready server with graceful shutdown, TLS, CORS, health/readiness probes, and pprof
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
- Distributed job locking via database rows, instrumented with the `pg_job_lock_held` and `pg_job_lock_transitions_total` metrics
- NOTIFY/LISTEN pub/sub with ping-based health checking, reconnection, and generic fan-out
- `PoolStatCollector` for exposing `pgxpool` connection pool statistics (saturation, acquire waits, connection churn) as Prometheus metrics
- Auto-generated query code via [sqlc](https://sqlc.dev/)

### `test/` — Test utilities

- `Must`/`MustNot` assertions and generic equality checks with diff output
- Golden file testing for JSON and protobuf
- Test helpers for JWT auth, Twirp services, and structured logging

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
