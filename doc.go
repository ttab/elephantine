// Package elephantine holds the building blocks shared by elephant services:
// the API server and its RPC service options, authentication and JWT
// handling, structured logging, Prometheus metrics, feature flags, graceful
// shutdown and the supervised task group ErrGroup. The subpackages cover
// Postgres (pg), job locking (pg/joblock), the protocol-neutral RPC
// vocabulary (rpc) and test helpers (test).
//
// # Migrating from v0.29
//
// v0.30 breaks ErrGroup.GoWithRetries and joblock.Options: the failure budget
// is a duration (GiveUpAfter) rather than a count, the retry arguments moved
// into a RetryOptions struct, and the backoff curve is the library's own, so
// BackoffFunction and StaticBackoff are gone. See docs/migrations/v0.30.md in
// the repository for the old-to-new table and worked examples, and the field
// comments on RetryOptions for what replaced each argument.
package elephantine
