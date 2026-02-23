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
- **Error groups** — panic-recovering error groups with retry and backoff support
- **Prometheus helpers** — `MetricsHelper` for registering counters, gauges, and histograms
- **Feature flags** — context-based feature flag propagation
- **Vault** — HashiCorp Vault client with Kubernetes auth

### `pg/` — PostgreSQL

- Type conversion helpers for `pgtype` (`Text`, `Int32`, `UUID`, `Time`, and nullable pointer variants)
- Transaction helpers (`WithTX`, `Rollback`)
- Distributed job locking via database rows
- NOTIFY/LISTEN pub/sub with ping-based health checking, reconnection, and generic fan-out
- Auto-generated query code via [sqlc](https://sqlc.dev/)

### `test/` — Test utilities

- `Must`/`MustNot` assertions and generic equality checks with diff output
- Golden file testing for JSON and protobuf
- Test helpers for JWT auth, Twirp services, and structured logging
