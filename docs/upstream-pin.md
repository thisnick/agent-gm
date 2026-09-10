# Upstream pins

Serves spec §3.6. Every dependency Agent GM pins by an exact revision is
recorded here, one row per pin, plus the entry each bump adds. The bump
procedures are in [operations.md](operations.md#the-libgm-pin-policy); this
page is the record they write to.

## The pins today

| Dependency | Pinned | Why this one | Bumped on |
|---|---|---|---|
| `go.mau.fi/mautrix-gmessages` (`pkg/libgm`) | `b0d61b4e1a4e94f0d5e6fedd43cadb80bd0a9e51` (`b0d61b4`), `ConfigVersion` 2026.9.2 (unchanged) | The Google Messages client library. Its `ConfigVersion` must be current enough for Google to accept conversation creation | pending live gate, see Bumps below |
| `github.com/modelcontextprotocol/go-sdk` | `v1.7.0` | The MCP protocol layer. It decides the JSON-RPC framing, the `WWW-Authenticate` challenge on `/mcp` and the RFC 9728 handler | initial pin, `v1.0.0` |
| Node | `22` (`devbox.json`, and `actions/setup-node` in `release.yml`) | Runs the `@agent-gm/cli` postinstall shim and packs the npm tarball | initial pin, `v1.0.0` |
| npm | `>= 11.5.1`, installed into a scratch prefix by `release.yml` and passed to `scripts/release.sh` as `AGENT_GM_NPM` | Trusted publishing (OIDC) is not implemented before 11.5.1: an older npm publishes unauthenticated and the registry answers `404`. Node 22 ships npm 10 | `v1.0.1` |

The `libgm` pin is the only one recorded in three places — `go.mod`,
`internal/gm/pin.go` and spec §3.6 — and `devbox run pin-consistency` fails if
they diverge. `GOFLAGS=-mod=readonly` is set so an accidental `go get` cannot
move any of them.

## Bumping a pin

For libgm, begin with the [maintained patch and reapplication guide](../third_party/mautrix-gmessages/AGENT_GM_PATCHES.md).
The local `replace` means the imported source must be updated as well as the
recorded hashes. The guide distinguishes direct library tests, adapter tests,
live validation, and known gaps; verify reconstruction against each new base.

The full procedures, with the commands, are in
[operations.md](operations.md#the-libgm-pin-policy) (`libgm`) and
[operations.md](operations.md#the-go-sdk-pin) (`go-sdk`). In outline:

- **`libgm`** — update all three places in one commit; diff `pkg/libgm` and
  `pkg/connector` between the old and new commits; `devbox run
  fixture-validation`; `devbox run check && devbox run conformance`; then a
  **live gate** run by the maintainer by hand — pair, list, send one text,
  receive a reply.
- **`go-sdk`** — update `go.mod`; `devbox run check`; then `devbox run
  conformance`, which is the gate, checked against
  `scripts/mcp-conformance-baseline.yaml` in both directions. No live gate:
  nothing in the SDK touches Google.
- **Node or npm** — `devbox run release-dry-run`, which packs the tarball and
  installs it for real, is the whole gate.

A bump is its own change, never a drive-by commit, and never part of a routine
upgrade.

## What a bump entry records

Add a section below on every bump, so the next reader can see what moved
without cloning anything:

```markdown
## <dependency> <old> → <new> (<release>)

- **`ConfigVersion`**: <before> → <after>.               (libgm only)
- **Symbols in spec §3.1 that changed**: <each one, or "none">.
- **Delivery-state and event vocabulary**: every added, removed or renamed
  enum value, or "none".
- **Wire behaviour that changed**: status codes, headers, error codes.
- **Gate**: what was run and what it did — for `libgm`, the live gate, with
  what was sent and what came back.
```

A bump with no entry here is a bump nobody can review later.

## Bumps

## `libgm` be48a58 → b0d61b4 (unreleased)

- **`ConfigVersion`**: 2026.9.2 → 2026.9.2 (unchanged; not urgent under D3).
- **Symbols in spec §3.1 that changed**:
  - `client.go`: `NewClient(authData, pk, logger)` gained a fourth parameter,
    `httpSettings exhttp.ClientSettings`; it replaces the hardcoded transport
    and the removed `SetProxy` method (proxying is now configured through
    `exhttp.ClientSettings.WithProxy` before the client is built). Agent GM's
    two call sites (`internal/gm/libgm.go`) now pass
    `exhttp.SensibleClientSettings`.
  - `client.go`: `Connect`, `ConnectBackground` and `Reconnect` all gained a
    leading `ctx context.Context` parameter, threaded down into
    `startLongPolling`/`doLongPoll`. Agent GM's callers already had a context
    in scope at every call site and now pass it through.
  - `pair_google.go`: `StartGaiaPairing(ctx)` became
    `StartGaiaPairing(ctx, bgCtx context.Context)` — a second, longer-lived
    context for the pairing long-poll goroutine, separate from the
    per-request `ctx`. `DoGaiaPairing` (Agent GM's only entry point) passes
    `ctx` for both and is otherwise unchanged.
  - `pair.go`: `StartLogin()` became `StartLogin(ctx context.Context)`. Dead
    code for Agent GM (D19: QR pairing is withdrawn and unused), updated for
    compilation only.
  - No symbol was removed from spec §3.1 except `SetProxy`, which Agent GM
    never called.
- **Delivery-state and event vocabulary**: none. No `.proto` file changed
  between the two commits.
- **Wire behaviour that changed**: none observed. The two commits that touch
  `pkg/libgm/longpoll.go` (`e00c4031`, `2acd3449`) add a per-request
  `context.CancelFunc` and a one-minute read-timeout cancellation for the
  *foreground* (non-background) long-poll path — a case Agent GM's own
  `Connect` uses but the maintained patch does not touch. They do not change
  status codes, headers, or the wire payloads.
- **Interaction with the maintained patch**
  (`third_party/mautrix-gmessages/AGENT_GM_PATCHES.md`): upstream's own
  `e00c40312938...` commit threads `context.Context` through `doLongPoll` and
  the `dittoPinger`, which is the same intent as the patch's former
  `doLongPollContext` split and its ad hoc `ctx`/`reconnectCtx` fields on
  `dittoPinger` — upstream now carries both natively, so that part of the
  patch is retired rather than re-applied. What the patch still adds on top,
  unchanged in effect: `RunBackground`/`waitBackgroundRetry`/`backgroundBusy`
  (`background_session.go`), `refreshAuthTokenContext`
  (reintroduced in `client.go`, since upstream does not have a
  context-aware `refreshAuthToken`), the `loggedIn && !background` recovery
  pinger gate, the three `time.Sleep` → `waitBackgroundRetry` swaps and the
  `ctx.Err()` early-return in `doLongPoll`'s retry loop, and `readLongPoll`'s
  `backgroundBusy`-aware idle-close (closing the specific response body via
  `idleClosed`/`rc.Close()` rather than `closeLongPolling()`, and counting an
  intentional idle close as a clean return). `patches/background-session.patch`
  was regenerated from the new base against this tree.
- **Gate**: `go build ./...`, `go vet ./...` and `go test ./...` are clean at
  both the repository root and the nested `third_party/mautrix-gmessages`
  module, including `CGO_ENABLED=1 go test ./pkg/libgm -race -count=1`
  (`TestPassiveCancellationNeverClaimsActiveSession` passes). **The live gate
  required by spec §3.6(d) — pair, list, send one text to the approved direct
  number, receive a reply, run by the coordinator by hand — has NOT been run.**
  This bump must not be merged or released until that gate passes; this PR
  was prepared without access to a physical paired phone.

`go-sdk` is unchanged at this bump. npm moved at `v1.0.1` for the reason in
the table.
