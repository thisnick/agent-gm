# Maintained libgm patch: background request sessions

Read this record before updating the upstream pin or changing connection
behavior. It describes the implementation shipped in Agent GM v1.3.0 and
carried forward, unreleased, onto the `b0d61b4` pin, including its known
limitations.

## Source and reproducible patch

- Upstream: <https://github.com/mautrix/gmessages>.
- Base commit: `b0d61b4e1a4e94f0d5e6fedd43cadb80bd0a9e51`.
- Go version: `v0.2608.1-0.20260910090721-b0d61b4e1a4e`.
- Imported files: upstream `pkg/libgm`, `go.mod`, `go.sum`, `LICENSE`, and
  `LICENSE.exceptions`. The Matrix connector is not copied.
- First shipped in commit `1f8659ed279ac1f30526078c77613def7822145a` (PR #15),
  against base commit `be48a58b733825f6dfd6bb630af5f236d3bc9ae8`. Carried
  forward onto `b0d61b4` per the procedure below; see
  [docs/upstream-pin.md](../../docs/upstream-pin.md) for what changed in that
  bump and what upstream now does natively (the former `doLongPollContext`
  split and the `dittoPinger` context fields).
- Exact source delta: [patches/background-session.patch](patches/background-session.patch).
  Paths are relative to the upstream repository root. The patch includes new
  source/tests, changes to existing files, and the mode difference noted below.

The root `go.mod` uses a local `replace` pointing here. Updating its `require`
version alone does **not** change the compiled library. Builds use the committed
source; the patch is a maintenance artifact and is not applied during builds.

## Why the patch exists

Upstream already has `ConnectBackground()`: open a receive stream, process
pending events, acknowledge them, and return after idle closure. It skips the
normal `postConnect` that claims an active session. The upstream bridge uses
this through its background entry point; its normal lifecycle instead keeps
an active connection for live events and catch-up.

The background API does not coordinate explicit fetch/send requests with
listener readiness and lifetime. Its recovery pinger can also eventually call
`SetActiveSession` or reconnect. Our extension coordinates temporary sessions
for existing RPCs and prevents that recovery behavior in background mode.

No new protocol commands, protobuf definitions, encryption, or shared Google
API key changes were introduced. Passive requests are a locally maintained
extension tested in practice, not a Google guarantee or a complete upstream
background request API.

## Changes and invariants to preserve

| File / symbol | Change and reason |
| --- | --- |
| `pkg/libgm/background_session.go`: `RunBackground` | New context/callback entry point. Checks login; initializes the private RPC session ID if empty without `SetActiveSession` / `GET_UPDATES`; opens the listener and waits for readiness; runs the callback; waits for drain; sends final acknowledgments. Requests must not race listener startup or outlive their response stream. Since `b0d61b4`, upstream's own `doLongPoll` already takes a `ctx context.Context`, so `RunBackground` calls `c.doLongPoll(ctx, true, true, ...)` directly; the patch no longer needs its own `doLongPollContext` wrapper (below). |
| Same file: `waitBackgroundRetry` | Context-aware retry delay so cancellation interrupts waits. |
| `pkg/libgm/client.go`: `backgroundBusy` | Atomic flag postpones idle closure while the callback runs. It is not a concurrency lock; callers serialize sessions. |
| Same file: `refreshAuthTokenContext`, `RegisterPush` | Adds context-aware token-refresh HTTP, keeps the original wrapper, and passes the registration context through. Upstream has never had a context-aware `refreshAuthToken`; re-added on every bump so far, including `b0d61b4`. |
| `pkg/libgm/longpoll.go`: `doLongPoll` | As of `b0d61b4`, upstream's own `doLongPoll(ctx context.Context, loggedIn, background bool, onFirstConnect func()) bool` already threads a caller-supplied context down (added by upstream commit `e00c40312938...`), which is the same intent as this patch's former `doLongPollContext` split — retired rather than reapplied. What the patch still adds on top: an `ctx.Err()` early-return and `refreshAuthTokenContext(ctx, nil)` (instead of the unauthenticated `refreshAuthToken(nil)`) at the top of the retry loop, and swapping its three `time.Sleep(...)` retry delays for `waitBackgroundRetry(ctx, ...)` so a canceled context interrupts them. |
| Same file: recovery pinger | Starts only when `loggedIn && !background`, preventing background recovery from claiming active status. Upstream's `dittoPinger` gained its own `ctx`/`reconnectCtx` fields in `b0d61b4` (upstream commit `e00c40312938...`, replacing its prior `context.TODO()` uses in `Ping`/`recoveryLoop`) — those are upstream's own now, not part of this patch. Audit any new upstream recovery path on each bump. |
| Same file: `readLongPoll` | Postpones idle close by one second while busy (`Reset(time.Second)` on the shared `closeIn` timer, re-checked in a loop), and closes the specific response body (`rc.Close()`) rather than the global connection (`closeLongPolling()`). Tracks intentional idle closure in an `idleClosed atomic.Bool` and treats it as a clean/expected read stop (`errors.Is(..., io.EOF) \|\| c.disconnecting \|\| idleClosed.Load()`) and a successful return (`receivedEvents \|\| idleClosed.Load()`) even with no events. Since `b0d61b4` upstream's own `readLongPoll` additionally takes a `cancel context.CancelFunc` and runs a second, *foreground*-only idle-read timeout (`1 * time.Minute`, canceling `ctx` — a case this patch does not touch) alongside the background timer this patch modifies; both timers now share one `closeIn *time.Timer` variable and are stopped via one `defer closeIn.Stop()` once assigned. |
| `pkg/libgm/client.go`, `pair_google.go`: `DisablePostPairConnect` | Optional flag checked after `PairSuccessful`, before the asynchronous reconnect. Default false preserves upstream behavior. Push-mode pairing must not immediately become active. |
| `pkg/libgm/session_handler.go`: `sendAckRequestContext` | Adds an error-returning helper and keeps the original void wrapper. `RunBackground` reports ACK errors; existing ACK requeue behavior is retained. See the cancellation gap below. Upstream has not touched `session_handler.go` through `b0d61b4`. |
| `pkg/libgm/background_session_test.go` | Direct library cancellation/no-active-request regression test, described below. |

There is also a packaging-only mode difference: `pkg/libgm/gmproto/build.sh`
is `100644` locally versus `100755` upstream, with identical contents. The
patch records the existing mode for exact reconstruction; background mode
does not require this difference. Review the mode explicitly on future imports.

`RunBackground` does **not** lock concurrent callers. Agent GM's
`internal/gm/push.go` uses a per-account gate around protocol access and session
persistence. Its `WithSession` context marker lets sequential nested RPCs share
one batch. Do not overlap batches with each other or with `Connect` /
`Disconnect`; do not retain the callback context or launch concurrent requests
through it. Agent GM currently bounds an outer batch to two minutes.

## Known limitations

- `sendAckRequestContext(ctx)` accepts a context but still calls
  `makeProtobufHTTPRequest`, not `makeProtobufHTTPRequestContext`. Final ACK
  HTTP does **not** honor that cancellation context. Errors are exposed, but
  fully cancellable ACKs are not implemented. The patch records the shipped
  code, including this gap; do not remove this note without fixing/testing it.
- The callback must honor cancellation. A deadline cannot forcibly interrupt
  arbitrary callback code or upstream operations that ignore their context.
- The direct library test covers one mocked cancellation path. It does not
  establish every recovery/error branch, successful idle drain, busy-listener
  timing, token-refresh cancellation, or ACK failure/cancellation. Add focused
  regression coverage when changing those paths.
- Receiving queued events is not exhaustive history synchronization. Agent GM
  owns explicit fetches and timestamp cutoffs.
- Sound and vibration require observation on the phone; HTTP success and unit
  tests cannot establish notification behavior.

## Code outside this patch

`internal/gm/push.go` owns registration/lifecycle, durable pending-wake counters,
retry, serialization, session persistence, and batching. `webpush*.go` in that
directory handles decryption. Server wiring lives in `cmd/agent-gm`.

The one-minute scoped read freshness, destination refresh before sends, and
fifteen-minute catch-up are Agent GM policy in `internal/core/refresh.go`,
`reconcile.go`, API handlers, and account workers. They are not libgm changes.

## Reconstruct and verify the current patch

Run from the Agent GM root with a clean upstream checkout containing the base
commit. These commands create a scratch tree and leave the committed source
and upstream checkout untouched. Never apply the patch to an already patched
tree.

```bash
patch_record="$PWD/third_party/mautrix-gmessages/patches/background-session.patch"
upstream_checkout=/path/to/mautrix-gmessages
patch_verify_dir=$(mktemp -d -t agent-gm-patch-verify-XXXXXX)
git -C "$upstream_checkout" archive b0d61b4e1a4e94f0d5e6fedd43cadb80bd0a9e51 \
  pkg/libgm go.mod go.sum LICENSE LICENSE.exceptions | tar -x -C "$patch_verify_dir"
git -C "$patch_verify_dir" apply --check "$patch_record"
git -C "$patch_verify_dir" apply "$patch_record"
git diff --no-index --exit-code \
  "$patch_verify_dir/pkg/libgm" third_party/mautrix-gmessages/pkg/libgm
for upstream_file in go.mod go.sum LICENSE LICENSE.exceptions; do
  cmp "$patch_verify_dir/$upstream_file" "third_party/mautrix-gmessages/$upstream_file"
done
```

The source diff must be empty, including executable-bit differences. Remove
only that specific scratch directory after verification is complete.

## Updating to a newer upstream pin

1. Use a clean work branch and preserve unrelated user changes. Record old/new
   full commits. First reconstruct the old tree using the procedure above; a
   mismatch means there are undocumented changes to capture before proceeding.
2. Review upstream changes in `pkg/libgm` and `pkg/connector`, especially
   `ConnectBackground`, `postConnect`, recovery `GET_UPDATES`, listener lifecycle,
   RPC IDs, ACKs, pairing, and `ConfigVersion`. Check whether upstream now
   implements any patch behavior. Review new imports, API and enum changes.
3. Export the new commit's same source/module/license paths into another scratch
   directory with `git archive`. Try `git apply --check` there. If it fails,
   port the changes by intent using the table above. Do not force application,
   discard rejects, or omit failed hunks. A clean apply is not behavioral proof.
4. Replace the imported source with the reviewed scratch tree, removing files
   that upstream removed. Preserve this document and `patches/`. Retain both
   licenses and review module changes. Use Devbox for Go tooling. Keep the
   local `replace` until upstream actually provides the required behavior.
5. Update the root Go requirement/sums, `internal/gm/pin.go` (short/full hashes),
   spec section 3.6, `docs/upstream-pin.md`, and this document's base. Follow
   [the pin policy](../../docs/operations.md#the-libgm-pin-policy). Existing pin
   checks compare recorded hashes, not the imported source contents.
6. Regenerate `patches/background-session.patch` from pristine new upstream
   `pkg/libgm` to the patched tree, including new source/tests and mode changes.
   Use `git diff --no-index --src-prefix=a/ --dst-prefix=b/`; normalize directory
   prefixes to `a/pkg/libgm` and `b/pkg/libgm` while preserving hunk counts and
   `/dev/null` headers. Include no machine-specific paths or credentials.
   Reconstruct from the new base plus patch and require an exact match.
7. Run the automated and live gates below. Record API/config/enum changes and
   evidence in `docs/upstream-pin.md`. Add a changeset for an actual pin/code
   change, then use the normal reviewed release and digest-pinned deployment
   workflow with a compatible data backup.

The pairing opt-out could eventually move into Agent GM by calling upstream's
exported `StartGaiaPairing` / `FinishGaiaPairing` directly. That would require
preserving success-event/phone-ID handling. This record does not make that
refactor or assume an upstream proposal has been accepted.

## Automated tests and their scope

Paths below are relative to the Agent GM root unless marked nested.

| Test | Evidence |
| --- | --- |
| Nested `pkg/libgm/background_session_test.go`: `TestPassiveCancellationNeverClaimsActiveSession` | Mock transport permits only one `ReceiveMessages` request. A 100 ms deadline terminates the batch and an RPC session ID exists without claiming active status. Run under `-race`. |
| `internal/gm/push_test.go`: `TestPassiveBatchReusesOneSession` | Three sequential nested calls reuse one mocked batch; verifies adapter coordination, not Google transport. |
| Same file: `TestSessionSaveHoldsPassiveGateUntilDiskWriteCompletes` | Persistence retains the gate, propagates failure, and releases the gate afterward. |
| Same file: `TestPushAcknowledgementRequiresDurableWake` | Wrong/stopped endpoints fail; unsaved wakes fail; saved wake counters persist and wakeups coalesce. This is HTTP push acknowledgment, not Google's message ACK RPC. |
| Same file: `TestPushWorkerRetriesOnlyPendingWork` | Retries a mocked failed batch until delivery, then stops fetching without another wake. |
| Same file: `TestLegacyPushKnownAnswer` | Legacy `aesgcm` known-answer decryption, corruption and malformed-header rejection. |
| `internal/gm/webpush_test.go`: `TestDecryptWebPushRFC8291` | RFC `aes128gcm` known-answer decryption, corruption/truncation and wrong-secret rejection. |
| `cmd/agent-gm/push_probe_test.go`: `TestPushReceiverAuthenticatesBeforeWake` | Diagnostic receiver rejects invalid pushes before waking; valid wakes coalesce. |
| `cmd/agent-gm/background_test.go`: `TestBackgroundDiagnosticRejectsUnsafeAccountBeforeOpeningSession` | Invalid diagnostic account arguments fail before opening a session. |
| `internal/core/refresh_test.go`: `TestFreshReadsCoalesceAndDiscoverPhoneRename` | Concurrent stale reads share a refresh; names update; stale data refetches; freshness survives coordinator recreation. |
| Same file: `TestThreadRefreshFailureKeepsCheckpointAndRetriesWithoutDuplicates` | Failed pagination does not advance the scoped checkpoint; retry honors timestamp boundary and deduplicates. |
| Same file: `TestSendAlwaysRefreshesDestinationAndDoesNotSendOnRefreshFailure` | Each send refreshes metadata; failed refresh prevents sending; idempotent retries do not send twice. |
| Same file: `TestSlowRefreshKeepsStartCutoffAndCachesFromCompletion` | Fetch-start cutoff and completion-based TTL stay distinct during slow batches. |
| Same file: `TestFreshnessPreservesExplicitEpochBackfill` | Explicit full backfills are not restricted by recent freshness. |
| `internal/api/refresh_test.go`: `TestReadRoutesRefreshGoogleDataBeforeAnswering` | Read routes fetch fake-backend changes before answering and report required-refresh failure. |

Run through Devbox from the root:

```bash
# Quick direct library regression test (separate Go module).
devbox run -- env CGO_ENABLED=1 go -C third_party/mautrix-gmessages \
  test ./pkg/libgm -race -count=1

# Required gates for an actual upstream update.
devbox run pin-consistency
devbox run fixture-validation
devbox run check
devbox run conformance
```

`devbox run check` explicitly includes the nested libgm race test, alongside
root tests, lint, vet, and repository checks. Root `go test ./...` alone does
not traverse the nested module. `fixture-validation` checks assumptions against
pristine pinned upstream, not the patched tree. Neither it nor `pin-consistency`
replaces the reconstruction check above.

## Live gate for a pin or connection change

Use an authorized test phone and a disposable copy of paired data. Stop other
processes using that pairing first: a copied database is not a new Google
pairing. Never commit phone numbers, message bodies, cookies, pairing data,
push capabilities, or tokens.

1. Exercise pairing/startup in push mode. Verify no automatic active reconnect
   and that the encrypted push subscription persists across restart.
2. Send from the test phone to the paired phone while idle. Confirm push receipt,
   completed background fetch, and exactly one stored message **before** a read
   that could refresh it. Ask the owner to verify physical sound/vibration.
3. Send from Agent GM to the test phone; confirm receipt and reply ingestion,
   destination refresh, and return to idle. Include media when the change affects
   media/session lifetime; do not claim media coverage from a text-only test.
4. Send directly from the paired phone. Inspect store/logs first to establish
   whether a push arrived. If absent, verify recovery through a stale conversation
   list and separately through a direct thread read. Deliberate stale-data
   injection belongs only in the disposable database.
5. Exercise stale/warm conversation, message, and contact reads. Observe a real
   fifteen-minute catch-up without a triggering read; confirm queued pushes are
   serviced afterward without duplicate messages.
6. Exercise error/deadline handling and pending-wake restart recovery, inspecting
   race reports from an instrumented build. Successful sends do not prove these
   failure paths; record untested cases explicitly.
7. After deployment, verify image/revision/health, repeat authorized public-URL
   send/receive checks, revoke test credentials, and remove temporary servers/data.
   Retain the rollback backup.

Historical evidence: v1.2.0 included owner-confirmed vibration with push delivery.
The v1.3.0 rollout on 2026-09-09/10 exercised a race-instrumented isolated server,
stale/warm reads, stale destination metadata, text send/receive, a user-originated
no-push text recovered through both read paths, and a real scheduled catch-up.
Production public-URL text send/receive then passed, including exactly-once
storage of a reply ingested through push before any read. These are historical
observations; repeat relevant gates for every new upstream pin.
