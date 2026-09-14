# Upstream pins

Serves spec §3.6. Every dependency Agent GM pins by an exact revision is
recorded here, one row per pin, plus the entry each bump adds. The bump
procedures are in [operations.md](operations.md#the-libgm-pin-policy); this
page is the record they write to.

## The pins today

| Dependency | Pinned | Why this one | Bumped on |
|---|---|---|---|
| `go.mau.fi/mautrix-gmessages` (`pkg/libgm`) | `d7b1aaf69303163c7b96eb646f4b2f2a81daf8e2` (`d7b1aaf`), `ConfigVersion` 2026.9.2 (unchanged) | The Google Messages client library. Its `ConfigVersion` must be current enough for Google to accept conversation creation | `b0d61b4` → `d7b1aaf`, live gate passed 2026-09-14; see Bumps below |
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

## `libgm` b0d61b4 → d7b1aaf (unreleased)

Eight upstream commits, all by the upstream maintainer on 2026-09-11 and
2026-09-14: `a894267` (`.github` only), `40b0f43` (`pkg/connector/login.go`
only), `6ce6b49` streaming downloads, `10110fd` full `ListConversationsRequest`,
`612eadb` empty phone in `DeleteConversation`, `f4089e7` (connector only),
`e6cc299` long-poll timeout logs, `d7b1aaf` nil safety on public methods.

- **`ConfigVersion`**: 2026.9.2 → 2026.9.2 (unchanged; still stale against
  Google's live 2026.9.9, so §3.6 D3 still applies to conversation creation).
- **Symbols in spec §3.1 that changed**:
  - `methods.go`: `ListConversations(ctx, count int, folder)` became
    `ListConversations(ctx, req *gmproto.ListConversationsRequest)`. Agent GM's
    one call site (`internal/gm/libgm.go`, `rawListConversations`) now builds
    `&gmproto.ListConversationsRequest{Count, Folder}`; the
    `conversationsFetchedOnce` behaviour (§3.7) is unchanged.
  - `media.go`: `DownloadMedia(mediaID, key)` returns `io.ReadCloser` instead
    of `[]byte` — a streaming AES-GCM decryptor (`crypto.AESGCMDecryptStream`,
    new file `crypto/aesgcmstream.go`) over the HTTP body, decrypting chunk by
    chunk with a 4 MiB chunk ceiling. Agent GM's one call site (`rawDownload`)
    drains it with `io.ReadAll` inside the existing goroutine and closes it, so
    `Backend.Download` still returns `[]byte` and the media cache is unchanged.
  - `media.go`: `DownloadAvatar` keeps its signature but is now capped at
    5 MiB by `http.MaxBytesReader`; a larger avatar fails the read instead of
    being buffered.
  - `methods.go`: `DeleteConversation` now sends `Phone` only when non-empty
    (`ptr.NonZero`). Agent GM always passes a phone, so no observable change.
  - `client.go` and friends: every public method on a nil `*Client` (and
    `ResetSessionID`/`buildMessage` on a nil `*SessionHandler`) now returns
    `ErrClientIsNil` or no-ops instead of panicking; `getSessionHandler()` is
    the nil-safe accessor `methods.go` uses. Agent GM never holds a nil client,
    so this is defensive only. `triggerEvent` takes `any` instead of
    `interface{}` (identical type).
  - Nothing else in §3.1 moved; `NewClient`, `Connect`, `ConnectBackground`,
    `Reconnect`, pairing and every RPC wrapper keep their `b0d61b4` signatures.
- **Delivery-state and event vocabulary**: none. No `.proto` file changed.
- **Wire behaviour that changed**: none on the Google side. Downloads now
  decrypt as the body streams rather than after `io.ReadAll`; the request
  headers and URL are the same. The foreground long-poll read timeout
  (`e6cc299`) only adds `last_read_start`/`last_read` to the warning it
  already logged.
- **`go.mod` in the nested module**: `go.mau.fi/util` `eb612d93` → `8d876c16`
  (for `ptr.NonZero`), `maunium.net/go/mautrix` `4ed72472` → `e984191a`
  (connector-only, not compiled here), and `gabriel-vasile/mimetype` dropped
  as a direct requirement. The root `go.mod` follows `go.mau.fi/util`; the
  `replace` and the local import of the patched tree are unchanged.
- **`pkg/connector`** (not vendored): `handlegmessages.go` streams downloads
  into Matrix uploads, `chatsync.go` dispatches `GMChatResync` asynchronously,
  `login.go` fixes the override-login context. None of it is behaviour Agent
  GM mirrors.
- **Interaction with the maintained patch**
  (`third_party/mautrix-gmessages/AGENT_GM_PATCHES.md`): nothing retired,
  nothing added. Every hunk applied cleanly against the pristine `d7b1aaf`
  tree except `readLongPoll`, where upstream's `lastRead`/`lastReadStart`
  timestamps land inside the function the patch rewrites; that hunk was
  merged by hand and both sides are kept. The patch file was regenerated from
  pristine `d7b1aaf`, reconstructs the committed tree exactly, and now carries
  18 hunks (was 17: the timestamps split one `readLongPoll` hunk in two).
- **Race suppressions** (`scripts/race-suppressions.txt`): re-audited. Upstream
  added no locking around `skipCount`, `longPollingConn`, `listenID` or
  `disconnecting`, and the four suppressed frames (`postConnect`,
  `HandleRPCMsg`, `closeLongPolling`, `doLongPoll`) still exist and still
  perform the accesses; every entry stays.
- **Gate**, automated: `go build ./...` and `go vet ./...` are clean at the
  root and in the nested module; `CGO_ENABLED=1 go test ./... -race -count=1`
  passes at the root and `CGO_ENABLED=1 go test ./pkg/libgm -race -count=1`
  in the nested module (`TestPassiveCancellationNeverClaimsActiveSession`
  among them); `pin-consistency`, `fixture-validation` (the §13.4 assertions
  against a fresh clone of `d7b1aaf`, with the two signature strings above
  updated), `lint-names`, `no-real-numbers`, `no-deployment-host` and
  `changeset-check` pass, as does `conformance` against
  `scripts/mcp-conformance-baseline.yaml` in both directions. The old-base
  reconstruction check in the patch guide was run first and matched pristine
  `b0d61b4` exactly.
- **Live gate** (spec §3.6(d), §13.3), run by the coordinator on 2026-09-14
  against the real paired account from this branch at `59f4ab6` plus the two
  test-harness changes below, with the deployment stopped so the gate owned
  the session and a copy of the data directory taken first; every test with
  `-race` and the suppression list:
  - **list** — the approved direct conversation is in
    `ListConversations(FolderInbox)` through the new request struct and
    ingests to a `conv_` ID.
  - **send one text** — `SendText` returned `SUCCESS`; the echo carried the
    sent `TmpID`, then `sending` and `sent`; confirmed in the receiving
    handset's SMS thread.
  - **receive a reply** — the inbound text from the approved number arrived
    inside the ten-second window with the right direction and sender.
  - **session reload** — the account reloaded from its session file and
    reached `connected` without re-pairing.
  - **ConfigVersion and default SMS app** — `config_version_compiled`
    2026.9.2.4.6 against `config_version_live` **2026.9.10.4.6** (Google moved
    from 2026.9.9 since the last gate), so `stale=true`; `is_default_sms_app=true`.
    Upstream `d7b1aaf` is upstream `HEAD` and still carries 2026.9.2, so
    conversation *creation* stays exposed to `google_undocumented_status` (D3).
  - **media download** (the path this bump rewrote) — every attachment in the
    approved direct conversation(s), eight files from 5 KB to 2.4 MB, PNG and
    a HEIF that Google serves as a JPEG, downloaded through the streaming
    decryptor. **The same test run from the `b0d61b4` checkout against the
    same data produced byte-identical files** (`cmp` on all of them), so the
    chunked `AESGCMDecryptStream` reproduces the old `io.ReadAll`-then-decrypt
    output exactly, including across chunk boundaries. Note for anyone
    reading the store: Google declares the *original* size on an attachment
    it has transcoded, so `size_bytes` can be smaller than the download;
    that predates this bump.
  - **media send** — `Upload` of a 5 KB PNG, an immediate `Download` of the
    fresh upload (identical bytes), `SendMedia` with a caption returned
    `SUCCESS`, the echo carried the `TmpID` and one attachment, then
    `sending` and `sent`; the attachment, downloaded after it was listed, is
    identical to the uploaded file. Two observations: the phone answers
    `FAILURE_3` to the header-only `tinyJPEG` the Slice 2 gate uses through
    the server, so this leg sends a real image; and the `sent` echo does not
    yet carry the media id and key, which appear once the message is listed.
  - **Harness changes made to run it**, both in `internal/livegate`: the send
    guard (`refuseUnlessOnlyApproved`) now ignores a *hidden* participant
    that carries the owner's own number — Google Messages lists the paired
    phone a second time on this thread as a non-"me" participant, on both
    pins, and the old guard refused to send; and `media_live_test.go` adds the
    two media tests above.
- **What the gate did not cover**: a fresh pairing, an *inbound* MMS from the
  test phone (its automation sends SMS only; download was exercised on
  attachments the phone had already received and on the echo of the media
  send), the push-mode checklist, and group creation. One `openLive` connect
  during the run timed out with `phone_not_responding` after 60 s and
  succeeded on the next attempt; the same happened on no other of the
  eleven connects, and the old pin was not tried at that moment, so it is
  recorded as transient rather than attributed.
- **Reviewer notes on the vendored tree**: upstream's `readLongPoll` now
  writes `lastRead`/`lastReadStart` in the read loop and reads them from the
  one-minute foreground timeout goroutine without synchronisation. That is a
  new upstream race site that fires only when the foreground long-poll goes
  a minute without a read, so the gate did not hit it; it is outside the four
  suppressed frames and would fail a `-race` run that did. `DownloadMedia`
  still does not check the HTTP status code (it never did), so a non-2xx body
  now fails in the decryptor's header check instead of the old
  `failed to decrypt media`.

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
- **Gate**: `devbox run check` is clean, including `golangci-lint`, at the
  repository root, and `CGO_ENABLED=1 go test ./pkg/libgm -race -count=1`
  passes in the nested `third_party/mautrix-gmessages` module
  (`TestPassiveCancellationNeverClaimsActiveSession` among them).
  `devbox run pin-consistency`, `devbox run fixture-validation` and
  `devbox run conformance` all pass, conformance against
  `scripts/mcp-conformance-baseline.yaml` in both directions.
- **Live gate** (spec §3.6(d), §13.3), run by the coordinator by hand against
  the real paired account, with the deployment stopped so the gate owned the
  session and a copy of the data directory taken first:
  - **list** — the approved direct conversation is in
    `ListConversations(FolderInbox)` and ingests to a `conv_` ID.
  - **send one text** — `SendText` returned `SUCCESS`; the remote echo carried
    back the same bare-UUID `TmpID` that was sent, followed by delivery states
    `sending` then `sent`. Confirmed on the receiving handset as well as in
    the echo, so this is a real send and not a reported one.
  - **receive a reply** — an inbound message with the expected text arrived
    from the approved number inside the test's ten-second window and was
    ingested with the right direction and sender.
  - **session reload** — every account reloaded from its session file,
    connected without re-pairing, and reached `connected` with the session
    still present.
  - **ConfigVersion and default SMS app** — `config_version_compiled`
    2026.9.2.4.6 against `config_version_live` 2026.9.9.4.6, so `stale=true`,
    and `is_default_sms_app=true`. **The pin is stale against Google even
    after this bump**: upstream `b0d61b4` is upstream `HEAD` and still carries
    2026.9.2, so there is no fresher pin to take. Conversation *creation*
    stays exposed to `google_undocumented_status` (§3.6 D3) until upstream
    publishes a newer `ConfigVersion`; sends into existing conversations are
    unaffected, as this gate shows.
- **What the gate did not cover**, so that nobody reads the above as more than
  it is:
  - **a fresh `pair`.** The gate attached to the existing pairing and proved
    session reload instead; re-pairing needs the owner at the physical handset
    and a Google sign-in. §3.6(d) names pairing, and it was not exercised.
  - **the push-mode checklist** in
    [`AGENT_GM_PATCHES.md`](../third_party/mautrix-gmessages/AGENT_GM_PATCHES.md#live-gate-for-a-pin-or-connection-change).
    The §13.3 suite connects actively (`AGENT_GM_CONNECTION_MODE` is read only
    by the server), so push receipt, background fetch, the fifteen-minute
    catch-up and pending-wake restart recovery are deployment-time checks
    (step 7 of that list) and are still owed after release.
  - **media, and group creation.** Text only; claim neither.
  The gate ran with `-race` and the suppression list added in the same release
  (spec §13.3), so the two unsynchronised upstream fields it names were
  ignored and everything else was still watched.

`go-sdk` is unchanged at this bump. npm moved at `v1.0.1` for the reason in
the table.
