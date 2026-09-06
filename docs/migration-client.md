# Moving a client to Connect

The per-client playbook for the Twirp-to-Connect migration. A client can move
as soon as the service it calls serves Connect (step 1 of
[migration-service.md](migration-service.md) is deployed), and the platform's
own clients should all have moved well before any service retires Twirp.
[connect.md](connect.md) has the wire facts.

## Go

Three substitutions, all local:

1. **Constructor.**
   `<pkg>.New<Service>ProtobufClient(url, httpClient, opts...)` becomes
   `<pkg>connect.New<Service>ServiceClient(httpClient, url)`. The return type is
   the same plain interface, so nothing downstream changes. Two things to check
   at the call site: the argument order is `(httpClient, url)`, and `url` is the
   server root (`https://repository.api.tt.se`), not a path.
2. **Error checks.** `elephantine.IsTwirpErrorCode(err, twirp.NotFound)` becomes
   `rpc.IsCode(err, connect.CodeNotFound)`. `rpc.IsCode` recognises both error
   types, so this substitution can land before the constructor swap. Code
   reading `twirp.Error.MetaMap()` reads `rpc.Meta(err)` instead. A retry loop
   that keyed on Twirp's `internal` for transport failures should key on
   `connect.CodeUnavailable` and `connect.CodeUnknown` as well: Connect
   classifies a failed dial as `unavailable`, not `internal`.
3. **Headers.** `twirp.WithHTTPRequestHeaders(ctx, h)` becomes
   `rpc.WithOutgoingHeaders(ctx, h)` on the context plus
   `connect.WithInterceptors(rpc.PropagateHeaders())` as a client option on the
   constructor. Most clients never did this; the bearer token travels in the
   `http.Client` and needs no change.

Then `go mod tidy`. If `twitchtv/twirp` is still in `go.mod`, name the remaining
importer in the pull request; usually it is a test helper or a transitive
dependency that goes away with the next elephant-api bump.

Tools built on `clitools` get the Connect constructors when `clitools` does, so
a tool's own change is the bump plus the error checks. Until then a tool can
construct the Connect client itself with the `http.Client` `clitools` hands it.

Done when: the calls succeed against a deployed service (the paths are
different, so a wrong ingress rule shows up here as `404`), error handling
tests pass, and `twitchtv/twirp` is gone from `go.mod` or accounted for.

## TypeScript (elephant-chrome and the `@ttab/elephant-api` package)

`@ttab/elephant-api` is generated in `elephant-api-npm` from copies of the
`.proto` files. It gains a second generation path with `@bufbuild/protoc-gen-es`
and `@connectrpc/connect-web`, published under a separate entry point so the
browser clients can move one shared client file at a time. `elephantine`'s
`rpc/errormeta.proto` is copied into the package like the other protos so the
`ErrorMeta` message type exists on the TypeScript side.

Per shared client file (`shared/Repository.ts`, `shared/Index.ts`, ...):

1. Replace `TwirpFetchTransport({ baseUrl: <repo>/twirp })` with
   `createConnectTransport({ baseUrl: <repo> })` from `@connectrpc/connect-web`,
   and the client types with the ones from the Connect entry point.
2. Error handling: a Twirp error was `{ code, msg, meta }`; a `ConnectError`
   has `code`, `message`, and `findDetails(ErrorMeta)` for the metadata. Hide
   that behind one `errorMeta(err)` helper in `shared/` so view code reads meta
   the same way it did.
3. The lock-conflict UI, which reads `lock_holder_sub` and its siblings, is the
   one to test by hand: it is the heaviest user of error metadata.

## Raw `fetch` and `curl` callers

Change the path prefix, rename the error field, and read meta from the detail:

| | Twirp | Connect |
|---|---|---|
| URL | `POST <base>/twirp/<pkg>.<Service>/<Method>` | `POST <base>/<pkg>.<Service>/<Method>` |
| Headers | `Content-Type: application/json` | `Content-Type: application/json` (`Connect-Protocol-Version: 1` optional) |
| Error body | `{"code","msg","meta"}` | `{"code","message","details":[{"type":"elephantine.rpc.ErrorMeta","value":"<base64>","debug":{"meta":{…}}}]}` |
| Status | as before | `failed_precondition` is 400 not 412; read the code from the body |

The request body is unchanged: protobuf JSON is protobuf JSON on both. A caller
that inspected HTTP status codes instead of the body's `code` has to switch to
the code; it is the same string on both stacks.

## What not to do

- Do not add a `/twirp/`-style prefix to Connect URLs, and do not configure a
  client path prefix. The unprefixed root is the standard and what every
  Connect runtime assumes.
- Do not require `Connect-Protocol-Version` anywhere, so plain HTTP callers
  keep working.
- Do not write a wrapper that fetches a token and passes it in. The token
  belongs in the `http.Client`, exactly as with Twirp; a Go tool that seems to
  need a wrapper is missing its `clitools` wiring.
