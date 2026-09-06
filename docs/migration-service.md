# Moving a service to Connect

The per-service playbook for the Twirp-to-Connect migration. Read
[connect.md](connect.md) first for how the pieces fit; this document is the
order of operations, what each step must contain, and how to know it is done.
Each step is one pull request. elephant-repository went first and is the
reference implementation for every identifier named here.

The policy that shapes the steps: services run dual-stack until their next
major release, because external customers have Twirp clients and a major
release is the contract under which they are asked to move. There is no hurry
and no traffic timer. New services launch Connect-only and skip this document.

## Before you start

Check the shared dependencies are released with what you need:

- `github.com/ttab/mage` with the `rpc` namespace (`mage rpc:generate`).
- `github.com/ttab/elephantine` with the `rpc` package, `RegisterConnect`,
  and `cmd/protoc-gen-elephant-rpc`, and a `ttab/mage` release that pins the
  plugin (until then, `ELEPHANT_RPC_PLUGIN=<elephantine checkout>` on every
  generation, and do not tag a release of your own generated code).
- If the service serves elephant-api services: an `elephant-api` release with
  the `<pkg>connect` packages.

Inventory the service:

```sh
grep -rl twitchtv/twirp --include='*.go' .        # every file that has to change by step 2
grep -rn 'twirp\.' --include='*.go' . | grep -v _test | wc -l
grep -rn 'WithHTTPRequestHeaders' --include='*.go' .   # client-side header injection
grep -rn '/twirp/' --include='*.md' --include='*.yaml' --include='*.json' .  # docs, ingress, dashboards
```

And find out whether anything in front of the service routes on the `/twirp/`
path prefix. If an ingress rule does, it needs a sibling rule for
`/<pkg>.<Service>/` before step 1 deploys, or the Connect paths are unreachable
from outside while the tests pass.

## Step 1: dual-stack, no handler changes

Goal: the service answers on both path families with identical behaviour, and
nothing in a handler has changed.

1. **Dependencies.** Bump `ttab/mage`, `elephantine` and, where relevant,
   `elephant-api`. Note that the elephantine bump may carry other behaviour
   changes (read its CHANGELOG lead-ins: request body cap, job lock package
   move, plaintext HTTP/2) and give each its own CHANGELOG paragraph in the
   service if it reaches a consumer.
2. **Generation**, for a service with its own protos: switch the magefile from
   `//mage:import twirp` to `//mage:import rpc` with `rpc.Twirp = true` in an
   `init`, vendor `newsdoc/newsdoc.proto` with `mage rpc:vendorProto` if the
   protos import it, delete `docs/*-openapi.json` and their links, run
   `mage rpc:generate`. Expect the first regeneration to rewrite every `.pb.go`
   (newer `protoc-gen-go`, `protoc (unknown)` in the header); verify the
   embedded descriptors are unchanged and `service.twirp.go` differs only in
   its gzipped descriptor blob.
3. **Mount.** With `elephantine.APIServer`: `NewDefaultServiceOptions` already
   fills both sides, so for every service add
   `path, h := <pkg>connect.New<Service>ServiceHandler(svc, opt.HandlerOptions()...)`
   and `server.RegisterConnect(path, h, opt)` next to the existing
   `RegisterAPI`. With a custom router: mount the returned path as a `POST`
   catch-all behind the same authentication middleware as the Twirp routes,
   build one Connect interceptor chain (`rpc.MetricsInterceptor(reg)`,
   `rpc.LoggingInterceptor(logger)`, `rpc.LegacyTwirpErrors()`), and add
   `twirp.WithServerInterceptors(rpc.TwirpInterceptor())` to the Twirp server
   options. `rpc.LegacyTwirpErrors()` is the transitional piece: it translates
   the Twirp errors the handlers still return so the Connect caller sees the
   right code. Check that the service starts without a duplicate metric
   registration panic; the collectors are shared through elephantine, but a
   service that registered its own copies has to stop.
4. **CORS.** If the service configures allowed headers itself, add
   `Connect-Protocol-Version` and `Connect-Timeout-Ms`.
5. **Tests.** Make the API test suite run against both stacks: a switch in the
   test context that builds either the Twirp protobuf client or the
   `New<Service>ServiceClient` adapter with the bearer token attached the same
   way, chosen by an environment variable (`TEST_RPC_STACK=twirp|connect`),
   with a per-test override. Replace `test.IsTwirpError` with
   `test.IsRPCError`. Run the suite both ways and fix what the Connect run
   surfaces. Add the second run to CI.
6. **Documentation, same PR.** Architecture or API documentation: both path
   families, the protocols served (Connect, gRPC, gRPC-Web on the Connect
   paths), the error body shapes and `ErrorMeta`, the three status
   differences. Permissions documentation: the Connect paths sit behind the
   same middleware and scope checks. Observability documentation:
   `rpc_protocol_responses_total` and what a change in it means, and the
   `status` label shift for `failed_precondition`. Anywhere `/twirp/` is
   named as the API. The project CLAUDE.md, if it states API facts.
7. **CHANGELOG.** A lead-in paragraph at the outermost tier, since this is new
   API surface: the Connect paths, the protocols, the error body, the three
   status differences, and that Twirp is unchanged. Then the elephantine
   behaviour changes that reach consumers, then bullets.

Done when: build, vet and lint are clean; `go test ./...` and
`TEST_RPC_STACK=connect go test ./...` both pass; the Connect paths are
reachable in the deployed environment (curl one method with a JSON body); and
`rpc_protocol_responses_total{protocol="connect"}` moves when they are called.

## Step 2: the error flip

Goal: no handler constructs a Twirp error. This is the mechanical part, and the
[table in connect.md](connect.md#errors-in-handlers) is the mapping. Apply it
completely:

- Every `twirp.*` error construction becomes the matching `rpc.*` helper, every
  `elephantine.IsTwirpErrorCode` becomes `rpc.IsCode`, every
  `elephantine.RequireAnyScope` (or the service's own copy) returns `rpc`
  errors, every `test.IsTwirpError` becomes `test.IsRPCError`. Anything that
  used `string(twirp.<Code>)` as a code string uses
  `connect.Code<X>.String()`, which spells the same.
- Preserve every message and every meta key exactly. The parity tests below
  are what prove it; the meta keys are part of the contract with clients.
- Rewrite every bare `fmt.Errorf` return in a handler to a coded error, since
  Twirp answered those `internal` and Connect would answer `unknown`. Server
  faults become `rpc.Internalf(...)`; while you are at each site, a parse
  failure of a caller-supplied value becomes `rpc.InvalidArgument`, which is a
  visible code change to record in the CHANGELOG. Do not add an interceptor
  that codes errors on the way out: fewer moving parts, and the handler is the
  one place that knows the right code. Helpers that return plain errors are
  fine as long as every handler that calls them wraps the result in a coded
  error.
- Remove `rpc.LegacyTwirpErrors()` from the Connect chain.
- Acceptance grep: `grep -rl twitchtv/twirp --include='*.go' .` lists only the
  Twirp mount (the file that calls `New<Service>Server`) and the test client
  construction. Every other importer is a defect.
- Parity tests: for the error paths the suite already exercises (missing
  scope, not found, invalid argument with `argument` meta, whatever the
  service's characteristic `failed_precondition` is, anything with numbered
  meta keys), perform the same call on both stacks and assert
  `test.ErrorParity`. Add golden files for the raw JSON error bodies of two of
  them on each stack.
- CHANGELOG: a line under the step 1 entry saying error parity is tested. There
  is no consumer-visible change beyond what step 1 recorded.

Done when: the grep is clean, parity tests pass, both test runs pass, lint is
clean.

## Between step 2 and step 3

This is the long part. While the service is dual-stack:

- Watch `rpc_protocol_responses_total{protocol="twirp"}` per method. It tells
  you which methods still have Twirp callers and roughly how many.
- Move the platform's own clients off Twirp with
  [migration-client.md](migration-client.md), starting with the ones that call
  this service. Internal traffic on Twirp should reach zero long before the
  major release.
- Identify external callers from the remaining Twirp traffic and the
  `customer` label, and tell them before the major release which version
  removes `/twirp/`.
- New RPCs added during this period get both mounts automatically (the mount
  code registers a whole service, not a method) and are written with `rpc`
  errors from the start.

## Step 3: retire Twirp

In the service's next major release:

1. Delete the Twirp mount and its interceptor, the Twirp test client
   construction and the stack switch (Connect is the only stack now).
2. Set `rpc.Twirp = false` (or delete the `init`), which makes
   `protoc-gen-elephant-rpc` emit the plain interface into `service.rpc.go`
   instead of `protoc-gen-twirp` emitting it in `service.twirp.go`; run
   `mage rpc:generate`; delete `service.twirp.go`. Implementations compile
   unchanged: same interface name, same methods.
3. `go mod tidy`: `twitchtv/twirp` leaves `go.mod`.
4. Remove the `/twirp/` ingress rules and any dashboard panels keyed on them.
5. Documentation: remove the Twirp column everywhere; the Connect paths are
   the API.
6. CHANGELOG: `**Breaking:**` lead-in, `/twirp/` paths removed, Connect paths
   are the API, the version that removed them.

For a service whose protos live in elephant-api rather than its own repository,
step 2 of this list is elephant-api's to do when the last of its services
retires Twirp; until then the interface stays in `service.twirp.go` and the
service just stops mounting it.
