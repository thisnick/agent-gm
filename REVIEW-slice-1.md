# Review — Slice 1 (the spike)

**Range reviewed:** `e7aa1d5..a23cb3d` (branch `slice-1/spike`, tip `a23cb3d`)
**Reviewed on:** `review/slice-1`, worktree `.claude/worktrees/review-slice-1`
**Spec:** `plans/AGENT_GM_SPEC.md` §2, §3, §4.1/4.4/4.7, §5.3, §11.4, §13, §16 Slice 1
**Prior reviewer notes:** `git show origin/review/spec-1:REVIEW-spec-1.md`

## Verdict

**Accept with required fixes.**

The slice does what §16 Slice 1 asks. Every claim the implementer made about
counts, tags and CI reproduced exactly under my own runs. Four of my ten
planted mutations **survived**, all four in code the spec calls out by name
(the mandatory `--user-data-dir`, the two-host cookie read, the SQL transition
trigger, the empty-`SourceID` refusal). None of the four is a wrong behaviour
today; all four are behaviours **no test would notice losing**, and three of
them are exactly the failure modes §11.4 and §13.5 say to guard. They are
required fixes, not blockers.

**The coordinator may run the live gate on this build.** `devbox run check` is
green at `a23cb3d`, `go vet -tags live` and `-tags fixtures` are clean, the
live tests are gated behind both `-tags live` and `AGENT_GM_LIVE=1`, they read
their numbers only from `AGENT_GM_LIVE_NUMBERS` / the git-ignored
`testdata/live-numbers.local`, and `no-real-numbers` passes in both directions.
Nothing I found affects the correctness of the paired-and-send path itself.

## Evidence I ran

```
$ devbox run check
agent-gm devbox: go go1.27.0
0 issues.
ok  github.com/thisnick/agent-gm/cmd/agent-gm      1.985s
ok  github.com/thisnick/agent-gm/internal/accounts 1.887s
ok  github.com/thisnick/agent-gm/internal/cli      1.022s
?   github.com/thisnick/agent-gm/internal/clock    [no test files]
?   github.com/thisnick/agent-gm/internal/config   [no test files]
?   github.com/thisnick/agent-gm/internal/core     [no test files]
ok  github.com/thisnick/agent-gm/internal/gm       1.038s
ok  github.com/thisnick/agent-gm/internal/gm/fake  1.015s
ok  github.com/thisnick/agent-gm/internal/store    1.649s
no-real-numbers: clean
EXIT=0
```

```
$ CGO_ENABLED=1 go test ./... -race -count=1 -v
EXIT=0
=== RUN Test...            : 94
--- (PASS|FAIL|SKIP) top   : 74     <- ordinary tests
--- (PASS|FAIL|SKIP) all   : 94     <- with subtests
--- SKIP: TestFindChromeWithChromeHidden (0.00s)
```

**Counts confirmed exactly as claimed: 74 ordinary tests, 94 with subtests, 6
packages with tests.** One skip (`TestFindChromeWithChromeHidden`,
`internal/cli/pairing_test.go:76`) — it skips when a Chrome binary is present in
the platform's usual locations, so on this host it never runs; see F-6.

```
$ AGENT_GM_UPSTREAM_DIR=/home/nick/code/mautrix-gmessages devbox run fixture-validation
fixture-validation: using existing checkout /home/nick/code/mautrix-gmessages
--- PASS: TestAssertion01_SectionThreeOneSignatures
--- PASS: TestAssertion02_ConfigVersion
--- PASS: TestAssertion03and12_MessageStatusTypeCoverage
--- PASS: TestAssertion04_GetOrCreateConversationStatus
--- PASS: TestAssertion05_SendMessageStatus
--- PASS: TestAssertion06_SendReactionAction
--- PASS: TestAssertion07_ListConversationsFolder
--- PASS: TestAssertion08_AlertType
--- PASS: TestAssertion09_Constants
--- PASS: TestAssertion10_ShouldIgnoreStatus
--- PASS: TestAssertion11_SendRetry
--- PASS: TestAssertion13_EmojiType
--- PASS: TestAssertion14_GenerateTmpID
--- PASS: TestAssertion15_GaiaCookies
--- PASS: TestAssertion16_DeviceSelection
--- PASS: TestAssertion17_FatalListenStatuses
--- PASS: TestAssertion18_DedupAbandonsBatch
--- PASS: TestAssertion19_BrowserActiveHasNoEmitters
--- PASS: TestAssertion20_CookieRefreshWithoutRepair
ok  github.com/thisnick/agent-gm/internal/upstream 0.009s
EXIT=0
```

19 functions, 20 assertions (3 and 12 are deliberately one function, because
the 200–279 band and the outside-the-band coverage are two halves of one walk
over the same enum). Upstream checkout verified at `be48a58 libgm/config: bump
version`, `git status --short` empty — I did not write to it.

**These are asserted against upstream, not against a copy.** I read all 694
lines of `internal/upstream/validate_test.go`. Every assertion goes through
`upstreamDir(t)`, which `t.Fatal`s when `AGENT_GM_UPSTREAM_DIR` is unset
(`validate_test.go:23-32`) and re-checks that the directory holds
`pkg/libgm/client.go`; there is no fallback to a vendored or committed copy.
`testdata/libgm/be48a58/` contains only a `README.md` recording the convention
— **no fixture blobs at all**, so there is nothing for a claim to be checked
against except the pinned tree. `scripts/fixture-validation.sh:32-36`
additionally re-verifies `git -C "$upstream" rev-parse HEAD` begins with the
pin before running a single test, so a stale or wrong checkout fails loudly
rather than silently validating the wrong tree. Where an assertion also names
an Agent GM symbol (`gm.MappedStatusValues`, `gm.IgnoredStatuses`,
`gm.SendRetryBackoff`, `gm.GaiaRequiredCookies`, …) the upstream side is always
the source of truth and the Agent GM side is the thing compared to it, which is
the right direction.

Both build tags compile and vet clean:

```
$ devbox run -- go vet -tags live ./internal/livegate/...      # clean
$ devbox run -- go vet -tags fixtures ./internal/upstream/...  # clean
```

Live gate inventory: 5 `-tags live` tests
(`internal/livegate/live_test.go:156,201,269,306,329`) covering Slice 1
acceptance tests 9, 10, 11, 12, 13. Acceptance test 8 (no Chrome → exit 9) is
covered by an **ordinary** test, `cmd/agent-gm/spike_test.go:170`, which is
better than a live gate. Acceptance test 7 (the real Chrome capture, with the
`openat` watch) has no automated form and remains a coordinator hand-run — see
F-6.

## Findings

### F-1 — the mandatory dedicated `--user-data-dir` has no test (**SURVIVED**)

*Spec:* §11.4 step 1 (“**The dedicated `--user-data-dir` is mandatory, not
stylistic**”), §16 Slice 1 acceptance test 7.
*Site:* `internal/cli/chrome.go:203`.

Plant **R-M8**: delete the `"--user-data-dir="+c.ProfileDir,` argument from the
`exec.CommandContext` argv. **Whole suite still passes.** No test anywhere reads
the argv `Capture.Run` builds (`grep -rn "user-data-dir" --include=*_test.go` →
no hits).

*Production consequence.* Two, and the second is the serious one. (a) Current
Chrome refuses `--remote-debugging-port` against the default profile directory,
so pairing would fail at the live gate with an opaque
“chrome's debugging port never answered”. (b) Worse, on a build where it *did*
attach, Agent GM would open a live CDP credential channel onto **the owner's own
Chrome profile** — every cookie for every site — rather than onto the dedicated
one. That is a security property of the pairing flow with a zero-cost regression
path.

*Fix required.* Make the argv testable and assert it. Smallest change: extract
`func (c *Capture) args(port int) []string` and add a test asserting the slice
is exactly `{"--user-data-dir="+ProfileDir, "--remote-debugging-port=<port>",
gm.GaiaCaptureURL}` — which simultaneously pins §11.4's “no other flag is
passed — no `--enable-automation`, no `--headless`”, also currently untested.
Then re-plant R-M8 and show it dies.

### F-2 — the two-host cookie read has no test (**SURVIVED**)

*Spec:* §11.4 step 4 (“**The read must cover `https://messages.google.com` as
well as `https://www.google.com`** … a read of `.google.com` alone is the most
common way this fails”), §16 Slice 1 acceptance test 7.
*Site:* `internal/cli/chrome.go:406`.

Plant **R-M4**: `var cookieURLs = []any{"https://www.google.com"}`. **Whole
suite still passes.**

*Production consequence.* `OSID` is host-scoped to `messages.google.com`, so the
read returns an unusable set, `waitForCookies` never satisfies
`MissingRequiredCookies` and the capture spins until the 5-minute timeout, then
reports `ErrOSIDNeverAppeared` — the exact symptom the spec names as the most
common failure, discoverable only by a coordinator standing at a phone.

*Note:* `internal/cli/pairing_test.go:262` (`TestTheSevenCookiesAndTheirDomains`)
looks like it covers this, but it asserts `gm.GaiaCookieDomains` — the
*expectation table* — not the CDP request's `urls` array. It is the mutation's
survival that proves the difference.

*Fix required.* Assert `cookieURLs` (or the params map `readCookies` builds)
contains both hosts. Re-plant R-M4.

### F-3 — the SQL delivery-state trigger is untested **and weaker than §4.4** (**SURVIVED**)

*Spec:* §4.4, “Permitted transitions for an **outgoing** message, **enforced by
a SQL trigger as well as in Go**”.
*Site:* `internal/store/migrations.go:137-171`.

Plant **R-M6**: neuter the trigger's `WHEN` clause (`… AND 0`), i.e. the trigger
never fires. **Whole suite still passes.** No test reaches the trigger:
`grep -rn "UPDATE messages" --include=*_test.go` → no hits, so every store test
goes through `UpsertMessage`, which applies the *Go* rule
(`internal/store/messages.go:233-240`) and never lets an illegal value reach
SQLite. The SQL half of “as well as in Go” is presently dead weight.

Separately, and independent of the plant, the trigger **does not encode §4.4's
table**. I extracted it from `migrations.go` and loaded it into a bare SQLite
database:

```
triggers: messages_no_backward_delivery
read      -> sent      : REFUSED
delivered -> sent      : REFUSED
failed    -> sent      : ALLOWED
deleted   -> sending   : ALLOWED
delivered -> canceled  : ALLOWED
canceled  -> delivered : ALLOWED
```

The four `ALLOWED` rows are all refused by `gm.TransitionAllowed`
(`internal/gm/delivery.go:171-201`), correctly. The trigger only ranks the
four-rung `sending→sent→delivered→read` ladder and gives every terminal state
rank `0`, so any move *out of* `failed`, `canceled` or `deleted` passes.

*Production consequence.* Today, nothing: every writer goes through
`UpsertMessage`. But the trigger exists precisely as the backstop for a future
writer (Slice 2's backfill, a repair script, a `sqlite3` session), and as
written it would let a resurrected `failed` message come back as `sent` — a
message the owner was told did not send, silently reappearing as sent.

*Fix required.* (a) One store test that writes a backward transition with raw
SQL, bypassing `UpsertMessage`, and expects the `ABORT`. (b) Extend the trigger
to refuse leaving `failed`/`canceled`/`deleted` for anything but `deleted`, so
the trigger and `TransitionAllowed` agree row for row. Re-plant R-M6.

### F-4 — the empty-`Mobile.SourceID` refusal has no test (**SURVIVED**)

*Spec:* §3.2, §4.1 (`acct_` is UUIDv5 of the lowercased `Mobile.SourceID`),
§16 Slice 1 acceptance test 4.
*Site:* `internal/gm/libgm.go:221-227`.

Plant **R-M9**: replace `if !plausibleAddress(address)` with `if false`. **Whole
suite still passes.**

*Production consequence.* `AccountID("")` is a perfectly valid UUIDv5, so two
different degenerate pairings would derive the **same `acct_` ID** and adopt
each other's conversations and messages. This is the “ID derivation missing the
account component” failure mode in a different disguise, and `google_account` is
`NOT NULL UNIQUE` on the row, so the second pairing would fail on a constraint
rather than on a comprehensible error.

*Note:* `TestPairingWithNoAddressCreatesNothing`
(`internal/accounts/supervisor_test.go`) exercises the **supervisor's** refusal
against the fake; the adapter's own guard, which is the one that runs against
the real library, is untested. `plausibleAddress` is unexported and reachable
only through `StartGooglePairing`, which needs a real `libgm.Client` — that is
why it has no test.

*Fix required.* Test `plausibleAddress` directly (a same-package test can) over
`""`, `"noatsign"`, `"@x"`, `"x@"`, `"a b@c"`, `"alex@example.com"`. Re-plant
R-M9.

### F-5 — `pairing_init_timeout` omits `details.device_count` (undeclared deviation)

*Spec:* §3.5 — `ErrHadMultipleDevices` “is reported as
`details.multiple_devices: true` **plus `details.device_count`**”.
*Site:* `internal/gm/errors.go:255-258` sets only `{"multiple_devices": true}`.

`TestMultipleDevicesIsDetailNotCode` asserts the code and `multiple_devices`
but not `device_count`, so the gap is invisible. I believe the field is
genuinely **unobtainable at this pin** — `pair_google.go` wraps
`ErrHadMultipleDevices` inside `ErrPairingInitTimeout` with
`fmt.Errorf("%w (%w)", …)` and never carries the count (fixture assertion 16
asserts exactly that wrapping) — which makes this a spec defect rather than an
implementation one, but it was **not declared** among the four deviations.

*Fix required.* Declare it, and either drop `device_count` from §3.5 or state
that it is present only when the count is separately known.

### F-6 — `TestFindChromeWithChromeHidden` silently skips on any machine with Chrome

*Spec:* §16 Slice 1 acceptance test 8.
*Site:* `internal/cli/pairing_test.go:76`.

The one skip in the suite. It is the negative half of the no-Chrome path, and it
skips exactly on the machines that matter. The *positive* consequence — the
message and exit 9 — is covered by `cmd/agent-gm/spike_test.go:170`, which does
run, so the risk is small; but a skip that is unconditional on a developer
workstation and on GitHub's `ubuntu-latest` (which ships Chrome) is a test
nobody ever runs.

*Fix suggested.* Drive `FindChrome` through an injected `exec.LookPath` and an
injected candidate list instead of the real filesystem, so the hidden-Chrome
case is deterministic.

### F-7 — `--paste` parses any `-H` header, not only the Cookie header

*Spec:* §11.4 (“accepts … a **cURL command** copied from browser devtools”; the
brief's requirement is that it parse only the Cookie header).
*Site:* `internal/cli/paste.go:104-107`.

`cookiesFromCurl` looks for `cookie:` in the header value and strips it, but
when the substring is **absent it does not skip the header** — it falls through
and splits the whole value on `;` / `=`. So `-H 'x-client-data: SID=whatever'`
contributes `SID`. The name allowlist at `paste.go:65-70` bounds the damage to
the seven cookie names, and the paste is the owner's own clipboard, so severity
is low; but the code does not do what its own doc comment says.

*Fix suggested.* `continue` when the header value has no `cookie:` prefix,
except for `-b` / `--cookie`, whose whole value *is* the cookie string.

### F-8 — four `devbox.json` scripts point at files that do not exist

*Spec:* §13.6 (“**Every job in that table is a `devbox run <script>`** … A CI
step that shells out to something with no script is a step nobody can reproduce
locally”).
*Site:* `devbox.json` `conformance`, `lint-names`, `build-matrix`, `image`.

```
$ devbox run conformance   → ./scripts/mcp-conformance.sh: not found   (exit 127)
$ devbox run lint-names    → FAIL (no internal/lint package)           (exit 1)
$ devbox run build-matrix  → ./scripts/build-matrix.sh: not found      (exit 127)
$ devbox run image         → ./scripts/build-image.sh: not found       (exit 127)
```

They are correctly **absent from `.github/workflows/ci.yml`**, and §16 Slice 1
only requires the three jobs that are present, so this is not a CI failure. But
`Makefile` advertises `make conformance` and `make lint-names` as working
targets, and a declared-but-broken script is the thing §13.6 exists to forbid.

*Fix suggested.* Delete the four scripts from `devbox.json` and the `Makefile`
until their slices land, or stub each with `echo "arrives in Slice N" && exit 0`.

### F-9 — spec self-contradiction: §11.4's device line cannot be served by §3.1's mandated call

*Spec:* §11.4 (“`agm pair` prints which device it chose **and when it was last
seen**”) vs §3.1 (`DoGaiaPairing` “**is the call Agent GM uses**”).

The implementer declared this as a deviation and is right to. `DoGaiaPairing`
does not surface the `*PairingSession` that `StartGaiaPairing` returns, and the
last-seen timestamp lives only in an upstream log line
(`pair_google.go:364` region, fixture assertion 16). §11.4 promises output that
§3.1's own mandated call cannot produce.

*Fix required (spec, not code).* Either soften §11.4 to “prints which device it
chose”, or change §3.1 to let Agent GM drive `StartGaiaPairing` +
`FinishGaiaPairing` itself — noting that doing so also forfeits the
“`DoGaiaPairing` reconnects in its own goroutine” behaviour §3.1 says Agent GM
depends on and must not re-implement.

### F-10 — `no-real-numbers` does not scan commit messages, though its header says it does

*Site:* `scripts/no-real-numbers.sh:5` (“No commit, no fixture, no doc page, no
example and **no commit message**…”) vs `:41` (`git ls-files`).

The script scans tracked files only. The CI job runs it the same way. Low
severity — no commit in this range carries a number, and `git log --format=%B
e7aa1d5..a23cb3d` is clean — but the header overclaims.

*Fix suggested.* Drop “no commit message” from the header, or add a
`git log --format=%B` pass over the PR range in the CI job.

## Clause-by-clause verification table

All rows at `a23cb3d` unless noted. `n/a` rows are listed so every deferral is
explicit.

### §2 Architecture

| Item | Status | Evidence |
|---|---|---|
| §2.2 package boundaries: nothing above `internal/gm` imports `libgm`/`gmproto` | verified `a23cb3d` | `grep -rn "mautrix-gmessages" --include=*.go` outside `internal/gm`, `internal/upstream` → no hits |
| §2.3 `Backend` interface complete, `var _ gm.Backend = (*fake.Backend)(nil)` | verified `a23cb3d` | `internal/gm/fake/fake_test.go` `TestEveryBackendMethodIsCallable` |
| §2.3 events channel cap 1024, non-blocking send, `dropped_events` counter | verified `a23cb3d` | `libgm.go:710-717`; `TestFakeDropsEventsAndCountsThem` |
| §2.3 `ResolveResult` carries the raw status | verified `a23cb3d` | `libgm.go:448`; `TestFakeResolveStatusScript` |
| §2.3 `ConversationChange` carries folder/pinned/unread | partial `a23cb3d` | field present; pinned/unread refused — declared deviation, `libgm.go:590-595` |
| §2.4 single writer goroutine, `SetMaxOpenConns(1)` | verified `a23cb3d` | `store.go:66-70,98-107`; suite passes `-race` |
| §2.4 WAL, `busy_timeout=5000`, `foreign_keys=on`, `synchronous=NORMAL` | verified `a23cb3d` | `store.go:58-61` |
| §2.4 one ingest goroutine per account, `account_id` on every row | verified `a23cb3d` | `TestTwoAccountsRunConcurrentlyWithoutCrossTalk`, `TestIngestGoroutineDrainsEvents` |
| §2.4 per-conversation send locks | n/a | `core` send arrives Slice 2 |

### §3.1 Library surface

| Item | Status | Evidence |
|---|---|---|
| Every §3.1 symbol exists with the stated signature at `be48a58` | verified `a23cb3d` | fixture assertion 1 (56 signatures, against upstream) |
| `NewClient(auth, nil, logger)` — `pk == nil` always | verified `a23cb3d` | `libgm.go:71` |
| `PairCallback` left nil; `SetProxy`, `ConnectBackground`, `SetPingInterval`, `SetDataReceiveCheckInterval` not called | verified `a23cb3d` | `grep -c` over `internal/gm` → 0 call sites each |
| `Unpair`/`UnpairGaia`/`UnpairBugle` out of contract | verified `a23cb3d` | no call sites |
| `Connect` is the only connect path | verified `a23cb3d` | `libgm.go:83-89` |
| `TmpID` == `MessagePayload.TmpID` == `TmpID2`, one bare UUID | verified `a23cb3d` | `libgm.go:461-478`; `TestGenerateTmpIDIsABareUUID`; fixture assertion 14; **plant R-M2 killed** |
| `ParticipantID` = `DefaultOutgoingID` | partial `a23cb3d` | plumbed as `req.ParticipantID` (`libgm.go:468`); the *choice* of `DefaultOutgoingID` is core's, Slice 2 |
| Caption as a second `MessageInfo` | verified `a23cb3d` | `libgm.go:507-514` |
| `ForceRCS` gated on RCS + `SEND_MODE_AUTO` + explicit ask | n/a | adapter passes through; the gate is core's, Slice 2. `gm.ForceRCSEligible` exists and is tested |
| `Upload`/`Download` wrapped in a goroutine + `select` on `ctx.Done()` | verified `a23cb3d` | `libgm.go:646-694` |
| `ListConversations` issued once per account on connect | verified `a23cb3d` | `supervisor.go:230-237` and the connect path |

### §3.4 Event stream — reactions **per account**

| Event | Status | Evidence |
|---|---|---|
| `ClientReady` → `connected`, upsert conversations | verified `a23cb3d` | `supervisor.go:389-396`; `TestEventsMoveOnlyTheirOwnAccountsState` |
| `AuthTokenRefreshed` → persist `AuthData` | verified `a23cb3d` | `supervisor.go:397-401` |
| `ListenTemporaryError` → `degraded` / `ListenRecovered` → `connected` | verified `a23cb3d` | `supervisor.go:402-407` |
| `ListenFatalError` 401/403 or `ErrInvalidCredentials` → `signed_out`; else `error` | verified `a23cb3d` | `errors.go:164-178`, `supervisor.go:408-416`; `TestIsFatalListenError`; fixture assertion 17; **plant R-M1 killed** |
| matched on the value, never the string | verified `a23cb3d` | `IsFatalListenError` uses `errors.As`/`errors.Is` only |
| `PingFailed`: `ErrRequestedEntityNotFound` → `signed_out`; else `ErrorCount > 1` → `error` (first ignored) | verified `a23cb3d` | `supervisor.go:417-426`; **plant R-M7 killed** by `TestEventsMoveOnlyTheirOwnAccountsState` |
| `PhoneNotResponding` / `PhoneRespondingAgain` → per-account health flag | gap `a23cb3d` | events plumbed to `Apply` but no-op'd (`supervisor.go:466-471`); health doc is Slice 2 |
| `NoDataReceived` → counter **+ reconciliation sweep** | gap `a23cb3d` | no-op'd, same site. **The sweep hook does not exist in this slice** — see “Reconciliation hook” below |
| `HackySetActiveMayFail` → re-issue `SetActiveSession` after a delay | gap `a23cb3d` | no-op'd, same site |
| `PairSuccessful`: read `PhoneID`, never dereference `QRData` | verified `a23cb3d` | `libgm.go:751-755`, `supervisor.go:427-431` |
| `RevokePairData` → `signed_out`, deletes nothing | verified `a23cb3d` | `supervisor.go:438-440`; `TestSignOutKeepsHistoryAndShredsTheSession` |
| `GaiaLoggedOut` → `signed_out`, deletes nothing | verified `a23cb3d` | `supervisor.go:432-437` |
| `AccountChange`: `IsFake` ignored, real → `account_changed` | verified `a23cb3d` | `supervisor.go:441-446` |
| `*events.BrowserActive` **not** subscribed | verified `a23cb3d` | absent from `handleEvent`; fixture assertion 19 |
| `WrappedMessage` / `Conversation` / `UserAlertEvent` / `Settings` / `TypingData` demuxed | verified `a23cb3d` | `libgm.go:762-784` |
| unknown type → `unknown_events` counter | verified `a23cb3d` | `libgm.go:781-784`, `supervisor.go:472-476` |
| `AlertType` 28 values; `BROWSER_ACTIVE(2)` → resync, sync-started → defer backfill, etc. | gap `a23cb3d` | enum verified (fixture assertion 8) but `EventUserAlert` only calls `touch` (`supervisor.go:463-465`); no per-alert dispatch |
| **every reaction scoped to one account** | verified `a23cb3d` | `TestEventsMoveOnlyTheirOwnAccountsState` asserts the sibling account is untouched |

**Reconciliation hook.** §3.4's dedup note and §5.4's sweep are the one place
the spec calls “the single most important correctness consequence in this
section”. Upstream's abandon-the-batch behaviour is **verified against the pin**
(fixture assertion 18) and **modelled in the fake**
(`TestFakeAbandonsTheRestOfABatch`), which is the right groundwork. But there is
no sweep and no trigger for one in this slice — `NoDataReceived`,
`BROWSER_ACTIVE` and `MOBILE_DATABASE_SYNC_COMPLETE` all fall into the same
no-op arm. That is legitimate (§5.4 and backfill are Slice 2 deliverables) but
it was **not among the four declared deviations**, and it is the crash-boundary
/ silent-loss risk of this design. It must be the first thing Slice 2 lands.

### §3.5 Error taxonomy

Every row below is asserted by `TestClassifyEnumeratesTheSection35Taxonomy`
(`internal/gm/errors_test.go`), which **enumerates** the list as §16 test 5
requires (a subtest per row, visible in the `-v` output) rather than sampling.

| Library error | Code / status | Status |
|---|---|---|
| `ErrPhoneNotResponding` | `phone_not_responding` 504, `KeepsOperationPending=true` | verified `a23cb3d` |
| `ErrConnectionClosed` | `disconnected` 503 | verified `a23cb3d` |
| `ErrInvalidCredentials` | `unsupported_capability` 409 + `not_signed_in`, signs out | verified `a23cb3d` |
| `ErrRequestedEntityNotFound` | same | verified `a23cb3d`; **plant R-M10 killed** |
| `ErrCallerNoPermission` | `google_permission_denied` 502 | verified `a23cb3d` |
| `RequestError` | `google_error` 502 + type/message in details | verified `a23cb3d` |
| `HTTPError` | `google_http_error` 502 | verified `a23cb3d` |
| `ErrNoCookies` … `ErrPairingTimeout` (6 pairing codes) | 409 each | verified `a23cb3d` |
| `ErrPairingInitTimeout` | 409 retryable, `multiple_devices` in details | partial `a23cb3d` — `device_count` missing, **F-5** |
| no `pairing_multiple_devices` code exists | verified `a23cb3d` | `TestMultipleDevicesIsDetailNotCode` |
| **`not_signed_in` ≠ `not_paired`** | verified `a23cb3d` | `TestCredentialDeathIsNeverNotPaired`; `not_paired` is deliberately absent from the `Code` list, so it cannot be reached by accident |
| `errors.Is` against the three sentinels (not `RequestError.Is`) | verified `a23cb3d` | `libgm.go:861-905` checks sentinels before `errors.As(&events.RequestError{})` |

### §3.7 Sharp edges

| Item | Status | Evidence |
|---|---|---|
| `FAILURE_2`/`FAILURE_3` transient, retried `[3s, 8s, 20s]` | verified `a23cb3d` | `TestSendFailureMapping`; fixture assertion 11 |
| **`FAILURE_4` not retried**, → `not_default_sms_app` | verified `a23cb3d` | `errors.go:340-352`; fixture assertion 11 asserts `!SendStatusFailure4.IsTransient()`. This is the implementer's own surviving plant M18, fixed at `4a4fd32` — the fix is real and the killing assertion is in the fixture job, so a future regression is caught against upstream too |
| `UNKNOWN(0)` not transient | verified `a23cb3d` | `TestSendFailureMapping` |
| `ErrPhoneNotResponding` keeps the operation **pending**, never failed | verified `a23cb3d` | `errors.go:194-201` `KeepsOperationPending`; `TestClassifyEnumeratesTheSection35Taxonomy` asserts the flag. Consumption by an operations row is Slice 2 |
| `GoogleAccountSwitch` non-empty recorded | partial `a23cb3d` | surfaced in `SendResult` (`libgm.go:526-529`) and printed by `spike send`; the `account_changed` marking on this path is Slice 2 |
| `GetOrCreateConversation` `CREATE_RCS` retried exactly once, that way | verified `a23cb3d` | `libgm.go:436-447`; `TestFakeResolveStatusScript` |
| a **second** `CREATE_RCS` → `google_error` | gap `a23cb3d` | adapter returns the raw status; the rule is core's, Slice 2 |
| unnamed status → `google_undocumented_status` with the bare integer | verified `a23cb3d` | `TestUndocumentedResolveStatus`; fixture assertion 4 |
| `config_version_stale` = live/compiled **date** diff, no status required | verified `a23cb3d` | `TestConfigVersionStaleNamesBothVersions`, `TestCompiledConfigVersionMatchesTheSpec` (`SameDate` ignores V1/V2) |
| status ranges 1–27 / 100–118 / 200–279 / 300 all mapped, nothing falls to `unknown` | verified `a23cb3d` | `TestDeliveryStateMapping`, `TestUnmappedStatusIsReportedNotSwallowed`, `TestSystemEventBand`; **fixture assertions 3+12 walk the pinned enum** |
| tombstones `kind='system'`, excluded unless `include_system` | verified `a23cb3d` | `TestSystemEventsAreExcludedByDefault` |
| `shouldIgnoreStatus` set carried over verbatim | verified `a23cb3d` | fixture assertion 10 compares the pinned function body against `gm.IgnoredStatuses()` |
| **reaction enum**: 14 values, `CUSTOM(8)`/`EMOTIFY(13)` empty unicode | verified `a23cb3d` | fixture assertion 13 |
| emoji canonicalised through `UnicodeToEmojiType` → `Unicode()` **before** deriving `react_` | verified `a23cb3d` | `libgm.go:532-540`, `store/ids.go:83-86`; `TestEmojiCanonicalisation`; fixture assertion 13 asserts `❤`/`❤️` collapse |
| `EMOTIFY` served as `{"emoji": null, "type": "emotify"}`, not skipped | verified `a23cb3d` | `TestEmojiCanonicalisation`; DTO is Slice 2 |
| `Conversation.LatestMessage` never persisted on the conversation row | verified `a23cb3d` | absent from the `conversations` insert (`store/messages.go:113-…`) |
| microseconds → milliseconds converted once at the `gm` boundary | verified `a23cb3d` | `internal/gm/convert.go`; store columns are `*_ms` |
| library logger at `info`, `trace` only under `AGENT_GM_UNSAFE_TRACE=1` | verified `a23cb3d` | `internal/config/config.go` |

### §4.1 Identifiers

| Item | Status | Evidence |
|---|---|---|
| one frozen `IDNamespace`, declared once | verified `a23cb3d` | `store/ids.go:13` |
| `acct_` = UUIDv5(ns, "account", lowercased `Mobile.SourceID`) | verified `a23cb3d` | `ids.go:48-52`; `TestIDDerivationComputedIndependently` computes the expected UUIDs independently rather than re-calling the function |
| **`account_id` in `conv_`, `msg_`, `contact_` derivations** | verified `a23cb3d` | `ids.go:57-70`; **plant R-M3 killed by three named tests** |
| a different phone on the **same** account → the same IDs | verified `a23cb3d` | `TestSameAccountDifferentPhoneKeepsEveryID` |
| a **different** account → disjoint IDs | verified `a23cb3d` | same test + `TestTwoAccountsRunConcurrentlyWithoutCrossTalk` |
| pairing the same address twice yields **one** account | verified `a23cb3d` | `TestRePairNeverPassesThroughPairing` |
| `att_`, `react_`, `part_` inherit the account transitively | verified `a23cb3d` | `ids.go:73-86` |
| `op_` is UUIDv7 | verified `a23cb3d` | `ids.go:89-91` |
| wrong prefix → `invalid_request`, never `not_found` | verified `a23cb3d` | `TestHasPrefixRejectsTheWrongType`; `spike.go:511` prints the expected prefix and exits 2 |
| `react_` addressable by `DELETE /v1/reactions/{id}` | n/a | REST is Slice 2 |

### §4.3–4.5 Store

| Item | Status | Evidence |
|---|---|---|
| migration `0001` covers `accounts`, `conversations`, `messages`, `participants`, `server_meta` | verified `a23cb3d` | `migrations.go:26-…` — exactly the five §16 names |
| forward-only; a database from the future refuses to open, naming both numbers | verified `a23cb3d` | `TestDatabaseFromTheFutureRefusesToOpen`; `ErrSchemaTooNew` (`store.go:38-48`) |
| no down-migration exists | verified `a23cb3d` | no `down` field on `migration` |
| no dangling foreign keys after migrate | verified `a23cb3d` | `TestMigrationLeavesNoForeignKeyViolations` (`PRAGMA foreign_key_check`) |
| §4.4 transition table enforced **in Go** | verified `a23cb3d` | `TestTransitionRules`, `TestDeliveryTransitionsInTheStore` |
| §4.4 transition table enforced **by a SQL trigger** | **failed `a23cb3d`** | **F-3** — untested and materially weaker than the table |
| a backward move is refused, logged, stored state left alone, `delivery_state_raw` still overwritten | verified `a23cb3d` | `messages.go:233-262`; `core/ingest.go:88-97` |
| an unchanged upsert writes **nothing** | verified `a23cb3d` | `messages.go:226-231`; `TestUnchangedUpsertWritesNothing` |
| `content_hash` over text, media id+size, reactions | verified `a23cb3d` | `messages.go:66-83` |
| `last_activity_ms` never moves backwards | verified `a23cb3d` | `TestConversationActivityNeverMovesBackwards` |
| every query carries an `account_id` predicate or an explicit all-accounts marker | verified `a23cb3d` | `TestMessagesRefusesAQueryWithNoAccountPredicate` |
| no per-account fact stored as a `server_meta` key | verified `a23cb3d` | `server_meta` holds only the schema/global rows; every per-account fact is a column on `accounts` |
| §4.5 data key: 64 hex or std base64, HKDF-SHA256, 4 distinct `info` strings | verified `a23cb3d` | `datakey.go`; `TestParseDataKey` |
| §3.3 envelope: XChaCha20-Poly1305, 24-byte nonce, **AAD = info \| acct id** | verified `a23cb3d` | `session.go:11-65`; `TestSessionEnvelope` |
| session file `0600` in a `0700` directory, atomic write | verified `a23cb3d` | `session.go:42-52,66-…` |
| a session renamed to another account's name fails to open | verified `a23cb3d` | `TestSessionEnvelope` (AAD binding) |
| `messages_fts` | n/a — declared, Slice 2 | `migrations.go:9-13` records it |
| `participants.contact_id` FK to `contacts` | n/a — declared, Slice 2 | `migrations.go:16-19` records the SQLite reason |

### §4.7 Account lifecycle

| Item | Status | Evidence |
|---|---|---|
| the seven states exist with the stated read/write rules | verified `a23cb3d` | `store/accounts.go:20-45`; `TestAccountStatesAndAmbiguityRule` |
| `pairing` is never a resting state | verified `a23cb3d` | `TestPairingRowsAreSweptAndAreNeverARestingState` |
| a **cancelled/abandoned** pairing leaves no orphan account row | verified `a23cb3d` | `TestAbandonedPairingLeavesNoRow`, `TestPairingWithNoAddressCreatesNothing` |
| sign-out shreds the session, zeroes `AuthData`, sets `session_present=0`, **deletes nothing** | verified `a23cb3d` | `supervisor.go:330-342`, `libgm.go:838-857`; `TestSignOutKeepsHistoryAndShredsTheSession`, `TestSignOutStateAndSessionPresence` |
| re-pair resumes the same rows, never a second account | verified `a23cb3d` | `TestRePairNeverPassesThroughPairing`; a new address does create a second account (`TestNewAccountPassesThroughPairing`) |
| `parked` on `max_concurrent`, distinct from `degraded` | verified `a23cb3d` | `supervisor.go:243-250`; `TestSurplusAccountsAreParkedNotDegraded` |
| `state_reason` vocabulary (`capacity`, `listen_error`, `credentials`, `revoked_by_phone`, `cookies_expired`, `account_switched`, `crash_recovered`) | verified `a23cb3d` | `store/accounts.go:34-42`; all but `crash_recovered` are exercised |
| `agm accounts remove` and its effect sentence | n/a | Slice 2 |

### §5.3 Live ingestion

| Step | Status | Evidence |
|---|---|---|
| 1. `shouldIgnoreStatus` → drop and count | verified `a23cb3d` | `ingest.go:71-75`; `TestShouldIgnoreStatus` |
| 2. 200–279 → `kind="system"`, excluded by default | verified `a23cb3d` | `TestSystemEventsAreExcludedByDefault` |
| 3. `msg_` from (account, conv source, msg source) | verified `a23cb3d` | plant R-M3 |
| 4. `content_hash` over canonical content | verified `a23cb3d` | `messages.go:66-83` |
| 5. unchanged → no-op; else update honouring §4.4 | verified `a23cb3d` | `TestUnchangedUpsertWritesNothing` |
| 6. attachments upsert | n/a | `attachments` table is Slice 2 |
| 7. reactions set-replace | n/a | `reactions` table is Slice 2 |
| 8. `TmpID` → operation resolution | n/a | `operations` table is Slice 2 |
| 9. bump `last_activity_ms` / `latest_message_id`, never for `IsOld` | verified `a23cb3d` | `ingest.go:99-108`; `TestConversationActivityNeverMovesBackwards` |
| one goroutine per account, the only message writer | verified `a23cb3d` | `TestIngestGoroutineDrainsEvents` |
| counters atomic | verified `a23cb3d` | `ingest.go:30-36` (`a23cb3d` itself); suite passes `-race` |

### §11.2 / §11.4 CLI and pairing

| Item | Status | Evidence |
|---|---|---|
| exit codes 0/2/3/4/5/6/7/8/9/10, `1` unassigned | verified `a23cb3d` | `cmd/agent-gm/exit.go:9-21`; the mapping is exhaustive over `gm.Code` |
| Chrome resolution: `$AGENT_GM_CHROME` → platform locations → `PATH` | verified `a23cb3d` | `chrome.go:66-84`; `TestFindChromeHonoursTheEnvironmentFirst` |
| **mandatory dedicated `--user-data-dir`** | **failed `a23cb3d`** | **F-1** |
| **no other flag** — no `--enable-automation`, no `--headless` | partial `a23cb3d` | true in `chrome.go:200-205` but untested; folded into F-1 |
| loopback-only CDP port, random | verified `a23cb3d` | `chrome.go:250-263` binds `127.0.0.1:0` to reserve |
| navigate to upstream's capture URL | verified `a23cb3d` | `gm.GaiaCaptureURL` (`libgm.go:175`); fixture assertion 15 asserts it against `connector/login.go` |
| wait for `OSID` before reading | verified `a23cb3d` | `chrome.go:436-449`, `ErrOSIDNeverAppeared` |
| read **exactly** `SID, HSID, OSID, SSID, APISID, SAPISID, __Secure-1PSIDTS` and nothing else | verified `a23cb3d` | `chrome.go:388-402` allowlist; `TestTheSevenCookiesAndTheirDomains`; fixture assertion 15 |
| **across `messages.google.com` AND `www.google.com`** | **failed `a23cb3d`** | **F-2** |
| `OSID` accepted only from `messages.google.com` | verified `a23cb3d` | `chrome.go:396-400` + `gm.GaiaCookieDomains` |
| `__Secure-1PSIDTS` read last | verified `a23cb3d` | `chrome.go:441-446` |
| Chrome terminated immediately after capture | verified `a23cb3d` | `chrome.go:222-232,247-248`; killed by PID, never a name pattern |
| **nothing logs or persists cookies outside the encrypted session file** | verified `a23cb3d` | `grep -rn "[Cc]ookie" --include=*.go` over `cmd`, `internal/{accounts,core,store,config}` → only state/reason names and flag help; `chrome.go:247` logs `len(cookies)`, never a value; `TestParsePasteNeverEchoesAValue`; the only persistence path is `SessionStore.Save` |
| profile kept at `<state>/chrome-profile/<acct_id>`, mode `0700` | verified `a23cb3d` | `chrome.go:128-135,177-183`; `TestProfileDirIsKeyedByAccount` |
| a **new** account gets a fresh profile directory | verified `a23cb3d` | `chrome.go:131-133`; `TestProfileDirIsKeyedByAccount` |
| `--forget-browser` with/without `--account`; one effect sentence used everywhere | verified `a23cb3d` | `ForgetBrowserEffect` (`chrome.go:148-150`); `TestForgetBrowser` |
| the three security notices printed once | verified `a23cb3d` | `spike.go:409-424` |
| **no-Chrome path: the two options in order, exit 9, not a missing-binary error** | verified `a23cb3d` | `NoChromeMessage` (`chrome.go:93-121`), `spike.go:398-403`; `TestNoChromeMessageOffersBothWaysInOrder`, `TestPairWithNoChromePrintsTheTwoOptionsAndExitsNine` (asserts option 1 precedes option 2 and the code is 9) |
| `--paste` / `--paste-file`; JSON **or** a devtools cURL | verified `a23cb3d` | `TestParsePasteAcceptsJSON`, `TestParsePasteAcceptsACurlCommand` |
| **a paste missing `OSID` is refused**, naming the cookie and its domain | verified `a23cb3d` | `paste.go:20-36,71-75`; `TestParsePasteNamesTheMissingCookiesAndTheirDomains`, `TestPastePathNamesMissingCookies`; **plant R-M5 killed by both** |
| the paste parses only the Cookie header | partial `a23cb3d` | **F-7** |
| paste never echoed, never written to disk | verified `a23cb3d` | `TestParsePasteNeverEchoesAValue` |
| **an empty `Mobile.SourceID` is refused** | **failed `a23cb3d`** | **F-4** (behaviour is present at `libgm.go:221`; the guard has no test) |
| `--refresh-cookies` gated on tachyon token + `PairingID`, restores on failure, refuses a different account | verified `a23cb3d` | `libgm.go:280-311`; `TestFakeWrongAccountRefreshChangesNothing`; fixture assertion 20 |
| `--device-index N` → `GaiaHackyDeviceSwitcher` | verified `a23cb3d` | `libgm.go:203`; `TestFakeDeviceSelection`; fixture assertion 16 |
| prints which device it chose **and when it was last seen** | **gap `a23cb3d`** | declared deviation; **F-9** (spec is unsatisfiable as written) |
| the after-pairing notes (default SMS app, group messaging, concurrent web use) | verified `a23cb3d` | `spike.go:357` `afterPairingNotes` |

### §13 Testing and CI

| Item | Status | Evidence |
|---|---|---|
| §13.1 the fake is a deterministic in-memory Google Messages, explicit clock | verified `a23cb3d` | `TestEachFakeCanHoldItsOwnClock`, `TestPairListSendEchoAndDeliveryWalk` — no sleeps |
| §13.1 the fake can be scripted to fail with **any** §3.5 error | verified `a23cb3d` | `TestFakeIsScriptableWithEverySection35Error`, `TestFakePairingFailures` |
| §16 test 3: pair, list 3, send, echo, `sending → sent → delivered` | verified `a23cb3d` | `TestPairListSendEchoAndDeliveryWalk`; SMS variant stops at `sent` (`TestSMSConversationStopsAtSent`) — correct, SMS has no delivery receipt |
| §16 test 4: two fake accounts concurrent, no cross-talk | verified `a23cb3d` | `TestTwoAccountsRunConcurrentlyWithoutCrossTalk` |
| §16 test 6: the fake satisfies the whole interface | verified `a23cb3d` | `TestEveryBackendMethodIsCallable` + the compile-time assertion |
| §13.3 no live send from CI or from a reviewer | verified `a23cb3d` | `//go:build live` + `AGENT_GM_LIVE=1`; absent from `ci.yml`; I ran no live test |
| §13.4 all 20 assertions against a fresh clone of `be48a58` | verified `a23cb3d` | quoted above |
| §13.6 `check` job = `devbox run check` | verified `a23cb3d` | `ci.yml:23-31` |
| §13.6 `pin-consistency` job | verified `a23cb3d` | `ci.yml:33-41`; script exercised below |
| §13.6 `fixture-validation` job | verified `a23cb3d` | `ci.yml:43-52` |
| §13.6 `no-real-numbers` job | verified `a23cb3d` | `ci.yml:54-62`; negative test below |
| §13.6 `check` also runs `lint-names` | n/a — declared, Slice 3 | `internal/lint` does not exist; I ran the lint by hand instead (see the rubric) |
| §13.6 `conformance`, `build-matrix`, `image` jobs | n/a — Slices 3/4 | correctly absent from `ci.yml`, but see **F-8** |

## The pin cannot drift silently

Three independent mechanisms, and I exercised all three.

1. **`internal/gm/pin_test.go` is an ordinary test** — no build tag — so
   `devbox run check` and `devbox run test` both fail if `go.mod`,
   `PinnedUpstreamCommit`, `PinnedUpstreamCommitFull` or §3.6's `commit:` line
   is edited alone. It parses the real files off disk
   (`../../go.mod`, `../../plans/AGENT_GM_SPEC.md`), not a constant.
2. **`scripts/pin-consistency.sh`** re-checks the same three places plus two
   more: that `PinnedUpstreamCommitFull` begins with **both** the short pin and
   the `go.mod` pseudo-version hash, and that `go.mod` actually requires
   `PinnedUpstreamModule`. Its failure message states the bump-is-a-slice rule.
3. **`scripts/fixture-validation.sh:32-36`** refuses to run if the checkout's
   `HEAD` does not begin with the pin — so the 20 assertions can never
   accidentally pass against a *different* upstream tree.

Plus `GOFLAGS=-mod=readonly` in `devbox.json`, so a stray `go get` cannot move
it, and `TestCompiledConfigVersionMatchesTheSpec` pins `util.ConfigMessage`
(`2026.9.2.4.6`) as an ordinary test as well as fixture assertion 2.

I verified `go.mod`'s pseudo-version `v0.2608.1-0.20260904125044-be48a58b7338`,
`PinnedUpstreamCommitFull = "be48a58b733825f6dfd6bb630af5f236d3bc9ae8"` and
§3.6's `commit: be48a58` all agree, and that the upstream checkout is at that
commit. **A single-place edit fails `devbox run check`, not only a CI job** —
which is the strong form.

## `no-real-numbers` does what it claims

Positive: `no-real-numbers: clean` over the whole tree (in `devbox run check`).
Negative, run by me on a probe file:

```
$ cat nrn-probe.txt
call +1 415 555 0123 and <a plausible NANP number, redacted here>
$ ./scripts/no-real-numbers.sh nrn-probe.txt
no-real-numbers: nrn-probe.txt:1 looks like a real phone number: <redacted>
exit=1
```

The fictional `555` exchange is allowed and the plausible number is caught, in
one pass — both directions proven. `testdata/live-numbers.local` and `*.local`
are git-ignored, and the live tests read numbers only from
`AGENT_GM_LIVE_NUMBERS` or that file. Caveat in **F-10**.

## Plant table

Ten planted mutations, all mine, none from the implementer's list of 22. Method:
apply the edit, run `go test ./... -count=1` over the whole module, record
killed (by a **named** test) or SURVIVED, `git checkout --` to revert.

| # | Site | Mutation | Result | Killed by |
|---|---|---|---|---|
| R-M1 | `internal/gm/errors.go:176` | `IsFatalListenError` no longer treats **403** as fatal | **KILLED** | `TestIsFatalListenError` — `errors_test.go:288: IsFatalListenError(http 403 while polling) = false, want true` |
| R-M2 | `internal/gm/tmpid.go:12` | `GenerateTmpID` returns `"op_" + uuid` instead of a bare UUID | **KILLED** | `TestGenerateTmpIDIsABareUUID` — `delivery_test.go:253: tmp ID "op_2edf1dbc-…" is not 36 characters` |
| R-M3 | `internal/store/ids.go:58` | `ConversationID` drops the `accountID` component | **KILLED** ×3 | `TestTwoAccountsRunConcurrentlyWithoutCrossTalk` (`supervisor_test.go:305`), `TestIDDerivationComputedIndependently` (`store_test.go:76`), `TestSameAccountDifferentPhoneKeepsEveryID` (`store_test.go:126`) |
| R-M4 | `internal/cli/chrome.go:406` | cookie read drops `https://messages.google.com`, leaving `.google.com` alone | ***SURVIVED*** (74/74 pass) | — → **F-2** |
| R-M5 | `internal/cli/paste.go:70` | a paste missing **one** required cookie is accepted (`len(missing) > 1`) | **KILLED** ×2 | `TestParsePasteNamesTheMissingCookiesAndTheirDomains` (`pairing_test.go:227`), `TestPastePathNamesMissingCookies` (`spike_test.go:374` — `exit = 10, want 2`) |
| R-M6 | `internal/store/migrations.go:146` | the backward-transition trigger never fires (`… AND 0`) | ***SURVIVED*** (74/74 pass) | — → **F-3** |
| R-M7 | `internal/accounts/supervisor.go:422` | the **first** `PingFailed` moves the account to `error` (`ErrorCount > 0`) | **KILLED** | `TestEventsMoveOnlyTheirOwnAccountsState` — `supervisor_test.go:528: the first ping failure moved the state to error` |
| R-M8 | `internal/cli/chrome.go:203` | drop the mandatory `--user-data-dir` from Chrome's argv | ***SURVIVED*** (74/74 pass) | — → **F-1** |
| R-M9 | `internal/gm/libgm.go:221` | accept an empty/implausible `Mobile.SourceID` (`if false`) | ***SURVIVED*** (74/74 pass) | — → **F-4** |
| R-M10 | `internal/gm/errors.go:215` | `ErrRequestedEntityNotFound` classified as `google_error` instead of `unsupported_capability`/`not_signed_in` | **KILLED** ×2 | `TestClassifyEnumeratesTheSection35Taxonomy/events.ErrRequestedEntityNotFound` (`errors_test.go:111,114,117,120`), `TestCredentialDeathIsNeverNotPaired` (`errors_test.go:175,181`) |

**6 killed, 4 survived.** Every survivor is a named finding above with its
production consequence and the smallest test that would kill it. Each must be
re-planted after the fix and shown to die, with the killing test carrying a doc
comment recording the plant and its date (§13.5).

Working tree left clean: `git status --short` is empty apart from this file.

I planted **no** `t.Skip`ped forward tests this round: Slice 1's surfaces are
`internal/` Go APIs rather than routes or tools, so a plant written ahead of the
code would have had to assume a Go signature rather than a wire contract, and
§13.5's “declares its wire assumptions” has no meaning here. Slice 2 has routes,
and I will plant against them.

## Rubric — §18.1: *every name means what it means in Google Messages*

**Score: 1 of 2.**

Almost everything user-visible in this slice reads correctly. I ran the §13.5
name lint by hand, since `internal/lint` is deferred to Slice 3:

```
$ grep -rnoiE '\b(room|portal|event_id|redact|generation|outbox|tombstone|room_)\b' \
      --include=*.go cmd internal | grep -v _test
(no hits on any served surface)
```

`tombstone` appears 21 times in `internal/gm` and every one is a comment citing
the upstream enum — and `internal/gm/types.go:133` says so explicitly, naming
Agent GM's own word instead. `docs/` holds only `README.md`. The delivery-state
vocabulary (`sending`, `sent`, `delivered`, `read`, `failed`, `canceled`,
`deleted`, `received`, `downloading`, `download_failed`) is Google Messages'
own UI language; the account states (`pairing`, `connected`, `degraded`,
`error`, `signed_out`, `parked`, `account_changed`) are honest; `conversation`,
`message`, `account`, `pair`, `tmp_id` and `alert` all mean what they mean.
`spike diag` prints `config_version_compiled` / `config_version_live` /
`is_default_sms_app`, which are the right names for the right facts.

**What costs the second point** — `cmd/agent-gm/spike.go:354-356`:

```go
if row.GaiaDestRegUUID != "" {
    fmt.Printf("Device:  %s\n", row.GaiaDestRegUUID)
}
```

In Google Messages, **“Device” is a phone the owner recognises** — a model name
and a last-seen time, which is exactly what §11.4 promises `agm pair` will
print. What Agent GM prints under that label is `AuthData.DestRegID`, a raw
`gmproto` registration UUID. It is the one thing in this slice where a name
promises Google Messages' meaning and delivers a protocol internal — the same
mistake §4.6 forbids for `send_mode_raw`.

**Smallest change that would fix it:** relabel the line to
`dest_reg_uuid:  <uuid>` (an obviously-internal name for an internal value),
or drop it from `spike pair` entirely. Restoring the *real* device line needs
F-9 resolved first, because `DoGaiaPairing` cannot surface the phone's name or
last-seen.

Everything else on this axis is a 2.

## Verified

§2.2 package boundaries · §2.3 the whole `Backend` interface and the fake ·
§2.4 single writer, per-account ingest goroutine, WAL pragmas ·
§3.1 all 56 signatures at `be48a58`, the not-called set, `TmpID` as a bare UUID,
the caption shape, the context wrappers · §3.4 the connection-lifecycle,
pairing and data event rows, all scoped per account, and the 401/403 fatal rule
matched on the value · §3.5 the complete taxonomy, enumerated, including
`not_signed_in` ≠ `not_paired` and `ErrPhoneNotResponding` → pending ·
§3.6 the pin in all three places, guarded three ways ·
§3.7 the `[3s, 8s, 20s]` schedule, `FAILURE_4` never retried, the status ranges,
the reaction enum and emoji canonicalisation, `CREATE_RCS` retried once,
undocumented statuses, `config_version_stale`, the microsecond conversion ·
§4.1 the whole ID scheme including the account component and the re-pair
invariants · §4.3 forward-only migrations · §4.4 the delivery vocabulary and
the Go transition rules · §4.5 the data key and the session envelope ·
§4.7 the state vocabulary, sign-out-keeps-everything, re-pair-resumes, `parked`,
`state_reason`, no orphan row from a cancelled pairing ·
§5.3 steps 1–5 and 9 · §11.2 the exit-code mapping · §11.4 Chrome resolution,
loopback CDP, the exact seven cookies, `OSID` sequencing, immediate termination,
the per-account profile, the no-Chrome message and exit 9, the paste fallback
and its `OSID` refusal, cookies never logged or persisted outside the envelope ·
§13.1 the fake · §13.3 the live gating · §13.4 all twenty assertions against the
pinned tree · §13.6 the four CI jobs that exist · §16 Slice 1 acceptance tests
1–6 and 8.

## Could not verify

- §16 Slice 1 acceptance test 7 in full — the real Chrome capture and the
  `openat` watch proving nothing else writes the cookies. Coordinator hand-run.
  Static evidence is in F-1/F-2 and the cookie-leakage grep above.
- §16 Slice 1 acceptance tests 9–13 — the live gates. Written, gated, vetting
  clean; not run by me, by design.
- §3.4 per-alert dispatch and §5.4's reconciliation sweep — not present
  (`supervisor.go:463-471`). Upstream's batch-abandon behaviour *is* verified
  against the pin and modelled in the fake; the response to it is not built.
- §5.3 steps 6, 7 and 8 (attachments, reactions, `tmp_id` → operation) —
  the tables arrive in Slice 2.
- The `ForceRCS` gate, the `DefaultOutgoingID` choice, the second-`CREATE_RCS`
  rule, and the `account_changed` marking on `GoogleAccountSwitch` — all core
  rules with no core to hold them yet.
- `state_reason = "crash_recovered"` — declared, never produced.
- §13.6 `lint-names`, `conformance`, `build-matrix`, `image` — deferred; see F-8.

## Required fixes, in order

1. **F-1** — assert Chrome's argv; the dedicated `--user-data-dir` and the
   absence of `--headless`/`--enable-automation`. Re-plant R-M8.
2. **F-2** — assert the cookie read covers both hosts. Re-plant R-M4.
3. **F-3** — a raw-SQL test that hits the trigger, and align the trigger with
   §4.4's table for the terminal states. Re-plant R-M6.
4. **F-4** — test `plausibleAddress` directly. Re-plant R-M9.
5. **F-5** — declare the `device_count` deviation, or amend §3.5.
6. **F-9** — resolve the §11.4/§3.1 contradiction in the spec.
7. **F-6**, **F-7**, **F-8**, **F-10** — at the implementer's convenience,
   before Slice 2 closes.
