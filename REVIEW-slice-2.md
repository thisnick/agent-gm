# Review — Slice 2 (store, REST, CLI, media)

> **FINAL — seventh confirm pass at `77abfa9`: ACCEPT. Ready to tag.** See
> [Seventh confirm pass — `77abfa9`](#seventh-confirm-pass--77abfa9) at the end
> of this file. Nothing outstanding. Reviewed nine times: `03c0349` (reject),
> `3b0b67f`, `3604892`, `0e95472`, `fd6f153`, `ebeb8eb`, `dbcd716`, `ac59436`,
> `77abfa9`. Across them: **43 plants, 38 killed, 5 survivors — every survivor
> since fixed, and every one of the 43 dies at this SHA.**

**Range reviewed:** `6109645..03c0349` (25 commits, branch `slice-2/store-api-cli`)
**Reviewed on:** `review/slice-2`, worktree `.claude/worktrees/review-slice-2`
**Spec:** `plans/AGENT_GM_SPEC.md` §4, §5, §6, §7, §10, §11, §12, §13, §15, §16 Slice 2
**Prior review:** `git show origin/review/slice-1:REVIEW-slice-1.md`

## Verdict

**Reject.**

Everything the implementer claimed about *counts* reproduced exactly: 441
top-level tests, 947 with subtests, 8 live-gated, `devbox run check` green,
fixture validation green against the pinned upstream tree, `no-real-numbers`
clean, `pin-consistency` clean. The store, the credential layer, the strict
parameter rejection, the erasure order, the ticket system, the ambiguity rule,
sign-out/remove and the audit trail are all real and all held up when I drove
them myself.

But the slice is rejected on three findings that a package-scoped test suite
cannot see and that I reproduced against a running server:

- **R-1** — `agent-gm serve` wires **no `Backfiller` and no `Sweeper`**. §5's
  backfill and §5.4's reconciliation sweep — the sweep D26 calls mandatory
  because the event stream is *lossy* — never run in the shipped binary. Both
  are tested with test doubles and pass.
- **R-2** — `apierr.From` does not translate a `*gm.Error`. **Every** §7.2
  Google-layer code (`phone_not_responding` 504, `google_undocumented_status`,
  `not_default_sms_app`, `config_version_stale`, `disconnected`, every
  `pairing_*`) reaches a caller as `internal_error` 500.
- **R-3** — `agent-gm serve` never resumes accounts from `sessions/`, and
  `POST /v1/pairing/start` is unusable with `AGENT_GM_BACKEND=fake`, so the
  binary this slice ships cannot survive a restart and cannot be driven
  end-to-end with the fake as §13.1 promises.

**The coordinator must not run the live gate on this build.** R-1 means the
owner's history is never backfilled after `agm pair`, so test 45's
`agm messages list` shows only what live events happen to bring in. R-3 means
a server restart during the gate silently drops every account. R-2 means any
failure during the gate is reported as `internal_error` with no diagnosis and
nothing in the log. §16 Slice 2 tests 46 and 47 have no live test at all.

---

## Evidence I ran

```
$ devbox run check
agent-gm devbox: go go1.27.0
0 issues.
ok  github.com/thisnick/agent-gm/cmd/agent-gm       2.980s
?   github.com/thisnick/agent-gm/cmd/agm            [no test files]
ok  github.com/thisnick/agent-gm/internal/acceptance 1.014s
ok  github.com/thisnick/agent-gm/internal/accounts  9.677s
ok  github.com/thisnick/agent-gm/internal/api      12.430s
ok  github.com/thisnick/agent-gm/internal/apierr    1.235s
ok  github.com/thisnick/agent-gm/internal/audit     1.028s
ok  github.com/thisnick/agent-gm/internal/authz     6.174s
ok  github.com/thisnick/agent-gm/internal/cli       3.292s
?   github.com/thisnick/agent-gm/internal/clock     [no test files]
?   github.com/thisnick/agent-gm/internal/config    [no test files]
ok  github.com/thisnick/agent-gm/internal/core     21.603s
ok  github.com/thisnick/agent-gm/internal/gm        1.062s
ok  github.com/thisnick/agent-gm/internal/gm/fake   1.029s
ok  github.com/thisnick/agent-gm/internal/logging   1.025s
ok  github.com/thisnick/agent-gm/internal/media     1.039s
ok  github.com/thisnick/agent-gm/internal/settings  1.028s
ok  github.com/thisnick/agent-gm/internal/store    15.374s
no-real-numbers: clean
EXIT=0
```

```
$ CGO_ENABLED=1 go test ./... -race -count=1 -v
=== RUN   Test...                 : 947
--- PASS (top level)              : 441
--- PASS (with subtests)          : 947
--- FAIL                          : 0
--- SKIP                          : 0
EXIT=0
```

**Counts confirmed exactly as claimed: 441 / 947 / 0 skips.**

```
$ AGENT_GM_UPSTREAM_DIR=/home/nick/code/mautrix-gmessages devbox run fixture-validation
fixture-validation: using existing checkout /home/nick/code/mautrix-gmessages
--- PASS: TestAssertion01_SectionThreeOneSignatures … TestAssertion20_CookieRefreshWithoutRepair
PASS   ok github.com/thisnick/agent-gm/internal/upstream 0.014s
```

19 test functions carrying the 20 assertions of §13.4 (03 and 12 share one),
run against `/home/nick/code/mautrix-gmessages` at `be48a58b7338…`, read-only,
verified with `git rev-parse HEAD` before the run. Also green:
`devbox run pin-consistency`, `go vet -tags live ./...`, `go vet -tags fixtures ./...`.

**The server I drove.** `agent-gm serve` against `/tmp/rs2-data` with
`AGENT_GM_BACKEND=fake` + `AGENT_GM_ALLOW_FAKE=1`, and — because pairing is
impossible under the fake (R-3) — a reviewer harness (`cmd/zzreview`, deleted
afterwards) that is `serve.go` byte for byte except that `NewBackend` hands out
a `fake.Backend` and that it re-adopts accounts whose session file is on disk.
Everything below is from `curl` and the real `bin/agm` against those.

---

## Findings

### R-1 — `serve` wires no `Backfiller` and no `Sweeper`; backfill and the reconciliation sweep never run (**blocker**)

**Spec:** §16 Slice 2 deliverables ("the ingest loop with backfill, live events
and the reconciliation sweep"), §5.2, §5.4, D26, §7.5, §7.6 `coverage`.

`internal/accounts/supervisor.go:97` and `:100` declare
`Sweep Sweeper` and `Backfill Backfiller`. `cmd/agent-gm/serve.go:154` builds
the supervisor with `accounts.New(st, sessions, clk, nil)` and **never assigns
either field**. Grepping the whole tree, `sup.Backfill` is assigned only in
`internal/accounts/lifecycle_test.go:994,1292,1315`, and `sup.Sweep` is never
assigned outside tests at all. `internal/core/backfill.go` implements the work
(`(*Account).Backfill`, `KickBackfill`) but **no type implements
`accounts.Backfiller`**, so there is nothing to wire even if `serve` wanted to.

Reproduced against the running server, after two accounts paired, connected and
ingested:

```
$ curl .../v1/health  | jq -c '.data.accounts[0].backfill, .data.accounts[0].sweep'
{"state":"pending","conversations_done":0,"conversations_total":0,"completed_at":null}
{"last_sweep_at":null,"sweeps_total":0}

$ sqlite3 agent-gm.sqlite3 "select count(*) from backfill_state;"
0
$ sqlite3 agent-gm.sqlite3 "select backfill_complete_at_ms, last_sweep_at_ms from accounts;"
||
```

**Production consequence, three ways.** (a) The owner's history is never
backfilled: only what the live stream happens to deliver is indexed. (b) D26's
mandatory sweep never runs, so the message *loss* the library's dedup causes
(§3.4, fixture assertion 18) is never recovered — the single reason the sweep
exists. (c) `accounts.backfill_complete_at_ms` stays NULL forever, so
`GET /v1/search/messages` returns `coverage.complete: false` and a
`history_incomplete` warning on **every** search for the life of the
deployment, which is exactly the signal §7.6 reserves for "backfill is
outstanding, do not read an empty result as an absent message".

§16 Slice 2 test 4 passes because it injects a `Sweeper`; test 2 passes because
it calls `core` directly. Neither test can see that nothing calls them in
production.

**Fix:** add a `core`-backed adapter implementing `accounts.Backfiller` and
`accounts.Sweeper`, assign both in `serve.go`, and add a test that asserts a
supervisor built the way `serve` builds it has a non-nil `Backfill` and `Sweep`
— an assertion on the wiring, not on the workers.

### R-2 — `apierr.From` drops every `*gm.Error`; the whole §7.2 Google vocabulary becomes `internal_error` 500 (**blocker**)

**Spec:** §7.2 (whole table), §16 Slice 2 tests 9, 10, 11, 21; D5.

`internal/apierr/from.go:19-30` funnels every handler error. It recognises
`*apierr.Error` and nothing else, so a `*gm.Error` — the type
`internal/gm/errors.go:44` defines and `gm.Classify` returns — falls to
`internal_error`. There is no conversion anywhere:
`grep -rn '\*gm\.Error' internal/api internal/apierr` finds nothing outside
tests.

Proven three ways.

1. Unit, against the classified values `internal/gm` itself produces:

```
gm "pairing_no_cookies"       -> apierr "internal_error"   (want pairing_no_cookies)
gm "pairing_no_devices"       -> apierr "internal_error"
gm "pairing_wrong_emoji"      -> apierr "internal_error"
gm "pairing_cancelled"        -> apierr "internal_error"
gm "pairing_timeout"          -> apierr "internal_error"
gm "phone_not_responding"     -> apierr "internal_error"
gm "disconnected"             -> apierr "internal_error"
gm "google_permission_denied" -> apierr "internal_error"
```

2. Through the REST surface, test 9 as written ("HTTP 504
`phone_not_responding` **and** an operation in `pending`"), against the
implementer's own `internal/api` harness with the fake scripted to return
`ErrPhoneNotResponding`:

```
status=500 body={"error":{"code":"internal_error", …}}
FAIL: status = 500, want 504
```

and test 11's undocumented resolve status:

```
status=500 body={"error":{"code":"internal_error", …}}
FAIL: code = internal_error, want google_undocumented_status
```

3. Against the running server, `POST /v1/pairing/start` with an incomplete
cookie set — a `pairing_no_cookies` at the `gm` layer:

```
{"code":"internal_error","message":"an internal error; the request ID identifies it in the logs",
 "retryable":true,"details":null}
```

**Production consequence.** D5's whole point is that
`ErrPhoneNotResponding` must not look like a failure; served as a `500
internal_error` marked `retryable: true` it looks like *exactly* the thing a
client should retry, which is how a real person gets the same text twice. Every
`pairing_*` refusal — wrong emoji, cancelled, timed out, wrong account, no
devices — reaches `agm pair` as an unattributable 500, so §11.4's diagnostic
messages can never fire. And `agm` maps `internal_error` to exit 10 rather than
7, 8 or 9.

Why the suite does not see it: `internal/apierr/gm_agreement_test.go` asserts
the two *vocabularies* agree and that the *statuses* agree, never that the
translation happens. `internal/cli/exitcodes_test.go:30` walks
`apierr.ExitCodes()` against a **stub server that emits the code directly**, so
test 21's "every §7.2 code produced against a fake-backed server" is not what
runs — the server's ability to produce those codes is never exercised.

**Fix:** teach `apierr.From` to recognise `*gm.Error` and carry its `Code`,
`HTTPStatus`, `Message`, `Reason`, `Retryable` and `Details` across; add the
missing-code case to `gm_agreement_test.go` as a translation test, and rewrite
test 21 to drive a fake-backed server rather than a stub.

### R-3 — `serve` never resumes accounts, and the fake backend cannot pair over REST (**blocker**)

**Spec:** §13.1 ("With both set, the CLI, the REST suite and the MCP
conformance run all drive a real server with no phone"), §4.7 (re-pair /
restart), §16 Slice 2 test 1.

`cmd/agent-gm/serve.go:49` says step 5 is *"resume the accounts, then bind"*.
There is no resume: no `Adopt`, no `Start`, no read of `sessions/`. `cmd/agent-gm/spike.go:182`
has the resume; `serve` does not. After a restart every account row is still in
the database and still listed, but no account holds a backend, so every write
fails and no event stream runs.

Separately, `serve.go:186-190` makes `NewBackend` return an error under
`AGENT_GM_BACKEND=fake`, so `POST /v1/pairing/start` is a hard 500 and **no
account can ever be created against a fake-backed server**:

```
$ curl -X POST .../v1/pairing/start -d '{"cookies":{…seven…}}'
{"data":{"pairing_id":"pair_…","state":"failed","emoji":null,"account_id":null,
 "error":{"code":"internal_error", …}}}
```

`internal/api/slice2_harness_test.go:167-176` re-adopts accounts on "restart"
and `:190` creates account rows with `UpsertAccount` directly, so test 1's
restart is asserted against a shape production does not have, and
`POST /v1/pairing/start` is covered only by the route-inventory table
(`internal/api/routes_test.go:42`) — never driven.

**Fix:** give `serve` the resume `spike` already has; under
`AGENT_GM_BACKEND=fake` return a seeded `fake.Backend` from `NewBackend` (its
address settable from the environment) instead of an error; add one test that
pairs, restarts and sends through the real `serve` wiring.

### R-4 — raw Google participant IDs are served on the message and reaction DTOs

**Spec:** §4.1 ("A caller never sees a raw Google conversation ID, message ID
or participant ID on a public surface"), §7.6 DTOs, §12.2, §18.1 rubric.

`internal/api/dto.go:282` copies `store.Reaction.ParticipantID` straight
through, and the message DTO's `sender.id` does the same. Against the running
server:

```
$ curl .../v1/messages | jq -c '.data.items[0].sender'
{"id":"me@Owner-A@example.test","is_me":true}

$ curl .../v1/messages/$M | jq -c '.data.reactions'
[{"id":"react_…","emoji":"❤️","type":"red_heart",
  "participant_id":"me@Owner-A@example.test","is_mine":true}]
```

§7.6 specifies `"sender": {"id": "part_…"}` and
`"participant_id": "part_…"`. The conversation DTO gets this right
(`dto.go:180` derives `store.ParticipantID`); the message and reaction DTOs do
not. In this deployment the raw ID embeds the owner's **Google account
address**, which §12.2 says is served on `/v1/accounts` and `/v1/health` "and
nowhere else".

**This also silently breaks the whole `sender` filter family.**
`internal/store/queries.go:247-269` matches
`m.sender_participant IN (SELECT p.id FROM participants …)` — a `part_` ID —
while ingest writes the raw source ID into that column. So:

```
$ curl '.../v1/messages'            | jq '.data.items|length'   → 3   (all outgoing, all is_me)
$ curl '.../v1/messages?sender=me'  | jq '.data.items|length'   → 0
```

`sender=me`, `sender=<E.164>` and `sender=<part_ id>` return an empty page
rather than an error, on every listing and on search. §7.6 says `sender=me`
"is never ambiguous and never an error" — it is instead always empty.

**Fix:** derive `part_` IDs at ingest (write `store.ParticipantID(convID,
sourceID)` into `messages.sender_participant` and `reactions.participant_id`,
with a migration for existing rows), or map at the DTO boundary *and* fix the
query predicates. Add a test that asserts every ID on a served DTO carries a
declared §4.1 prefix, and one that asserts `sender=me` returns the outgoing
messages it just wrote.

### R-5 — §16 Slice 2 test 35 now contradicts §6.3, and the test named for it asserts the opposite

**Spec:** §6.3 ("the mirror hazard"), §4.2 `operations` UNIQUE clause, §16
Slice 2 test 35.

§6.3 was amended in this slice to say that reusing an idempotency key against a
different account is `invalid_request`. The implementation does exactly that,
and correctly:

```
$ curl -X POST .../v1/conversations/$C2/messages -d '{"text":"…","client_request_id":"send-1"}'
{"error":{"code":"invalid_request","message":"client_request_id \"send-1\" was already used
 for this kind against account acct_5b79…; reusing it against acct_ce9b… is not a replay --
 it would send a second real message to a real person. …",
 "details":{"account_id":"acct_5b79…","field":"client_request_id","other_account_id":"acct_ce9b…"}}}
```

But §16 Slice 2 test 35 still reads *"The same `client_request_id` sent to two
accounts creates two operations and calls each backend once"*, and §4.2's
schema comment still reads *"the same key sending to two accounts is two
operations"*. `internal/core/slice2_operations_test.go:117-139` is **named**
`TestSlice2Test35IdempotencyIsPerAccount` and asserts `invalid_request` — the
opposite of the acceptance test it claims. This deviation is not in the
implementer's recorded list.

**Fix:** amend §16 Slice 2 test 35 and the §4.2 comment to state the refusal,
citing §6.3, in the same commit as any further work here.

### R-6 — a backup is a full copy of every message and is written world-readable

**Spec:** §4.5 ("`agent-gm.sqlite3` … `0600`", "tightens them on every start"),
§12.1 ("the file mode *is* the at-rest model for every message the owner has
ever sent or received"), §15.2.

```
$ curl -X POST .../v1/admin/backup
{"path":"/tmp/rs2-data2/backups/agent-gm-20260906T220359Z-9f5cc5e1.sqlite3", …}
$ ls -la /tmp/rs2-data2/backups/
drwx------  … .
-rw-r--r--  … agent-gm-20260906T220359Z-9f5cc5e1.sqlite3
```

`internal/store/backup.go` never chmods the file it creates, and
`store.CheckPermissions` (`internal/store/store.go:178-208`) checks the backups
*directory* but never the files in it, so the startup warning §4.5 promises
never fires either. The parent directory is `0700`, which limits the damage on
this host but is not the model §4.5 states, and a backup copied out of that
directory carries its mode with it. §16 Slice 2 test 43 asserts the file opens
standalone and that pruning keeps `backup.keep` — never its mode.

I did verify the deviation the implementer recorded: `VACUUM INTO` produces a
sound standalone copy (`pragma integrity_check` = `ok`, `user_version` = 3,
row counts match).

**Fix:** create the backup `0600`, chmod it after `VACUUM INTO`, extend
`CheckPermissions` to walk `backups/`, and extend test 43 to assert the mode.

### R-7 — live gates 46 and 47 do not exist

**Spec:** §16 Slice 2 tests 46 (`agm pair --paste`, and a paste missing `OSID`)
and 47 (`agm pair --refresh-cookies`, `pairing_wrong_account`, and D33's
short-lived profile gone from the filesystem).

`internal/livegate/slice2_live_test.go` holds three functions —
`TestSlice2LiveDirectConversationWalk`, `TestSlice2LiveMediaSend`,
`TestSlice2LiveGroupStart` — covering tests 45 and 48. Tests 46 and 47 have no
live test in either live file. The claim of "8 live-gated" is accurate as a
count (5 carried from Slice 1, 3 new) but three of the four Slice 2 gates are
covered by three tests and two gates by none.

**Fix:** write the two missing live tests, or mark 46 and 47 `gap` explicitly in
the slice report so the coordinator knows they are unscripted.

### R-8 — `docs/cli.md` can document a command that does not exist (**SURVIVED plant P10**)

**Spec:** §16 Slice 2 test 27, §11.3.

`internal/api/docs_test.go:291-307` checks one direction only — every command
and flag must *appear somewhere* in `docs/cli.md`, by substring. Nothing checks
the reverse. I appended to `docs/cli.md`:

```markdown
## agm conversations purge

`agm conversations purge <conversation_id>` — permanently erases a thread from every device.
```

```
ok  github.com/thisnick/agent-gm/internal/api  9.278s
ok  github.com/thisnick/agent-gm/internal/cli  3.196s
```

A destructive command that does not exist, documented with a scope claim that
contradicts D14 and §7.7's `effect` sentence, and the suite is green. Renaming
the `agm conversations list` synopsis to `agm conversations ls` also survives,
because "conversations list" still occurs elsewhere on the page.

**Fix:** parse `docs/cli.md`'s command headings and synopsis lines the way
`documentedRoutes` parses `docs/api.md`, and fail on a documented command with
no entry in the CLI inventory.

### R-9 — an `internal_error` is never logged, though its message says it is

`internal/api/server.go`'s log hook is wired to `log.Debug()` in
`cmd/agent-gm/serve.go:189-196`. At the default `AGENT_GM_LOG_LEVEL=info` the
500s of R-2 produced **not one line** in the server log:

```
$ tail -6 /tmp/rs2-server.log
INF starting  backend=fake client_source_mode=socket_peer public_url=…
INF listening addr=127.0.0.1:18999
```

while `"an internal error; the request ID identifies it in the logs"` went out
to the caller five times. An unattributable 500 is the one error an operator
cannot diagnose from the outside, so it is the one that must always be logged.

**Fix:** log an `internal_error` at `error` level with its `request_id` and the
wrapped `Err`, unconditionally.

### R-10 — smaller contract deviations

| # | Spec | Observed | Fix |
|---|---|---|---|
| a | §16 Slice 2 test 22: a widening refresh is **`invalid_scope`** | `invalid_request` (`"body field \"scopes\" must be a subset …"`) | either add `invalid_scope` to the §7.2 table or amend test 22 — as it stands the acceptance test names a code the server cannot emit. The *substance* is right: the widening was refused and did **not** spend the presented token (I refreshed with it successfully afterwards) |
| b | §4.4: every JSON surface renders timestamps "RFC 3339 UTC with **millisecond** precision" | the SSE `account.state_changed` event carries `"at":"2026-09-06T22:03:47.765215213Z"` (nanoseconds) | truncate to milliseconds in the event encoder |
| c | §4.7: `state_reason` is a short string **or `null`**; §7.5's account object is "the same per-account object `GET /v1/health` embeds" | `GET /v1/accounts` serves `"state_reason":null,"label":null`; `GET /v1/health` serves `"state_reason":"","label":""` for the same account (`internal/accounts/lifecycle.go:283,285,325,327`) | make the health block use the same nullable encoding |
| d | §7.6: search returns `results[{message, rank, snippet, conversation}]` | it returns `data.items[…]` (correct per §7.1, which says a listing puts rows in `data.items`) | the two clauses contradict each other; amend §7.6 to say `items`, and say so in `docs/api.md` |
| e | §7.1: an unknown **query parameter** is named in `details.parameter` | a tampered or mismatched `cursor` is reported as `details.field` even when it arrived as a query parameter | report `parameter` for query-sourced values |
| f | §12.4 names `auth.admin_session_narrowed` as an audit kind | declared at `internal/audit/kinds.go:39` and `internal/authz/audit.go:15`, **never emitted** — a narrowed mint writes `auth.admin_session_minted` with `narrowed:true` | emit it, or delete the constant and drop it from §12.4 |
| g | §4.1's prefix table declares no pairing prefix | `internal/api/pairing.go:154` mints `pair_` | add the `pair_` row to §4.1 |
| h | §7.7: `POST /v1/conversations/{id}/messages` answers `{operation, message_id}` | answers `{operation, changed, conversation_id}` — no top-level `message_id` | add it (`null` until the echo lands, per §6.5) |
| i | §10.2 step 3 / §16 test 17 | a `send_media` succeeds but the resulting local message row carries `attachments: []`; I could not tell whether the fake simply does not echo media, so this is **unverified**, not a finding | add a test asserting the sent message carries its `att_` row |

---

## What I verified myself, end to end

Against a running server with two fake accounts, driven with `curl` and the
real `bin/agm`:

- **File modes (§4.5).** Started against a pre-existing `0777` data directory;
  after start: `drwx------` data dir, `-rw-------` on `agent-gm.sqlite3`, `-wal`,
  `-shm` and `sessions/`. Tightening on every start, not only at creation, is
  real. (Backups excepted — R-6.)
- **`/healthz` (§7.5).** `200 {"status":"ok"}`, unwrapped, no envelope. The
  contract bug the implementer reported fixed is fixed.
- **IDs (§4.1).** `auth_01a078b6-679e-**7**750-…` — `auth_` prefix, UUIDv7.
  `acct_` is UUIDv5 of the lowercased address; pairing `Owner-A@example.test`
  and lowercase produce the same `acct_`. `source_url` on `/v1/health` is
  populated and carries the running commit.
- **Strict parameter rejection (§7.1, test 12).** Route by route:
  `?_=1`, `?directon=incoming`, `?nope=1`, `?bogus=1`, `?zz=1`, `?x=1` on
  conversations, messages, contacts, search, operations, accounts, health,
  admin settings, admin audit, admin diagnostics and whoami — all
  `400 invalid_request` naming `details.parameter`. An unknown body field is
  `details.field`. `?client_request_id=` is `invalid_request` naming it (§7.1).
- **Wrong prefix (test 13).** `GET /v1/conversations/msg_…` and
  `GET /v1/conversations/12345` are both
  `invalid_request` / `details.expected_prefix: "conv_"` — never `not_found`.
- **Cursors (§7.4, test 14).** A cursor issued without `folder` replayed with
  `folder=active` is `invalid_request` naming the mismatch; a tampered cursor is
  `invalid_request`; replaying an untampered cursor returns the next page.
- **§12 credential protection.** A narrowed session (`scopes:["messages:read"]`,
  `narrowed:true`) gets `403 insufficient_scope` on `/v1/admin/settings` with a
  `WWW-Authenticate: Bearer realm="agent-gm", error="insufficient_scope",
  scope="admin", resource_metadata=…` challenge. A widening refresh is refused
  **and does not spend the token** (the same token then refreshed
  successfully). Reusing a *spent* refresh token revokes the family — the next
  call with the previously-issued access token is `401 invalid_token` and
  `auth.refresh_token_reuse_detected` is audited with `tokens_revoked: 4`. Six
  wrong secrets in a row: five `401`, then `429 rate_limited` with
  `Retry-After: 60`, and **the correct secret is refused too**; after `kill`
  and restart the correct secret is **still** `429` — the durable cooldown
  survives, tests 24 and 25.
- **Audit (§12.4).** `auth.admin_session_minted` (with `narrowed`),
  `auth.admin_session_refreshed` (scopes before/after),
  `auth.admin_secret_failed` (source, failures_in_window, cooldown state),
  `auth.refresh_token_reuse_detected`, `account.state_changed`,
  `account.removed`. I read every payload: **no presented value, no secret, no
  cookie, no token** in any of them.
- **Rate limits (§12.3, test 30).** 330 reads: 109 served, 221
  `429 rate_limited` with `Retry-After`.
- **Refuses to start (§15.1).** Invalid `AGENT_GM_TRUSTED_PROXY_CIDRS`, a
  5-character `AGENT_GM_ADMIN_SECRET`, and `AGENT_GM_BACKEND=fake` without
  `AGENT_GM_ALLOW_FAKE=1` each refuse with a message naming the variable and
  never its value.
- **Ambiguity (§7.3, test 34).** With two accounts a write omitting
  `account_id` is `invalid_request` with `details.field: "account_id"` and
  `details.accounts` listing both as `{id, google_account, state}`; retrying
  with one succeeds. A read omitting it covers both accounts.
- **Idempotency (§6.3, test 7).** Same key + same body returns the same
  `op_` ID; same key + different body is `idempotency_conflict`; the key as a
  query parameter is `invalid_request`.
- **Media (§10.2, test 17).** Reserve → `201` with `upload_url` built from
  `AGENT_GM_PUBLIC_URL`, `token_audience: "upload:upl_…"`, `expires_in_seconds:
  7199`, and a ready-made `curl` line. `PUT` the exact bytes →
  `{"state":"complete", …}`. A **second** `PUT` with the same token →
  `401 invalid_token`. Sending the upload → `send_media` succeeds; a second send
  of the same upload → `invalid_request`.
- **Reactions (§7.6, test 15).** `POST …/reactions` with `❤` (no VS16) stores
  `react_cc6f68d2-…` and serves `"emoji":"❤️","type":"red_heart"`;
  `DELETE …/reactions/❤️` (VS16, percent-encoded) removes it — both directions
  canonicalise to one `react_` ID. A second, different emoji from the same
  person **replaces** the first: one row, `type` changes to `laugh`.
  `DELETE /v1/reactions/{react_id}` removes by ID.
- **`PATCH /v1/conversations/{id}` (test 16).** First archive:
  `changed: true`, `operation.kind: "archive"`. Repeat: `changed: false`,
  `operation: null`.
- **Delete-for-me (§7.7, test 28).** `agm messages delete` without `--yes`
  prints the effect sentence and aborts with "nothing was done". With `--yes`
  it prints, **after the call**, `deletes this message from your Google
  Messages account only; the recipient keeps it` — byte for byte the response's
  `effect` field. §11.3 records the compiled-constant deviation honestly and
  test 28 is still satisfiable as written.
- **Sign-out (§4.7, test 31).** `POST /v1/accounts/{a}/sign-out` shreds
  `sessions/{a}.enc` (only account b's file remains on disk), sets
  `state: "signed_out"` and deletes **zero** rows — `GET /v1/messages?account_id=a`
  still returns all 3. A write naming `a` is
  `409 unsupported_capability` with `details.reason: "not_signed_in"` — **not**
  `not_paired` — carrying `object_id`, `action`, `capability` and
  `capability_value`. Account `b` still sends. The SSE stream on
  `GET /v1/accounts/events` delivered
  `event: account.state_changed / {"from":"connected","to":"signed_out",
  "state_reason":"credentials", …}`.
- **Removal (§4.7, test 33).** `DELETE /v1/accounts/{a}` without
  `{"confirm": true}` is `invalid_request` and deletes nothing (3 messages still
  there). With it: `deleted_counts` `{conversations:1, participants:2,
  messages:3, operations:11, …}`, `media_files_unlinked: 0`, and the exact
  effect sentence of §4.7. The account is then `404`; account `b` is untouched;
  and `GET /v1/admin/audit?kind=account.removed` **still returns the row**,
  carrying `account_id` and the counts.
- **Backup (test 43).** File opens standalone: `integrity_check` = `ok`,
  `user_version` = 3, `accounts` = 1, `messages` = 1.
- **Settings (§7.7).** `GET /v1/admin/settings/{key}` serves value, source
  (`default`), bounds, scope, type, mutability and `requires_restart`;
  `PATCH` returns `{items:[{key, from, to, requires_restart}]}`.
- **Slice 1 findings closed.** F-1/F-2 (`--user-data-dir`, the two-host cookie
  read) now have tests at `internal/cli/capture_internal_test.go:23`; F-3 (the
  SQL transition trigger) at `internal/store/store_test.go:634`; F-4
  (`pairing_no_account`) at `internal/gm/pairing_internal_test.go:61` and
  `internal/gm/fake/fake_test.go:231`; F-8's four dangling `devbox.json`
  scripts now name the slice they arrive in and exit `2`.

---

## Plant table

Ten mutations, all mine, none from the implementer's list. Each was applied
with `perl -0pi`, the named packages run with `-race -count=1`, then reverted
with `git checkout --`.

| # | Site | Mutation | Result |
|---|---|---|---|
| P1 | `internal/authz/compare.go:94` | `hmac.Equal(presentedDigest[:], storedDigest[:])` → `presentedDigest == storedDigest` | **killed** — `TestSlice2Test23ComparisonSitesAreConstantTime` |
| P2 | `internal/store/ids.go:66` | drop `accountID` from the `msg_` derivation | **killed** — `TestIDDerivationComputedIndependently`, `TestSameAccountDifferentPhoneKeepsEveryID`, `TestTwoAccountsDoNotCrossTalk`, `TestSlice2TwoAccountsDoNotCrossTalk` |
| P3 | `internal/store/cursor.go:142` | drop `fingerprint` from the signed cursor payload | **killed** — `TestCursorIsBoundToTheFilterSetAsWritten`, `TestCursorRoundTrips`, `TestPaginationAcrossTenEqualTimestamps`, `TestSlice2_1_ListingSurvivesARestartWithIdenticalOrdering` |
| P4 | `internal/store/queries.go` (`ConversationQuery.where`) | drop the `c.account_id = ?` predicate | **killed** — `TestConversationListingFiltersInBothForms`. Note: `internal/api` stayed **green**, so no REST-level test asserts that `?account_id=` actually filters |
| P5 | `internal/core/send.go:46` | `gm.SendRetryBackoff[i]` → `[0]` (every retry waits 3s) | **killed** — `TestTransientSendIsRetriedOnTheBackoffSchedule`, `TestTransientSendGivesUpAfterTheWholeSchedule`, `TestSlice2Test10SendRetriesThroughTheOperationPipeline/failure_2_twice_then_success_reuses_one_tmp_id` |
| P6 | `internal/store/media.go:290` | `redemptions = redemptions + 1` → `redemptions = redemptions` | **killed** — `TestDownloadTicketRedeemsFiveTimesAcrossARestart`, `TestSlice2_18_ATicketRedeemsFiveTimesAndTheSixthFailsAcrossARestart`, `TestSlice2_18_NarrowingTheIssuingSessionKillsItsTickets` |
| P7 | `internal/store/erasure.go:61` | never collect the media paths inside the transaction | **killed** — `TestErasureCollectsPathsInsideTheTransactionAndUnlinksAfterTheCommit`, `TestEraseAccountPurgesOnlyThatAccount`, `TestSlice2_20_ErasureOrderLeavesAnOrphanFileAndNeverAnOrphanRow` |
| P8 | `internal/api/strict.go:33` | allowlist the cache-busting `_` query parameter | **killed** — `TestEveryRouteRejectsAnUnknownQueryParameter` (every route subtest) |
| P9 | `internal/apierr/effect.go:17` | effect sentence → "deletes this message everywhere, including for the recipient" | **killed** — `TestEffectSentencesAreTheSpecsWords`, `TestEffectSentencesSayWhatIsNotAffected/message_delete`. Note: `internal/api` and `internal/cli` stayed green, so only `apierr`'s own spec-text test defends the wording |
| P10 | `docs/cli.md` | (a) rename the `agm conversations list` synopsis to `ls`; (b) add a whole fictitious `agm conversations purge` section claiming it "permanently erases a thread from every device" | **SURVIVED** both ways — `internal/api` and `internal/cli` green. See R-8 |

Nine of ten killed by named tests. P4 and P9 are killed only at the layer
below REST, which is worth knowing but is not a finding on its own.

---

## Rubric — §18.1: *every name means what it means in Google Messages*

**Score: 1.**

Almost everything is right, and several things are better than the spec asked
for. `conversation` is a thread; `type` is `sms_mms` | `rcs`; `folder` is
`active` | `archived` | `spam_blocked`; `send_mode_raw` is never served and
`capabilities.force_rcs` is served instead (§4.6); `delete` is delete-for-me
everywhere and says so in the same words on the route, the CLI prompt and the
CLI result; `participants` (people in a thread) and `recipients` (numbers you
address) are kept distinct; `auth logout` ends a *token's* session and
`accounts sign-out` signs a *Google account* out, and neither name borrows the
other's; `parked` with `state_reason: "capacity"` is distinguishable from
`degraded`; `not_signed_in` is used where §7.8 says and `not_paired` is not; no
Matrix term survives anywhere I looked.

It scores 1 rather than 2 for one reason, and it is the rubric's own words:
**"no SQLite or `gmproto` internal reaches a public surface."**

- Exact failing surfaces: `GET /v1/messages`, `GET /v1/messages/{id}`,
  `GET /v1/conversations/{id}/messages`, `GET /v1/messages/{id}/context` and
  `GET /v1/search/messages` serve `sender.id` as a **raw Google participant
  ID**; the same routes plus `POST /v1/messages/{id}/reactions` serve
  `reactions[].participant_id` the same way.
- Smallest change that fixes it: in `internal/api/dto.go`, render both through
  `store.ParticipantID(conversationID, sourceID)` — the derivation the
  conversation DTO already uses at `dto.go:180` — and fix the matching
  predicates in `internal/store/queries.go:246-269` so `sender=` keeps working.

Secondary rubric notes, not enough to move the score on their own: `sender=me`
silently returns nothing (R-4), which asserts a Google behaviour — "the owner
sent nothing" — that is false; and `docs/cli.md` may claim a delete scope
Google does not offer (R-8).

---

## Clause-by-clause verification

| Clause | Status |
|---|---|
| §4.1 ID scheme, prefixes, UUIDv5/v7, wrong-prefix is `invalid_request` | **partial `03c0349`** — prefixes and derivations verified; `pair_` undeclared (R-10g); raw participant IDs served (R-4) |
| §4.2 schema, both index forms | verified `03c0349` (plants P2, P4; `queryplan_internal_test.go`) |
| §4.3 forward-only migrations, `foreign_key_check`, higher `user_version` refuses | verified `03c0349` (`migrations_forward_internal_test.go`, populated-older-DB case included) |
| §4.4 delivery vocabulary, transitions, backward move audited | verified `03c0349` (`store_test.go:634` trigger/Go agreement; fixture assertions 3/12) |
| §4.4 millisecond rendering on every JSON surface | **failed `03c0349`** — SSE `at` is nanoseconds (R-10b) |
| §4.5 data key, file modes tightened on every start | verified `03c0349` — except backups (R-6) |
| §4.7 sign-out keeps everything; remove is the only purge; parking; `state_reason` | verified `03c0349` (tests 31, 33, 36, 37, 38 driven or read) |
| §5.2 backfill, §5.4 reconciliation sweep — **in the shipped server** | **failed `03c0349`** (R-1) |
| §6.3 idempotency, mirror hazard, two transports | verified `03c0349`; spec self-contradiction (R-5) |
| §6.4/§6.5 operation status and object | verified `03c0349` — except the missing `message_id` on the send response (R-10h) |
| §6.6 crash recovery settles `running` → `unknown`, nothing resent | verified `03c0349` (`core.RecoverOperations` runs before the listener binds in `serve.go:121`) |
| §7.1 envelopes, `data.items`, strict rejection, normalisation warnings | verified `03c0349` — `details.field` vs `parameter` for cursors (R-10e) |
| §7.2 error taxonomy **as served** | **failed `03c0349`** (R-2) |
| §7.3 choosing an account | verified `03c0349` |
| §7.4 pagination, signed cursors bound to the filter set | verified `03c0349` |
| §7.5 health, auth, accounts, pairing routes | **partial `03c0349`** — `/healthz`, health, accounts, sign-out, remove, SSE verified; pairing unusable under the fake (R-3); `label`/`state_reason` encoding differs between the two account surfaces (R-10c) |
| §7.6 reads, search, matching across accounts | **partial `03c0349`** — listings, filters, search and cross-account matching verified; `sender=` broken (R-4); `results` vs `items` (R-10d) |
| §7.7 writes, `effect`, admin subtree | verified `03c0349` |
| §7.8 `unsupported_capability` reasons | verified `03c0349` (`not_signed_in` driven; vocabulary closed by `TestReasonVocabularyIsClosed`) |
| §10.1 download tickets, 5 redemptions, re-checked authorization | verified `03c0349` (plant P6 + tests 18) |
| §10.2 upload reserve/PUT/send, account-agnostic | verified `03c0349` |
| §10.3 ticket rules, `AGENT_GM_PUBLIC_URL`, erasure order | verified `03c0349` (plant P7 + test 19) |
| §11.1/§11.3 CLI inventory, `--json`, effect prompt | verified `03c0349` |
| §11.2 exit codes | **partial `03c0349`** — the CLI's mapping is exhaustively tested against a stub; the server's ability to produce those codes is not (R-2) |
| §11.4 pairing flow, `--paste`, `--refresh-cookies`, D33 | **unverified** — no live test (R-7); D33's short-lived profile is implemented (`internal/cli/chrome.go`) but only asserted in unit tests |
| §12.1 constant-time comparison, secret handling | verified `03c0349` (plant P1) |
| §12.2 two loggers, never stdout, redaction, sentinel scan | verified `03c0349` (`internal/acceptance/sentinels*`, `internal/logging`) — but see R-9 |
| §12.3 rate limits, trusted-proxy resolution | verified `03c0349` (limits driven; proxy resolution by test + the refuse-to-start case driven) |
| §12.4 audit | verified `03c0349` — except `auth.admin_session_narrowed` (R-10f) |
| §13.1 the fake selects at runtime and drives a real server | **failed `03c0349`** (R-3) |
| §13.4 fixture validation against `be48a58` | verified `03c0349` |
| §15.1 refuses to start on a mistyped environment | verified `03c0349` |
| §16 Slice 2 tests 1, 12, 13, 14, 15, 16, 17, 18, 19, 20, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 33, 34, 41, 42, 43, 44 | verified `03c0349` |
| §16 Slice 2 tests 2, 3, 5, 6, 8, 21, 32, 36, 37, 38, 39, 40 | verified `03c0349` at the layer they are written for; not re-driven through REST by me |
| §16 Slice 2 tests 4, 9, 10, 11, 35 | **failed `03c0349`** — 4 (R-1), 9/10/11 (R-2), 35 (R-5) |
| §16 Slice 2 tests 45, 48 | **unverified** — live, coordinator only |
| §16 Slice 2 tests 46, 47 | **gap `03c0349`** (R-7) |
| MCP (§8), OAuth (§9), packaging (§14) | **n/a** — Slice 3 and Slice 4 |
| `devbox run conformance`, `lint-names`, `build-matrix`, `image` | **n/a** — each exits `2` naming the slice it arrives in |

---

## Required fixes, in order

1. **R-1** — wire a `Backfiller` and a `Sweeper` into `serve`, with a test on
   the wiring.
2. **R-2** — make `apierr.From` translate `*gm.Error`; re-point test 21 at a
   fake-backed server.
3. **R-3** — resume accounts in `serve`; make the fake pairable over REST.
4. **R-4** — serve `part_` IDs on `sender` and `reactions[].participant_id`,
   and fix the `sender` predicates.
5. **R-5** — amend §16 Slice 2 test 35 and the §4.2 comment to match §6.3.
6. **R-6** — create backups `0600` and check them at startup.
7. **R-8** — make `docs/cli.md` drift fail in both directions.
8. **R-9** — log `internal_error` at `error` level.
9. **R-7** — write live tests 46 and 47, or record them as `gap`.
10. **R-10** a–i — the smaller deviations, each with the spec edit or the code
    edit named in the table.

## Live gate

**Not yet.** R-1, R-2 and R-3 must land first. R-1 makes the gate's read half
meaningless, R-3 makes a restart during the gate destructive, and R-2 makes any
failure during the gate undiagnosable. Once those three are fixed I would run
`devbox run check`, re-drive the §12 credential set, and clear the gate.

## Housekeeping

Working tree clean at `03c0349` apart from this file. Every plant reverted with
`git checkout --`; the reviewer harness (`cmd/zzreview`) and three temporary
test files were deleted. No process was killed by name or pattern — only PIDs I
started. No pairing, no Google contact, no message to any number, and no real
phone number anywhere in this file.

---

# Re-review — `3b0b67f`

**Range:** `03c0349..3b0b67f` (2 commits, 38 files, +1731/-69)
**Verdict: accept with required fixes.**

All three blockers are genuinely fixed, and I verified each one by driving the
real `bin/agent-gm serve` binary rather than a harness — which was the whole
point of the first review and is now possible for the first time. R-10f I
withdraw: the implementer is right and I was wrong. R-10i's fix uncovered a
larger hole (**R-11**), and two more findings fall out of the fixes themselves
(**R-12**, **R-13**), plus one fake-fidelity gap (**R-14**).

**The coordinator may run the live gate on this build**, with one caveat under
R-11: any attachment *download* during the gate will fail with
`unsupported_capability` / `media_pending`. Tests 45–48 as written do not
download, so the gate can proceed; R-11 and R-13 must land before the slice is
tagged.

## Evidence I ran at `3b0b67f`

```
$ devbox run check
0 issues.  … all packages ok …  no-real-numbers: clean   EXIT=0

$ CGO_ENABLED=1 go test ./... -race -count=1 -v
--- PASS (top level) : 453
--- PASS (with subtests) : 974
--- FAIL : 0        --- SKIP : 0        EXIT=0

$ grep -rhn "^func Test" $(grep -rl "go:build live" --include=*.go .) | wc -l
11
```

**Counts confirmed exactly as claimed: 453 / 974 / 11 live-gated.**

## The three blockers, verified against the real binary

**R-1 — fixed.** `internal/accounts/workers.go` is a real `Backfiller` and
`Sweeper`; `serve.go:214-216` assigns both. Against `bin/agent-gm serve` with
two accounts paired through the REST route:

```
$ curl .../v1/health | jq -c '.data.accounts_summary, (.data.accounts[]|{backfill,sweep})'
{"total":2,"connected":2,…,"backfill_complete":2}
{"backfill":{"state":"complete",…,"completed_at":"2026-09-06T22:47:36.248Z"},
 "sweep":{"last_sweep_at":"2026-09-06T22:47:36.247Z","sweeps_total":1}}

$ curl '.../v1/search/messages?q=hello' | jq -c '{coverage:.data.coverage,warnings:.warnings}'
{"coverage":{"complete":true,…},"warnings":[]}
```

`coverage.complete` is `true` and the permanent `history_incomplete` warning is
gone. The two extra bugs found while writing the test (`Pause` before
`ClientReady`, `Progress` building an engine to reach the store) are the right
kind of find.

**R-2 — fixed.** `apierr.From` now calls `fromGM`. Driven through the REST
surface, test 9 in full:

```
status=504 {"error":{"code":"phone_not_responding","message":"…the operation stays
 pending -- do not resend","retryable":true}}
operations → {"status":"pending","terminal":false,
              "error":{"code":"phone_not_responding","retryable":true}}
```

and test 11, plus two more I picked:

```
status=502 {"code":"google_undocumented_status","details":{"status":77}}   ← bare integer, no invented name
status=503 {"code":"disconnected","retryable":true}
status=502 {"code":"google_permission_denied","retryable":false}
```

Live, through `POST /v1/pairing/start` with one cookie:

```
{"code":"pairing_no_cookies","details":{"missing_cookies":["HSID","OSID","SSID","APISID","SAPISID"]}}
```

**R-3 — fixed.** `POST /v1/pairing/start` under `AGENT_GM_BACKEND=fake` pairs
two distinct accounts through the real server, and after `kill` + restart:

```
INF resumed accounts=2
$ curl .../v1/accounts   → both "connected"
$ curl -X POST .../v1/conversations/{id}/messages  → {"op":"succeeded"}
```

## Also verified fixed

| # | Evidence |
|---|---|
| R-4 (DTO half) | `sender.id` = `part_e64bbf6c-…`, `reactions[].participant_id` = the same `part_` ID. No raw Google value on any served DTO. Plant P13 (revert the derivation) is killed by `TestSlice2_R4_NoServedIDIsARawGoogleID` and `TestSlice2_R4_SenderFiltersFindWhatTheyJustWrote` |
| R-5 | §16 test 35 and the §4.2 comment now state the §6.3 refusal |
| R-6 | backup created `-rw-------`; I then `chmod 0666`'d it and restarted — `WRN … path=…/backups/… mode=0666 want=0600`. Warns, does not refuse, which is §4.5's rule |
| R-8 | both my surviving plants now die: the fictitious `agm conversations purge` fails `TestCLIDocNamesNoCommandThatDoesNotExist` **and** `TestCLIDocQuotesOnlyTheRealEffectSentences`; the `list`→`ls` synopsis rename fails the first |
| R-9 | `server.go:466-471` routes `CodeInternalError` to a dedicated `LogError` hook at error level with `request_id` and `cause`, wired in `serve.go` and asserted by `TestSlice2_R9_AnInternalErrorIsAlwaysLoggedAtErrorLevel` |
| R-10b | SSE now `"at":"2026-09-06T22:50:16.191Z"` — milliseconds |
| R-10c | `label` and `state_reason` are `null` on both `/v1/accounts` and `/v1/health` |
| R-10e | a filter-mismatched cursor and a garbage cursor are both `details.parameter: "cursor"` |
| R-10h | the send response now carries a top-level `message_id` |
| R-10a, d, g | amended in the spec |

## Withdrawn

**R-10f — withdrawn. The implementer is right.** `auth.admin_session_narrowed`
*is* emitted, on a narrowing **refresh**, which I had not exercised — I only
minted a narrowed session, which is a different event:

```
{"kind":"auth.admin_session_narrowed","payload":{"scopes_before":["admin","messages:read",
 "messages:write","messages:delete"],"scopes_after":["messages:read"], …}}
{"kind":"auth.admin_session_refreshed", … same before/after …}
```

To the direct question: **§12.4 does not want the kind on a narrowing mint.** A
mint has no "before" — there is no prior scope set to narrow *from*, so
`admin_session_narrowed` would be claiming a transition that did not happen.
`admin_session_minted` with `narrowed: true` already says the true thing: this
authorization was created smaller than the secret entitles. The current shape is
correct; §12.4 needs no edit.

## New findings

### R-11 — nothing ever sets an attachment to `available`, so `GET /v1/attachments/{id}/content` is unreachable for every real attachment (**required fix**)

**Spec:** §10.1, §4.2 `download_state`, §16 Slice 2 test 18.

`internal/core/ingest.go:191-193` writes **every** attachment with
`store.DownloadStatePending`, unconditionally, whatever its `media_id`.
`SetAttachmentDownloadState` is called from exactly one place in production
code — `internal/api/media.go:478` — and that call is guarded at `media.go:460`
and `:492` by `a.DownloadState != store.DownloadStateAvailable`, so it can only
ever re-write the state an attachment already has. **No production code path
can move an attachment to `available`.**

Driven end to end at `3b0b67f` — reserve, `PUT` the bytes, send, then fetch:

```
$ curl .../v1/conversations/{id}/messages | jq -c '.data.items[0].attachments'
[{"id":"att_31a8fcbb-…","mime_type":"image/jpeg","filename":"r.jpg","size":22,
  "download_state":"pending"}]

$ curl "$download_url" -H "authorization: Bearer $ticket"
{"code":"unsupported_capability",
 "message":"cannot download att_31a8fcbb-…: media_pending (download_state is pending)",
 "details":{"reason":"media_pending","capability_value":"pending", …}}
```

§10.1 reserves `media_pending` for an attachment whose `media_id` is empty and
whose thumbnail is set, while `GetFullSizeImage` is outstanding. Here the
`media_id` is populated and the bytes were supplied by the caller moments
earlier. So `GET /v1/attachments/{id}/content` refuses everything, `sha256` is
always `null`, and every download ticket the server mints can never be redeemed.

Why the suite does not see it: `internal/api/slice2_harness_test.go:520` calls
`SetAttachmentDownloadState(ctx, atts[0].ID, store.DownloadStateAvailable, "")`
— a transition **no production code makes**. Test 18's five redemptions, the
sha256 rules and `media_pending` itself all rest on it.

This is R-1's shape again: a state machine that exists, is tested, and is
reached only by the harness.

**Fix:** derive the state at ingest — `available` when `media_id` is non-empty,
`pending` when only `thumbnail_media_id` is, `unavailable`/`failed` from the
message's own `download_failed` states — and let the fetch path mark
`failed` on a refusal. Then re-run test 18 without the harness call, and delete
the harness call so it cannot hide the next regression.

### R-12 — the R-4 column change ships with no migration (**required fix**)

**Spec:** §4.3 ("a migration needing derived data recomputed sets
`server_meta.pending_reprocess = <task name>`; the process runs that task once
after startup and clears the key").

R-4 changed what `messages.sender_participant` and `reactions.participant_id`
*contain* — raw Google participant IDs became derived `part_` IDs — and
changed `react_` ID derivation with them (`ReactionID` now receives the `part_`
ID). `internal/store/migrations.go` is untouched: still `migration0001..0003`,
`SchemaVersion() == 3`, and `pragma user_version` on a database created at
`3b0b67f` is `3`.

So any database written before this commit keeps raw values in both columns.
On such a database `sender=me` still returns an empty page, the message and
reaction DTOs still serve raw Google participant IDs, and every stored `react_`
ID disagrees with the one a re-ingest now derives. The owner's Slice-1 live
data directory is exactly such a database.

Separately, `server_meta.pending_reprocess` is named in §4.2 and §4.3 but
**nothing in the tree reads or writes it** — the mechanism §4.3 specifies for
this exact situation does not exist.

**Fix:** migration `0004` that recomputes both columns and the affected
`react_` IDs, or that sets `pending_reprocess` and implements the reprocess
task. Add the populated-older-database case to
`migrations_forward_internal_test.go` with raw values in both columns.

### R-13 — all three blocker fixes live in the one file with no test (**required fix**; plants P11, P14, P15 all **SURVIVED**)

`cmd/agent-gm/serve.go` is where R-1, R-2's consequence and R-3 all lived, and
it is still untested. Three plants, each reverting one fix:

| Plant | Mutation | Result |
|---|---|---|
| P11 | delete `sup.Backfill = workers` (`serve.go:215`) | **SURVIVED** — `cmd/agent-gm`, `internal/accounts`, `internal/api` all `ok` |
| P14 | delete `sup.Sweep = workers` (`serve.go:214`) | **SURVIVED** — same three packages `ok` |
| P15 | make `resumeAccounts` a no-op returning `0, nil` | **SURVIVED** — same three packages `ok` |

Each restores the exact bug it fixed, and the suite stays green. The compiler
assertion the implementer added proves `Workers` *satisfies* the interfaces; it
cannot prove `serve` *assigns* them, and nothing else does either.

**Fix:** factor the wiring into a `buildServer(cfg, …)` that `runServe` calls,
and test it: assert the returned supervisor has non-nil `Backfill` and `Sweep`,
and that a directory with a session file yields a resumed, connected account.
Three assertions kill all three plants.

### R-14 — the fake's `ResolveConversation` omits the `is_me` participant, so `sender=me` cannot be verified end to end

`internal/gm/fake/fake.go:665-682` builds a conversation with
`DefaultOutgoingID: "me@"+b.address` but appends **only the recipients** to
`Participants` — unlike the harness's `seedConversation`, which adds the self
participant. So a conversation created through the real
`POST /v1/conversations` has no `is_me` row:

```
$ sqlite3 agent-gm.sqlite3 "select id,source_id,is_me from participants;"
part_2a42ea03-…|part-+12025550123|0
part_4c1a45aa-…|part-+12025550124|0
part_9e949cde-…|part-+12025550125|0

$ curl '.../v1/messages?sender=me' | jq '.data.items|length'
0                          ← the message's sender is part_e64bbf6c-…, is_me true
```

The served `sender.id` is a well-formed `part_` ID that resolves to **no
participants row**. `TestSlice2_R4_SenderFiltersFindWhatTheyJustWrote` passes
because `seedConversation` adds the self participant; the create→send→`sender=me`
path — the only one reachable through the API — is not covered, and returns an
empty page. §13.1 says the fake must "hold conversations, **participants**,
messages…", and a fake whose created conversations lack the owner is a shape
production does not have.

**Fix:** add the self participant to the conversation `ResolveConversation`
builds, and add an API test that creates a conversation, sends, and asserts
`sender=me` finds the message.

### R-15 — nit: `pairing_no_cookies` says "none were supplied" when some were

```
{"code":"pairing_no_cookies","message":"Google-account pairing requires the seven session
 cookies; none were supplied","details":{"missing_cookies":["HSID","OSID","SSID","APISID","SAPISID"]}}
```

One cookie *was* supplied. The details are right and the message contradicts
them, which is the one thing §11.4's diagnostics exist to avoid. Say "five of
the six required cookies are missing" and name them, or drop the clause.

## On `Workers.Progress` reporting `pending`

To the direct question: `pending` is the right word for a **connected** account
whose backfill has not finished, and the wrong word for an account that will
not be scheduled at all. §4.7 gave parking a state of its own precisely because
"waiting for a slot" and "retrying a listen error" are different things an
agent must be able to tell apart; the same argument applies one level down. A
`parked` account holds no goroutine and no client, so its backfill is not
pending — it is not going to happen until a slot frees. Reporting `pending`
tells a reader "wait and it will complete", which for a parked, `signed_out`,
`error` or `account_changed` account is false.

Note that `null` is *not* the answer here, even though §7.5 nulls the `google`
block for a non-connected account: §7.5's own example shows a `signed_out`
account still carrying `"backfill": {"state": "complete", …}`, because a
completed backfill stays true after the account is signed out. So:

- keep the block for every account, whatever its state;
- report `complete` whenever `backfill_complete_at_ms` is set, whatever the
  state;
- report `pending` only for an account in `connected` or `degraded`;
- add **`not_started`** for an account that has never completed and is not
  currently schedulable.

And the reason this question is hard to answer from the spec is itself worth
fixing: **§7.5 never enumerates the `backfill.state` vocabulary.** It shows
`"complete"` in one example and says nothing else. Whatever you choose, add the
closed list to §7.5 beside the account `state` vocabulary, and a
`TestBackfillStateVocabularyIsClosed` next to `TestReasonVocabularyIsClosed` —
otherwise the next value to appear will be whatever a caller happens to read.

## Plant table — re-review

| # | Site | Mutation | Result |
|---|---|---|---|
| P11 | `cmd/agent-gm/serve.go:215` | drop `sup.Backfill = workers` | **SURVIVED** (R-13) |
| P12 | `internal/apierr/from.go:29` | disable the `fromGM` branch | **killed** — `TestEveryClassifiedLibraryErrorKeepsItsCode` (6 subtests), `TestTheStructuredLibraryErrorsKeepTheirDetails`, `TestAnUnsupportedCapabilityReasonIsLookedUpNotCopied` |
| P13 | `internal/store/messages.go` | `sender = m.ParticipantID` (the raw ID again) | **killed** — `TestSlice2_R4_NoServedIDIsARawGoogleID`, `TestSlice2_R4_SenderFiltersFindWhatTheyJustWrote`, `TestMessageListingFiltersAndOrdering` |
| P14 | `cmd/agent-gm/serve.go:214` | drop `sup.Sweep = workers` | **SURVIVED** (R-13) |
| P15 | `cmd/agent-gm/serve.go` | `resumeAccounts` → `0, nil` | **SURVIVED** (R-13) |
| P16 | `docs/cli.md` | the two R-8 plants, replayed | **killed** — `TestCLIDocNamesNoCommandThatDoesNotExist`, `TestCLIDocQuotesOnlyTheRealEffectSentences` |

Three survivors, all one finding: `serve.go` has no test.

## Rubric — §18.1, re-scored

**Score: 2.** The one thing that held it at 1 is fixed: no raw `gmproto` value
reaches a public surface any more — `sender.id` and
`reactions[].participant_id` are derived `part_` IDs, asserted generically over
every ID field of every read DTO rather than by naming the two known offenders,
and my plant reverting the derivation dies. `docs/cli.md` can no longer claim a
delete scope Google does not offer. R-11's `media_pending` is a wrong *state*,
not a wrong *name* — `media_pending` means what §7.8 says it means; it is being
reported when it is not true, which is R-11's problem and not the rubric's.

## Required fixes before the slice is tagged

1. **R-11** — derive `download_state` at ingest; delete the harness's
   `SetAttachmentDownloadState(…, available, …)` and re-run test 18 without it.
2. **R-13** — a test over `serve`'s wiring; P11, P14 and P15 must all die.
3. **R-12** — migration `0004` for the two recomputed columns and the `react_`
   IDs, or implement `pending_reprocess`.
4. **R-14** — the self participant in the fake's `ResolveConversation`, plus a
   create→send→`sender=me` test.
5. **R-15** — the `pairing_no_cookies` message.
6. §7.5's `backfill.state` vocabulary, closed and tested (see above).

## Live gate

**The coordinator may run the live gate on `3b0b67f`**, for §16 Slice 2 tests
45, 46, 47 and 48. `devbox run check` is green, the live tests are gated behind
both `-tags live` and `AGENT_GM_LIVE=1`, they read numbers only from
`AGENT_GM_LIVE_NUMBERS` or the untracked `testdata/live-numbers.local`, and
`no-real-numbers` passes. Pairing, resume, send, reactions, delete, archive and
group creation are all sound, and the §7.2 codes now reach the CLI, so a
failure during the gate will be diagnosable for the first time.

**One caveat, from R-11:** any attachment *download* will fail with
`unsupported_capability` / `media_pending`. Test 45 sends `--file` and does not
download, so the gate as written is unaffected — but if the owner replies with
a photo, `agm attachments download` will refuse it, and that is the bug, not
the phone.

**And one warning, from R-12:** the owner's existing Slice-1 data directory
holds raw Google participant IDs in `messages.sender_participant`. Running
`3b0b67f` against it leaves those rows serving raw IDs and `sender=me` empty
for them. Run the gate against a clean data directory, as test 45 says
("`agm pair` from a clean data directory"), until migration `0004` exists.

## Housekeeping

Working tree clean at `3b0b67f` apart from this file. Every plant reverted with
`git checkout --`; the three temporary test files I used to drive tests 9, 11
and the gm-error family were deleted. No process killed by name or pattern —
only PIDs I started. No pairing, no Google contact, no message to any number,
no real phone number anywhere in this file.

---

# Confirm pass — `3604892`

**Range:** `3b0b67f..3604892` (1 commit, 17 files, +976/-26)
**Verdict: accept.**

All four findings are fixed, and I confirmed each against the real
`bin/agent-gm serve` binary rather than the harness. Every plant I raised in
the previous pass now dies, including the three `serve.go` survivors. One
required follow-up remains (**R-16**), small and not a live-gate blocker, plus
two notes.

**The coordinator may run the live gate on `3604892`**, for §16 Slice 2 tests
45, 46, 47 and 48, from a clean data directory. The R-11 caveat from the last
pass is withdrawn: an attachment download during the gate now works, verified
byte for byte.

## Evidence I ran at `3604892`

```
$ devbox run check
0 issues.  … all packages ok …  no-real-numbers: clean   EXIT=0

$ CGO_ENABLED=1 go test ./... -race -count=1 -v
--- PASS (top level) : 462
--- PASS (with subtests) : 987
--- FAIL : 0        --- SKIP : 0        EXIT=0

$ go vet -tags live ./... && go vet -tags fixtures ./...        VET-OK
$ devbox run pin-consistency    go.mod, internal/gm/pin.go and §3.6 all name be48a58
$ AGENT_GM_UPSTREAM_DIR=/home/nick/code/mautrix-gmessages devbox run fixture-validation
  --- PASS: TestAssertion01 … TestAssertion20_CookieRefreshWithoutRepair    PASS
$ live test functions: 11
```

**Counts confirmed exactly as claimed: 462 / 987 / 11.**

## R-11 — fixed, and verified end to end for the first time

`internal/core/ingest.go:191` now calls `store.DownloadStateFor(att.MediaID,
att.ThumbnailMediaID)`, and the harness helper that flipped the state is gone.
Driven through the real binary — reserve, `PUT`, send, then fetch by ticket
with **no session bearer**:

```
$ curl .../v1/attachments/att_4868ae1f-… | jq -c '.data|{download_state,sha256,sha256_available}'
{"download_state":"available",
 "sha256":"d20f6ffd523b78a86cd2f916fa34af5d1918d75f7b142237c752ad6b254213ab",
 "sha256_available":true}

$ curl "$download_url" -H "authorization: Bearer $ticket" -D-
HTTP/1.1 200 OK
Content-Disposition: attachment; filename="c.jpg"
Content-Security-Policy: default-src 'none'; sandbox; frame-ancestors 'none'; base-uri 'none'
Content-Type: image/jpeg
X-Content-Type-Options: nosniff
→ BYTES IDENTICAL to the upload

$ sha256sum the downloaded bytes → d20f6ffd…13ab   (equals the served sha256)

$ redemptions 2..6 → 200 200 200 200 401
```

That is §10.1's route, §10.3's five-redemption cap and test 18's counter, all
driven through production HTTP for the first time in this slice — the sha256 is
of the **decrypted** bytes and matches a digest I computed outside the process.

## R-12, R-13, R-14, R-15 — fixed

| # | Evidence |
|---|---|
| R-12 | migration `0004` joins through `participants`, `SchemaVersion()` is 4, and `TestMigration0004RewritesRawParticipantIDs` builds a populated v3 database with raw IDs in both columns. The forward test now asserts by name what 0004 wrote — the stronger claim, and the right call |
| R-13 | `buildServer` is factored out of `runServe`. All three of my survivors now die: **P11r** and **P14r** fail `TestServeWiresTheBackfillerAndTheSweeper` (P14r also `TestServeActuallyBackfillsAndSweeps`), **P15r** fails `TestServeResumesAccountsFromTheSessionDirectory` |
| R-14 | live, on a conversation created through `POST /v1/conversations`: participants are `[{part_e64bbf6c-…, is_me:true}, {part_2a42ea03-…, +1202555xxxx}]` and `GET /v1/messages?sender=me` returns 1 of 1 |
| R-15 | `"Google-account pairing needs all seven session cookies, and at least one is missing; details.missing_cookies names them, and OSID in particular is host-scoped to messages.google.com"` — and it now names the one thing that actually causes this, which is more than I asked for |

## Plant table — confirm pass

| # | Site | Mutation | Result |
|---|---|---|---|
| P11r | `cmd/agent-gm/serve.go:266` | drop `sup.Backfill = workers` | **killed** — `TestServeWiresTheBackfillerAndTheSweeper` (was SURVIVED) |
| P14r | `cmd/agent-gm/serve.go:265` | drop `sup.Sweep = workers` | **killed** — `TestServeWiresTheBackfillerAndTheSweeper`, `TestServeActuallyBackfillsAndSweeps` (was SURVIVED) |
| P15r | `cmd/agent-gm/serve.go:272` | `resumeAccounts` → `0, nil` | **killed** — `TestServeResumesAccountsFromTheSessionDirectory` (was SURVIVED) |
| P17 | `internal/store/attachments.go` | `DownloadStateFor` never returns `available` | **killed** — `TestSlice2_R11_UploadedBytesComeBackDown`, `TestSlice2_R11_DownloadStateIsDerivedFromTheAttachment` (2 subtests), and the whole `TestSlice2_18_*` family (6 tests) plus `TestSlice2_20_ErasureOrderLeavesAnOrphanFileAndNeverAnOrphanRow` — proof the harness helper is genuinely gone and test 18 runs through production |
| P18 | `internal/store/migrations.go` | make 0004's `messages` rewrite match nothing | **killed** — `TestMigration0004RewritesRawParticipantIDs` |
| P19 | `internal/gm/fake/fake.go:686` | `IsMe: true` → `false` on the fake's own participant | **killed** — `TestSlice2_R14_SenderMeWorksOnAConversationTheAPICreated` |

Six of six killed. No survivors this pass.

## One required follow-up

### R-16 — `server_meta.pending_reprocess` is written and consumed by nothing

**Spec:** §4.3 — *"A migration needing derived data recomputed sets
`server_meta.pending_reprocess = <task name>`; **the process runs that task
once after startup and clears the key**."*

Migration 0004 sets `pending_reprocess = 'reconcile_participants'`, which is
right and is the first time anything has written the key. But:

```
$ grep -rn "pending_reprocess\|reconcile_participants" --include=*.go . | grep -v _test | grep -v migrations.go
(nothing)
```

Nothing reads it, nothing runs `reconcile_participants`, nothing clears it. So
the key is set on every upgraded database and stays set for ever, and the rows
0004 could not resolve — a message whose sender was never ingested as a
participant — keep their raw Google IDs indefinitely. §4.3's *"runs that task
once after startup and clears the key"* is the half that is missing, and it is
the half the migration's own comment relies on.

This is the same shape as R-1, R-11 and R-13 one more time: written, tested,
not wired. It is small and its blast radius is a subset of rows on an upgraded
database, so it does not block the gate — but it should land before the slice
is tagged, and the test should be the one that would have caught the others:
start `buildServer` against a database with the key set and assert the task ran
and the key is gone.

**Fix:** in `buildServer`, after migrations and before binding, read
`server_meta.pending_reprocess`; if it is `reconcile_participants`, force a
full sweep for every account and clear the key in the same transaction that
records it done. Assert it in `cmd/agent-gm/serve_test.go` beside the three
tests that now kill P11r, P14r and P15r.

## Two notes

### N-1 — `download_state = "failed"` is in §4.2's vocabulary and nothing produces it

`store.DownloadStateFailed` is declared, §4.2 lists `failed` as one of the four
legal values, and `grep` finds no producer:

```
$ grep -rn "DownloadStateFailed" --include=*.go . | grep -v "attachments.go:41"
(nothing)
```

`DownloadStateFor` is a fact about *reachability* and gets that right, but it
reads only the attachment's two media IDs. §4.4 maps
`INCOMING_DOWNLOAD_FAILED(106)`, `INCOMING_FAILED_TO_DECRYPT(113)` and their
siblings to a message `delivery_state` of `download_failed`, and such a message
can still carry a `media_id` — in which case its attachment is now served
`available` and a caller that redeems the ticket gets an error from Google
instead of the honest `failed`. I could not produce that case against the fake,
so this is a **note, not a finding**: either derive `failed` from the message's
own `download_failed` state at ingest and mark `failed` when a fetch refuses,
or drop `failed` from §4.2 and say the vocabulary is three values. A value in a
closed vocabulary that nothing can produce is a value a caller will write a
branch for and never exercise.

### N-2 — `backfill.state`: the refinement is right; say so in §7.5

The judgement call is correct, and for the reason given: a re-backfill after a
re-pair is exactly the case where "walking at this moment" beats "finished
once before", and `running` is the more actionable fact. §4.7 splits `parked`
from `degraded` on the same principle and `not_started` from `pending` now does
too — the new §7.5 table is the right shape and closes a vocabulary that was
never enumerated at all.

One sentence is missing from it. Because a stored completion does **not**
override `running` or `paused`, a caller seeing `running` cannot tell
"never indexed, first walk" from "already indexed, refreshing" **from `state`
alone** — but it can from the block, because `completed_at` is filled from the
stored instant regardless of `state`. Say that in §7.5:

> `completed_at` is the stored completion instant and is independent of
> `state`: a `running` backfill with a non-null `completed_at` is a re-walk of
> an account whose history is already indexed, and an empty result from it is
> still "nothing", not "not yet".

Without it the table invites a reader to branch on `state` alone, which is the
one reading that gets a re-pair wrong.

## Verified across the three passes

Everything in the `03c0349` pass's "What I verified myself, end to end" section
still holds, and the following are now verified in addition, all against the
real binary: REST pairing of two accounts under the fake; restart and resume;
backfill to `complete` and the reconciliation sweep running; the §7.2 Google
vocabulary reaching callers with the right codes and statuses; `sender=me` on
an API-created conversation; the full media round trip including
download-by-ticket, the decrypted-bytes digest and the redemption cap; backup
mode and its startup warning; and migration 0004 over a populated v3 database.

**Rubric — §18.1: 2.**

## Housekeeping

Working tree clean at `3604892` apart from this file. All six plants reverted
with `git checkout --`. No process killed by name or pattern — only PIDs I
started. No pairing, no Google contact, no message to any number, no real
phone number anywhere in this file.

---

# Second confirm pass — `0e95472`

**Range:** `3604892..0e95472` (1 commit, 11 files, +689/-26)
**Verdict: accept**, with one required follow-up.

The migration blocker is real, the fix is right, and I reproduced **both the
failure and the fix against a v1 database I built myself** — not the owner's,
which I did not go near. R-16 and both notes are fixed and my plants for each
die. One new finding, **R-17**, in the R-16 fix: the reprocess key is cleared
even when nothing was reconciled.

**The coordinator may run the live gate on `0e95472`.** The blocker that would
have stopped it at the first start is gone, and the failure was fail-safe on
the way in — see below.

## Evidence I ran at `0e95472`

```
$ devbox run check
0 issues.  … all packages ok …  no-real-numbers: clean   EXIT=0

$ CGO_ENABLED=1 go test ./... -race -count=1 -v
--- PASS (top level) : 469
--- PASS (with subtests) : 1001
--- FAIL : 0        --- SKIP : 0        EXIT=0
```

**469 top-level confirmed as claimed** (1001 with subtests).

## The migration blocker — reproduced and confirmed fixed, on my own fixture

I did not touch `/home/nick/code/agent-gm-live`. Instead I built a v1 database
by hand with `sqlite3`, from migration 0001's DDL, carrying the Slice 1 shape
the report describes: three participants of which **two carry a raw Google
contact ID** in `participants.contact_id`, and one message whose
`sender_participant` is the raw Google participant ID.

Then I ran the real `bin/agent-gm serve` against it:

```
INF starting  backend=fake …
INF resumed accounts=0
INF ran the pending reprocess task and cleared the key  accounts=0 task=reconcile_participants
INF listening addr=127.0.0.1:19009

$ sqlite3 …/agent-gm.sqlite3 "pragma user_version; select id,contact_id from participants;
                              select id,sender_participant from messages;
                              select key,value from server_meta; pragma foreign_key_check;"
4
part_33333333-…|            ← contact_id resolved to NULL, not copied
part_44444444-…|
part_55555555-…|
msg_66666666-…|part_33333333-3333-5333-8333-333333333333   ← 0004's join, raw `gme` rewritten
pending_reprocess|
(foreign_key_check: empty)
```

v1 → v4 in one start, no foreign-key failure, `foreign_key_check` clean, and
0004's participant join did its work on the same database.

And the plant, **P20**, reproduces the owner's error byte for byte:

```
migration0002_contacts_internal_test.go:58: migrating a v1 database with contact_id set:
  migration 0002 (contacts, attachments, reactions, operations, uploads, tickets,
  media cache, settings, audit, fts): constraint failed: FOREIGN KEY constraint failed (787)
```

### Editing a shipped migration — in bounds here, and why

§4.3 says migrations are **"forward-only, numbered, and never edited after they
ship"**, and this commit edits 0002. I checked whether that is allowed rather
than assuming it, because it is exactly the clause a reviewer is here to hold.

It is in bounds, for a reason worth recording: **0002 has never committed
anywhere.** `internal/store/store.go:294-321` runs each migration inside one
transaction and bumps `PRAGMA user_version` *inside* it, rolling back on any
error — so the owner's database, on which 0002 failed, is still at v1 with
0002's effects entirely absent. Slice 2 is unreleased and untagged. There is no
database in the world at version 2, so there is nothing for a forward-only rule
to protect.

The alternative — leaving 0002 broken and adding a 0005 to repair it — would
have been strictly worse: every v1 database would still crash *in* 0002 and
never reach 0005. Editing was the only correct move.

**Add a sentence to §4.3 recording this**, so the next reader does not cite the
edit as precedent: *"'never edited after they ship' means never edited after a
database has committed it. A migration that has only ever failed, in a slice
that has not been tagged, has not shipped — and repairing it in place is
correct, because a later migration cannot run on a database that crashes before
reaching it."* Without that, the clause and the commit history contradict each
other and the next person resolves it whichever way is convenient.

Two further things I liked and checked: the `pending_reprocess` condition reads
the **old** table before it is dropped and fires on *"had a link and lost it"*
rather than *"has none now"* — a participant this account legitimately holds no
contact for has NULL and is not work; and migration 0004's key-setting became
conditional too, so a fresh deployment no longer schedules a full re-walk on
its first start for nothing. Neither was asked for.

## R-16 and the notes — fixed

| # | Evidence |
|---|---|
| R-16 | `core.RunPendingReprocess` runs in `buildServer` before binding. Live on my v1 fixture: `INF ran the pending reprocess task and cleared the key task=reconcile_participants`, and `server_meta.pending_reprocess` is empty afterwards. A task this build does not know is reported and **left** — the right call, and better than what I asked for |
| N-1 | `store.DownloadStateForMessage` lets a message's `download_failed` veto its media ID, so `failed` is producible. `TestEveryDownloadStateIsReachable` keeps all four of §4.2's values reachable — which is the assertion that stops the next dead value appearing |
| N-2 | the `completed_at`-is-independent-of-`state` sentence is in §7.5 |

## Plant table — second confirm pass

| # | Site | Mutation | Result |
|---|---|---|---|
| P20 | `internal/store/migrations.go` | 0002 copies `contact_id` straight across again | **killed** — `TestMigration0002SurvivesAV1DatabaseWithContactIDsSet`, failing with the owner's exact `FOREIGN KEY constraint failed (787)` |
| P21 | `cmd/agent-gm/serve.go:289` | delete the `RunPendingReprocess` call | **killed** — `TestServeRunsThePendingReprocessTaskAndClearsTheKey` |
| P22 | `internal/store/attachments.go` | drop the `download_failed` veto | **killed** — `TestDownloadStateIsVetoedByAFailedMessage` (3 subtests), `TestEveryDownloadStateIsReachable` |

Three of three killed. Across all four passes: 22 plants, 19 killed, 3
survivors — all three of those were one finding (R-13), and all three now die.

## One required follow-up

### R-17 — the reprocess key is cleared even when no account was reconciled

`internal/core/reprocess.go` says, in its own words:

> *"The key is cleared only when every account was reconciled. A sweep that
> failed — a phone that is asleep, an account that would not connect — leaves
> the key set, so the next start tries again rather than declaring the work done
> because it was attempted."*

That is true for a sweep that **fails**, and false for an account that is never
**offered**. `cmd/agent-gm/serve.go:285-289` passes `sup.List()` — the accounts
that resumed — so an account whose row exists but which holds no session
(`signed_out`, per §4.7, or `parked`, or one whose session did not load) is not
in the list, its `for` loop body never runs, and the key is cleared anyway.

My v1 fixture proves it, with the server's own log line as the evidence:

```
INF resumed accounts=0
INF ran the pending reprocess task and cleared the key  accounts=0 task=reconcile_participants
```

Zero accounts reconciled, two participants that lost a real contact link, and
the key cleared — so the next start will not try again and nothing records that
the work was dropped. The account in my fixture is `signed_out`, which §4.7
makes an ordinary resting state, not an error: *"signing out keeps
everything"*, and its history stays readable, which is exactly the history
whose links are now permanently wrong.

The function's own doc comment states the standard this misses: *"A migration
that hands work to a task nothing runs has not deferred the work; it has
dropped it."* Clearing the key with an empty account list is the same sentence
one level down.

**Fix:** clear the key only when the accounts reconciled cover every account
row in the database — compare `len(accountIDs)` against `st.Accounts(ctx)` —
and leave it set with the existing warning otherwise. The test is the one my
fixture is: a database with `pending_reprocess` set and an account that does
not resume must still have the key set after `buildServer` returns.

This is not a live-gate blocker: the owner's account has `session_present=1`
and will resume, so its links will be rebuilt. It is a required fix before the
slice is tagged.

## One thing for the coordinator, not a finding

The implementer's report says `agent-gm serve` was run **against the owner's
real Slice 1 data directory**, and its closing note says
*"I have not touched `/home/nick/code/agent-gm-live`."* Both cannot be true of
the same directory. The likely reading is that a **copy** was used, which would
be the right thing to have done — but the two statements should be reconciled
explicitly rather than by inference, because "which database did we run an
untested migration against" is a question with only one acceptable answer, and
because a migration that failed mid-flight is precisely when an operator wants
to know whether the original was in the room. I did not look, and I have no
independent evidence either way.

## Housekeeping

Working tree clean at `0e95472` apart from this file. All three plants reverted
with `git checkout --`. The v1 fixture I built lives in `/tmp`, not in the
repository. I did not read, copy, open or otherwise touch
`/home/nick/code/agent-gm-live`. No process killed by name or pattern — only
PIDs I started. No pairing, no Google contact, no message to any number, no
real phone number anywhere in this file.

## Live gate (coordinator, 2026-09-06, build 0e95472, owner's real data directory, backed up first)

Passed: migration v1→v4 on the real Slice-1 database (the first attempt on 3604892 had failed in 0002 on raw contact IDs; fixed in 0e95472); resume of the paired account; backfill of 107 conversations; one reconciliation sweep; `messages send --text` → SUCCESS → `sent`, arrived on the fleet phone (thread read via the fleet CLI); `messages send --file` → `sent`; add and remove reaction → both `succeeded` and both arrived on the phone as reaction texts; `messages delete` (delete-for-me) → `succeeded`, effect sentence correct; unnamed `conversations start` with the two fleet numbers → `succeeded`, conversation with 3 participants; group text → `sent`; an SMS sent from the fleet phone → `incoming received` on the direct conversation within seconds.

Findings sent to the implementer (fix before tagging): (1) a NAMED group start fails and is relabelled `config_version_stale` with empty details, while the same unnamed start succeeds — the name must not go on the first GetOrCreateConversation for non-RCS recipients, and the real Google status must be surfaced, not relabelled; (2) outgoing media produces two attachment rows, both `unavailable`, and the download answers `media_pending` — R-11 held only against the fake; (3) `operations.message_id` stays null on succeeded sends; (4) `--participant` matches only E.164; (5) `serve` did not listen for ~4 minutes (resume + reconcile before bind); (6) `agm session --watch` emitted nothing during the whole gate.

Not observable: MMS arrival on the handsets (the fleet CLI lists SMS only), so group and media delivery are attested by Google's SUCCESS and the `sent` state only. Delivery reports (`delivered`) were not observed for any message.

---

# Third confirm pass — `fd6f153`

**Range:** `0e95472..fd6f153` (4 commits: `2f96f98` R-17, `270fead` pairing
waits for the conversations it read, `a8860f9` the attachment size and
`config_version_stale` retired, `fd6f153` the docs)
**Verdict: accept with required fixes.**

R-17 is fixed and I confirmed it live on the exact case I raised. The
attachment-size fix is right and I watched it work end to end. The
`config_version_stale` retirement is the correct call and is argued well. But
**two of my six plants survived**, both defending changes made in response to a
live gate, and the retirement landed two spec defects — one of which silently
deletes a route's contract from the rendered §7.7 table.

**The coordinator may run the live gate on `fd6f153`.** Nothing here stops it;
D34's fix is what makes test 48 possible at all.

## Evidence I ran at `fd6f153`

```
$ devbox run check
0 issues.  … all packages ok …  no-real-numbers: clean   EXIT=0

$ CGO_ENABLED=1 go test ./... -race -count=1 -v
--- PASS (top level) : 474
--- PASS (with subtests) : 1009
--- FAIL : 0        --- SKIP : 0        EXIT=0
```

**474 top-level** (1009 with subtests). The report said 469; that was
`0e95472`'s number, and the two commits since add five more. Not a discrepancy
worth a finding, but the number in the report is stale.

## Verified fixed, against the real binary

**R-17 — fixed, and I confirmed the exact case.** I rebuilt my v1 fixture (a
`signed_out` account, three participants, raw contact IDs), let it migrate,
then inserted a `contacts` row that *does* match one participant and re-armed
`pending_reprocess`. Restarting `bin/agent-gm serve`:

```
INF resumed accounts=0
INF ran the pending reprocess task and cleared the key  accounts=0 task=reconcile_participants

gme    -> (null)
gthem  -> contact_77777777-7777-5777-8777-777777777777      ← relinked
gother -> (null)
meta: pending_reprocess=(cleared)
account state: signed_out
```

Zero accounts resumed and the link was still repaired, because the relink is a
join and runs for every account **row**. I accept the judgement that the key
may then be cleared: the database-side work is done for everyone, and the
fetch-side residue for a signed-out account is recovered by re-pairing under
§4.7's "re-pairing resumes the same rows". The reasoning for not leaving the
key set for ever on account of one deliberately signed-out account is right.

*(Cosmetic: the log line reports `accounts=len(live)` — the swept count — while
the relink covered every row, so it reads as "0 accounts" on a run that did
work for one.)*

**The attachment size — fixed, verified end to end.** Reserve, `PUT` 22 bytes,
send, echo, through the real routes:

```
$ curl .../v1/conversations/{id}/messages | jq -c '.data.items[0].attachments'
[{"id":"att_614d3dce-…","mime_type":"image/jpeg","filename":"d.jpg","size":22,
  "download_state":"available"}]

$ sqlite3 … "select count(*),size_bytes,download_state from attachments;
             select media_size_bytes from operations where media_size_bytes is not null;
             pragma user_version;"
1|22|available
22
5
```

Exactly one `att_` row, the real uploaded byte count, schema at v5. Making the
fake stop echoing a size Google does not send is the right move and is what
makes the fix visible at all.

**`config_version_stale` retired.** `grep -rn "CodeConfigVersionStale"` finds
nothing; it is gone from `internal/gm`, `internal/apierr`, the exit mapping and
the three tables, and survives as a `/v1/health` field. The reasoning is sound:
a version difference is the normal resting state between pin bumps, so
relabelling a failure with it sent an operator after the pin for failures that
had nothing to do with the pin — and threw away the status that did fail. The
live gate proved that concretely, with D34.

**D34 — I checked the upstream claim myself**, read-only against
`/home/nick/code/mautrix-gmessages` at `be48a58b7338…`. It holds on both
halves: `pkg/connector/startchat.go:189` sets `RCSGroupName` on the first
`GetOrCreateConversationRequest`, and `:214-218` sets `CreateRCSGroup=true`
with the name (defaulting to `""`) on the `CREATE_RCS` retry. So "upstream puts
it on the first call, and Agent GM moves it to the retry, which is where
upstream also has it" is accurate.

**SSE opens with a snapshot** — verified live:

```
event: account.state
data: {"account_id":"acct_…","from":"","to":"connected","state_reason":"","at":"2026-09-07T00:17:32.265Z"}
```

## Plant table — third confirm pass

| # | Site | Mutation | Result |
|---|---|---|---|
| P23 | `internal/gm/libgm.go:463` | set `RCSGroupName` on the **first** `GetOrCreateConversation` — revert D34 | **SURVIVED** — `internal/gm`, `internal/gm/fake`, `internal/core`, `internal/api` all `ok` |
| P24 | `internal/core/mutations.go:92` | drop `SetOperationMediaSize` | **killed** — `TestSlice2_AnOutgoingAttachmentCarriesTheSizeTheReservationCounted` |
| P25 | `internal/core/operations.go:299` | drop `FillMissingAttachmentSize` | **killed** — same test |
| P26 | `internal/store/attachments.go:98` | `size_bytes = excluded.size_bytes` — drop the `COALESCE` | **SURVIVED** — `internal/store`, `internal/api`, `internal/core` all `ok` |

Running total across five passes: **26 plants, 22 killed, 4 survivors** — one
of which (R-13) was fixed, and two of which are new below.

## Required fixes

### S-1 — §7.7's `POST /v1/conversations` row has five cells in a four-column table, so the route's answer is dropped when rendered

`plans/AGENT_GM_SPEC.md:2413`. The D34 sentence was inserted with a `|` before
it, which adds a column:

```
$ header: | Method | Path | Body | Answer |   -> 4 cols
$ row cells: 5
```

Rendered, the `Answer` column shows *"`account_id` is required when more than
one account exists…"* and the real answer — *"`200` with the existing or newly
created conversation plus the operation. `GetOrCreateConversation`; the
`CREATE_RCS` retry of §3.7 is internal. `name` is accepted only for 2+
recipients. Zero recipients, or two that normalise to one number, is
`invalid_request` **before** an operation row exists"* — **disappears from the
table entirely.** That is the answer contract for the one route this commit
changed, on the page that defines it.

`docs/api.md:352` is fine — a five-column table with five cells — so only the
spec is affected, which is worse: the spec is the source the docs are checked
against.

**Fix:** fold the D34 sentence into the Body cell (no extra `|`), and add a
test. `internal/api/docs_test.go` already parses the route tables in
`docs/api.md` and would have caught a malformed row there; the same check over
§7.7's table would have caught this one.

### S-2 — D32's decision row now contradicts every clause that cites it

`plans/AGENT_GM_SPEC.md:5182`, unchanged by this commit, still reads:

> *"it is an **error code** only when a conversation-creating call has actually
> failed (§3.7, §7.2) … §15.5 says what an operator does about it: nothing,
> until a create fails, and then a pin bump"*

while §3.7, §7.2, §13, §15.5 and §16 test 11 now all say there is no such error
code, and **§7.2 cites D32 as the authority for retiring it**. So the decision
record is quoted as the reason for a decision its own text denies.

§18.1 exists precisely so a reader can find out why something is the way it is.
A D-row that says the opposite of the change it authorises is worse than no
row, because it is the one place a reader is told to trust.

**Fix:** rewrite D32's decision cell to state the final position — a health
field, never an error code — and keep the original reasoning in the *because*
cell as the history, the way D19 and D31 already do. The Slice 2 live gate's
evidence belongs there too: it is what changed the decision.

### S-3 — D34's divergence from upstream is undefended (plant P23 **SURVIVED**), and nothing pins the upstream fact it diverges from

Reverting D34 — putting `RCSGroupName` back on the first
`GetOrCreateConversation`, which is exactly the bug the live gate found —
leaves every package green.

This is the most dangerous kind of undefended change, because the divergence
*looks like* a bug: a future reader comparing `internal/gm/libgm.go` against
`connector/startchat.go` at the pin sees Agent GM omitting a field upstream
sets, and the obvious "fix" is to put it back. The comment at `libgm.go:447-461`
argues against that well, but a comment is not a test, and the failure it
prevents is only observable at a live gate against a real phone.

It is testable with no phone. `ResolveConversation` builds a
`gmproto.GetOrCreateConversationRequest` and calls `b.client` twice; a narrow
interface over those two calls lets a test capture both requests and assert
*"the first carries no `RCSGroupName`; the retry carries the name and
`CreateRCSGroup`"*. That is an assertion about Agent GM's own wire behaviour,
not a tautology.

**And add fixture assertion 21.** §13.4's whole purpose is that a pin bump
cannot silently change an upstream fact Agent GM depends on, and D34 depends on
two: that upstream sets `RCSGroupName` on the first call, and that it sets
`CreateRCSGroup` plus the name on the `CREATE_RCS` retry. If a later pin
changes either, the divergence becomes unnecessary or wrong and nothing would
notice. I verified both by hand at `be48a58` for this review; that check should
not be a thing a reviewer does once.

### S-4 — the `COALESCE` that stops a sizeless echo erasing a known size is undefended (plant P26 **SURVIVED**)

`internal/store/attachments.go:98`. Replacing
`size_bytes = COALESCE(excluded.size_bytes, attachments.size_bytes)` with
`size_bytes = excluded.size_bytes` passes `internal/store`, `internal/api` and
`internal/core`.

The new §4.2 comment states the guarantee — *"A size already on the row is
never overwritten by a later sizeless echo"* — and nothing tests it. This is
not hypothetical: `applyParts` calls `UpsertAttachment` on **every** ingest of
a message, and the reconciliation sweep re-ingests on a timer (default 15
minutes, §5.2). Google's echo of media Agent GM sent carries no `Size` — the
premise of the whole fix — so without the `COALESCE` an outgoing attachment's
size would be correct from the first echo until the next sweep, and null for
ever after. The existing test asserts the size arrives; nothing asserts it
stays.

**Fix:** extend
`TestSlice2_AnOutgoingAttachmentCarriesTheSizeTheReservationCounted` to ingest
the same echo a second time and assert the size is still there — one call, and
it kills P26.

## Two smaller items

### S-5 — the SSE snapshot serves `""` where §4.7 says `null`

```
data: {"account_id":"acct_…","from":"","to":"connected","state_reason":"","at":"…"}
```

`state_reason` is *"a short machine-readable string … **or `null`**"* (§4.7),
and `""` is in neither vocabulary. This is R-10c's shape — fixed on
`/v1/accounts` and `/v1/health` in the second pass — reappearing on the new
snapshot event. `from` is worse: on a snapshot there genuinely is no previous
state, and `""` is not a state, so it should be `null` and a client should be
able to tell a snapshot from a transition by that alone rather than by the
event name. §7.5's new SSE paragraph does not say which it is; say it, and
serve `null` for both.

### S-6 — one stale comment

`internal/apierr/from.go:43` still lists `config_version_stale` among the codes
`fromGM` translates. Comment only, no behaviour, but it is the doc comment on
the function that exists to make §7.2 reachable, so it is the one place a
reader checks the vocabulary.

## On the question of what was run against what

The implementer states plainly that nothing was ever run against
`/home/nick/code/agent-gm-live` — not read, not served, not migrated — that the
migration blocker was reported by the coordinator from a structure-only copy,
and that the earlier phrasing was describing the coordinator's observation
rather than an action. That is a clear answer and it is consistent with
everything I can see: the migration fixtures in this commit are `t.TempDir`
databases, and my own reproduction needed nothing but a hand-built v1 file. I
have no evidence to the contrary and consider the matter closed. Recording it
here because "which database did we run an untested migration against" should
have a written answer, not an inferred one.

## Required fixes, in order

1. **S-1** — the §7.7 table row, plus a check over the spec's route tables.
2. **S-3** — a test for D34's request shaping, and fixture assertion 21.
3. **S-4** — a second ingest in the size test.
4. **S-2** — rewrite D32's decision cell.
5. **S-5** — `null` for `from` and `state_reason` on the snapshot, and say so
   in §7.5.
6. **S-6** — the stale comment.

None blocks the live gate.

## Housekeeping

Working tree clean at `fd6f153` apart from this file. All four plants reverted
with `git checkout --`. Fixtures live in `/tmp`. I read
`/home/nick/code/mautrix-gmessages` read-only to check D34's upstream claim and
wrote nothing there. I did not touch `/home/nick/code/agent-gm-live`. No
process killed by name or pattern — only PIDs I started. No pairing, no Google
contact, no message to any number, no real phone number anywhere in this file.

---

# Fourth confirm pass — `ebeb8eb`

**Range:** `fd6f153..ebeb8eb` (1 commit, 13 files, +505/-16)
**Verdict: accept with required fixes.**

All six S-items are fixed, and I killed each of the two survivors with my own
plant rather than taking the claim. Two new findings, **T-1** and **T-2**, both
fall out of a change made between passes that I had not reviewed: accounts now
resume *behind* the listener. The change itself is right; two things that
justified the old order were left behind, and one of them is now a
multi-account bug rather than a stale comment.

**The coordinator may run the live gate on `ebeb8eb`.** Neither finding touches
a single-account gate.

## Evidence I ran at `ebeb8eb`

```
$ devbox run check
0 issues.  … all packages ok …  no-real-numbers: clean   EXIT=0

$ CGO_ENABLED=1 go test ./... -race -count=1 -v
--- PASS (top level) : 477
--- PASS (with subtests) : 1015
--- FAIL : 0        --- SKIP : 0        EXIT=0

$ AGENT_GM_UPSTREAM_DIR=/home/nick/code/mautrix-gmessages devbox run fixture-validation
--- PASS: TestAssertion21_UpstreamNamesTheGroupOnTheFirstCall
PASS  (20 test functions carrying 21 assertions; 03 and 12 still share one)
```

**477 / 1015 confirmed exactly as claimed.**

## The six S-items — all fixed, each plant-verified by me

| # | Plant | Result |
|---|---|---|
| S-1 | **P29**: re-break §7.7's row by inserting the extra `\|` | **killed** — `TestSpecTablesHaveOneCellPerColumn`. The check walks every table in the spec and names the line, the header width and the row width |
| S-3 | **P27**: set `RCSGroupName` before the first `GetOrCreateConversation` | **killed** — `TestResolveConversationDoesNotNameTheGroupOnTheFirstCall`, both subtests (`a plain start asks the question and nothing more`, `CREATE_RCS retries with the name and the flag`) |
| S-4 | **P28**: drop the `COALESCE` | **killed** — `TestSlice2_AnOutgoingAttachmentCarriesTheSizeTheReservationCounted` |
| S-5 | **P30**: make the marshaller always emit a pointer | **killed** — `TestSlice2_TheSSESnapshotWritesNullNotEmptyString` |
| S-2 | by inspection | D32's decision cell now reads *"a health field and nothing else. There is no `config_version_stale` error code"*, with the original reasoning kept as history |
| S-6 | by inspection | `grep config_version_stale internal/apierr/from.go` → nothing |

Two of those deserve saying more about.

**S-4's fix found the harder half.** An identical replay does not exercise the
erasure, because it correlates; the test now steps the delivery ladder one rung
and replays with `old=true`, so `applyParts` runs and the correlation does not.
That is the path the sweep actually takes, and finding it is the difference
between a test that would have caught the regression and one that only looks
like it would.

**Fixture assertion 21 is the right shape.** I read it: it opens
`pkg/connector/startchat.go` **in the pinned upstream tree**, asserts
`RCSGroupName:` inside the first request literal *and* `CreateRCSGroup = ptr.Ptr(true)`
plus `RCSGroupName = ptr.Ptr("")` on the `CREATE_RCS` retry, and fails with
*"upstream no longer sets RCSGroupName on the FIRST GetOrCreateConversation.
D34 exists to diverge from that; re-read the decision before carrying it
forward."* A divergence is only meaningful while the thing it diverges from is
still true, and that is now enforced rather than remembered. `twenty` became
`twenty-one` in the spec, the script and the CI comment.

## Two new findings, from the reordering

Between `fd6f153` and this pass, `startAccounts` moved onto a goroutine behind
`net.Listen`, so `/healthz` answers immediately instead of after a four-minute
resume. **The change is right** and the reasoning in `serve.go:310-324` is
correct: §7.5 says `status` describes the server, not the accounts, and the two
things that must not wait — migrations (§4.3) and crash recovery (§6.6) — still
run before binding. I am not asking for it back. But two things that justified
the old order were left in place.

### T-1 — `RunPendingReprocess`'s doc comment now argues for the opposite of what happens

`internal/core/reprocess.go:48`:

> *"It runs **BEFORE the listener binds**, for the same reason crash recovery
> does (section 6.6): a caller must not see a half-reconciled database and read
> an empty `sender=me` page as an answer."*

It no longer does. `serve.go:385` runs `startAccounts` — which calls it — on a
goroutine after the socket is bound, so a caller *can* now see a
half-reconciled database, and during the reconcile window a `sender=me` query
can return a partial page. This is the same class as S-6, but it is the
justification for a design decision that was reversed, so it reads as an
argument against the code it sits in.

The behaviour is defensible: it is a one-time window on the first start after
an upgrade, and the alternative was the four-minute `/healthz` outage that
prompted the move. But it should be *chosen*, not inherited.

**Fix:** rewrite the comment to say the task runs behind the listener, why, and
what a caller may briefly observe; and add the same sentence to §4.3's
`pending_reprocess` paragraph, which is where an operator reads what the key
means. If the partial page matters more than I think, the alternative is to
report the reconcile in `GET /v1/health` so a caller can tell — but I would not
build that until someone wants it.

### T-2 — one undecryptable session now silently strands every account after it (plant **P31 SURVIVED**)

`cmd/agent-gm/serve.go:458-461` still says:

> *"A session that cannot be decrypted **stops the start** rather than being
> skipped. Section 15.4's first runbook row is exactly this…"*

and §4.5:1520 still says:

> *"**Refusing to start** with `session envelope cannot be decrypted` means the
> key differs from the one that sealed the session."*

Neither is true any more. `resumeAccounts` still `return`s on the first
unloadable session, but its caller now runs behind the listener and treats the
error as non-fatal, logging *"resuming accounts failed; the server is serving"*.
So the `return` no longer stops anything — it **abandons the loop**, and every
account after the failing one in `st.Accounts()` order never resumes at all:
no client, writes refused with `not_signed_in`, and its `state` and
`state_reason` say nothing about why, because the supervisor never touched it.
With one bad session file and three accounts, which of the other two still work
depends on row order.

Three lines below, the `sup.Start` failure path gets this exactly right:

```go
// One account that will not connect is that account's problem, not the
// server's: the rest still serve, and its state and state_reason say why
// (spec section 4.7).
… continue
```

That reasoning applies unchanged to a session that will not load. The
load/decrypt path should `continue` too, keep the loud error it already has,
and mark that account with §4.7's existing `credentials` reason so it says why
rather than being silently absent.

Plant **P31** — turn the `return` into a fall-through — passes `cmd/agent-gm`
clean, so **this path has no test in either direction**: not the old "stops the
start" behaviour, and not the new one.

**Fix:** `continue` with a `state_reason`, and a test with two accounts where
the first session is undecryptable and the second must still reach `connected`.
Then decide §4.5:1520 deliberately — either restore a refusal before the
listener binds for the specific case of *every* session being undecryptable
(which really is a misconfigured deployment), or amend the sentence to say
Agent GM serves and reports the account. Right now the spec promises a refusal
that does not happen.

## Plant table — fourth confirm pass

| # | Site | Mutation | Result |
|---|---|---|---|
| P27 | `internal/gm/libgm.go:482` | `RCSGroupName` on the first call — revert D34 | **killed** — `TestResolveConversationDoesNotNameTheGroupOnTheFirstCall` (2 subtests) |
| P28 | `internal/store/attachments.go:98` | drop the `COALESCE` | **killed** — `TestSlice2_AnOutgoingAttachmentCarriesTheSizeTheReservationCounted` |
| P29 | `plans/AGENT_GM_SPEC.md:2413` | re-break the §7.7 row | **killed** — `TestSpecTablesHaveOneCellPerColumn` |
| P30 | `internal/accounts/state.go:191` | marshal `""` instead of `null` | **killed** — `TestSlice2_TheSSESnapshotWritesNullNotEmptyString` |
| P31 | `cmd/agent-gm/serve.go:481` | undecryptable session no longer aborts | **SURVIVED** (T-2) |

Running total across six passes: **31 plants, 26 killed, 5 survivors** — three
of which (R-13) and two (S-3, S-4) have since been fixed and now die, leaving
P31.

## What I have *not* verified

The report lists six live-gate findings as closed. I have independently
verified two: **1** (D34 — implemented, tested, and its upstream premise pinned
by assertion 21) and **2** (the attachment size — driven end to end in the
third pass). Finding **5** I have read: `serve.go:385` binds and then starts
accounts on a goroutine, which is what it claims, and T-1 and T-2 are its
consequences.

Findings **3** (`operations.message_id` / `correlateEcho` on the Ingester the
supervisor builds), **4** (`--participant` via `store.PhoneMatch`) and **6**
(`agm session --watch`'s snapshot and keepalive line) landed in `270fead`,
which fell inside a range I reviewed for other things and did not examine
clause by clause. They are **unverified by me**, not verified-and-fine. If the
coordinator wants them covered before the tag, say so and I will drive them;
none of the three is hard to check against a fake-backed server.

## Required fixes

1. **T-2** — `continue` with a `state_reason`, a two-account test, and a
   deliberate decision about §4.5:1520.
2. **T-1** — the comment, and a sentence in §4.3.

Neither blocks the live gate.

## Housekeeping

Working tree clean at `ebeb8eb` apart from this file. All five plants reverted
with `git checkout --`; the spec plant was restored from a copy taken first. I
read `/home/nick/code/mautrix-gmessages` read-only to check assertion 21 and
wrote nothing there. I did not touch `/home/nick/code/agent-gm-live`. No
process killed by name or pattern — only PIDs I started. No pairing, no Google
contact, no message to any number, no real phone number anywhere in this file.

---

## Gate findings 3, 4 and 6 — driven at `ebeb8eb` (coordinator request)

Recorded here now so the evidence is not lost; it will be re-run and folded
into the T-1/T-2 confirm pass at whatever SHA the implementer reports next.
All three are **verified**, each against a fake-backed `bin/agent-gm serve`
driven over HTTP, and each with a plant of mine.

### Finding 3 — `operations.message_id` on echo correlation: **verified**

Against the running server, both kinds:

```
$ curl .../v1/operations/{op}   (after send_text)
{"kind":"send_text","status":"succeeded","message_id":"msg_81e8dc21-80ed-5efd-b7c1-2463a8ad9c21"}

$ curl .../v1/operations/{op}   (after reserve → PUT → send_media)
{"kind":"send_media","status":"succeeded","message_id":"msg_0baad4c6-5cf3-5555-bd95-442fd224c41f"}
```

**Plant G3** — blank `messageID` at the top of `correlateEcho` — is killed by
six tests, including `TestEchoCorrelationRunsOnTheSupervisorsIngestPath`
(the one written for this finding), `TestSlice2Test9…/the echo settles it to
succeeded` and `TestSlice2Test10…/failure 2 twice then success reuses one
tmp_id`.

**But the reverse link is never written — U-1, new finding.** §4.2 declares
`messages.operation_id`, `internal/store/messages.go:388` selects it, and the
§7.6 message DTO serves it as `"operation_id": "op_..."`. Nothing anywhere
writes it:

```
$ grep -rn "SET operation_id\|operation_id *=" --include=*.go internal/store/*.go | grep -v _test
(nothing)

$ curl .../v1/conversations/{id}/messages | jq -c '.data.items[]|{id,text,operation_id}'
{"id":"msg_81e8dc21-…","text":"finding three","operation_id":null}
{"id":"msg_0baad4c6-…","text":"pic","operation_id":null}
```

So `message.operation_id` is `null` on every message on every read route, for
ever — a caller reading a message cannot get back to the operation that sent
it, though `correlateEcho` holds both IDs at the moment it writes the other
direction. Same class as R-11: declared, served, never produced. **Fix:** write
it in the same transaction as `operations.message_id`, and assert both
directions in the finding-3 test.

### Finding 4 — `--participant` phone forms: **verified**

`GET /v1/conversations?participant=` against a participant stored as
`+12025550123`:

| form | rows |
|---|---|
| `+12025550123` | 1 |
| `12025550123` | 1 |
| `2025550123` | 1 |
| `202-555-0123` | 1 |
| `(202) 555-0123` | 1 |
| `202.555.0123` | 1 |
| `+1 202 555 0123` | 1 |

`store.PhoneMatch` (`queries.go:636`) normalises to the **last ten digits**, so
every form above collapses to one person without asserting a country. I then
seeded a second conversation with `+13105550123` — a different area code, the
same last seven digits — and checked the ambiguous case: `5550123` returns
**2**, both of them, which is the honest answer rather than a wrong one, and
`3105550123` still returns exactly 1. `sender=me` returned 2 of 2 alongside.

**Plant G4** — drop the suffix from `PhoneMatch` — is killed by
`TestParticipantAndSenderAcceptAllThreePhoneForms` (5 subtests) and
`TestPhoneMatchRefusesAShortFragment`.

### Finding 6 — SSE on real state changes: **verified**, and here is exactly what it emits

Live, over `GET /v1/accounts/events` on the running server, while pairing and
then signing out:

```
event: account.state_changed
data: {"account_id":"acct_6796fecf-…","from":null,"to":"pairing","state_reason":null,…}
data: {"account_id":"acct_6796fecf-…","from":"pairing","to":"connected","state_reason":null,…}
data: {"account_id":"acct_6796fecf-…","from":"connected","to":"signed_out","state_reason":"credentials",…}
```

and, over the wire in a test of mine that drives the SSE route directly with
scripted events, the rest of the vocabulary:

```
snapshot : {from:<nil> to:connected  state_reason:<nil>}        event: account.state
degraded : {from:connected to:degraded state_reason:listen_error}
connected: {from:degraded to:connected state_reason:<nil>}
signed_out:{from:connected to:signed_out state_reason:cookies_expired}
```

**What it emits, stated exactly, because the spec does not enumerate it:** one
`account.state` frame per account in scope when the stream opens, then one
`account.state_changed` per **transition of `accounts.state`** — the §4.7
vocabulary `pairing`, `connected`, `degraded`, `error`, `signed_out`, `parked`,
`account_changed`, carrying `from`, `to`, `state_reason` and `at` — plus a
`: heartbeat` comment frame every 30s. Re-asserting the same state is not a
transition and emits nothing (`supervisor.go:781-785`).

**An incoming message emits nothing, and that is correct.** §7.5 says the
stream "carries no message data, so it needs no replay ring and no cursor". I
asserted the negative: seeding a conversation and ingesting an incoming message
produced no frame within 500ms.

Heartbeat and CLI confirmed live: after 30s the raw stream carried `: heartbeat`,
and `agm session --watch` opens with the `account.state` snapshot of **every**
account as JSONL on stdout, writes `watching /v1/accounts/events; Ctrl-C to
stop` and `stream alive (no state change)`
(`internal/cli/commands_impl.go:357`) to **stderr**, and prints the
`account.state_changed` frame on the real transition — so §11.3's one-JSON-value
rule and §12.2's "stdout carries the result" both hold.

**Plant G6** — make `feed.publish` return early — is killed by
`TestEveryTransitionIsAuditedAndStreamed` and
`TestASlowSubscriberDoesNotBlockTheSupervisor`.

**U-2, small:** G6 left `internal/api` **green**. The only API-level SSE test
(`TestSlice2_TheSSESnapshotWritesNullNotEmptyString`) reads the opening
snapshot and stops, so nothing at the route layer asserts that a state *change*
reaches a client — a regression between the supervisor's feed and the SSE
handler would survive. The test I wrote for this pass is the missing one:
subscribe, apply `EventListenTemporaryError`, `EventListenRecovered` and
`EventGaiaLoggedOut`, and assert three `account.state_changed` frames plus the
negative for an incoming message.

---

# Fifth confirm pass — `dbcd716`

**Verdict: accept.** T-1, T-2, U-1 and U-2 are fixed and verified against the
real binary. Gate findings 3, 4 and 6 re-verified at this SHA. One small new
finding, **V-1**.

```
$ devbox run check                    0 issues … no-real-numbers: clean   EXIT=0
$ go test ./... -race -count=1 -v     480 top-level / 1018 with subtests
                                      0 FAIL, 0 SKIP                     EXIT=0
```

**480 / 1018 confirmed exactly as claimed.**

## T-2 — fixed, and the recovery half works

Two accounts paired through the real server; I then corrupted one byte in the
middle of the **first** account's `sessions/<acct>.enc` so the AEAD fails, and
restarted:

```
agent-gm: acct_6796fecf-… could not be resumed: acct_6796fecf-…: session envelope cannot
  be decrypted; the data key differs from the one that sealed it. Restore the original
  AGENT_GM_DATA_KEY -- there is no in-place rotation (spec 15.4)
ERR resuming accounts failed; the server is serving and the accounts that did not resume
  are listed with their state
INF resumed accounts=1

$ curl .../v1/accounts
{"id":"acct_6796fecf-…","state":"signed_out","state_reason":"credentials"}
{"id":"acct_a6435aa5-…","state":"connected","state_reason":null}
```

The account **behind** the failure resumes, the failing one says why, the error
names the account and `AGENT_GM_DATA_KEY`, and the session file is left in
place. Then the half the report did not claim and I checked anyway — restore
the file, restart:

```
INF resumed accounts=2
{"id":"acct_6796fecf-…","state":"connected","state_reason":null}
```

**It comes back with no re-pair**, which is §15.4's actual promise.

**On `signed_out` versus the coordinator's `error`: the implementer is right,
and there is a second reason they did not give.** `error` means "the supervisor
is retrying `Reconnect` with backoff" (§4.7) and nothing is retrying, so it
would be a false statement about the process. But also: `MarkUnresumable`
deliberately leaves `session_present = 1`, so `resumeAccounts` tries this
account again on **every** start — which is what makes the recovery above
happen automatically, and `transition` does not re-announce because state and
reason are unchanged. That is a better design than either suggestion and it is
worth recording as such. Flagging the divergence rather than burying it was the
right call.

**One consequence to write down (§4.7):** `signed_out` now has two shapes on
disk — shredded (`session_present = 0`, the owner signed out) and
present-but-unreadable (`session_present = 1`, the key is wrong). §4.7's
sign-out procedure specifies `session_present=0` and does not contemplate the
second. Add a sentence: it is the difference between "gone" and "unreadable
from here", and it is what decides whether a restart retries.

## T-1, U-1, U-2 — fixed

| # | Evidence |
|---|---|
| T-1 | `reprocess.go`'s comment now chooses the window instead of arguing against its own code, and §4.3 says the same. Making it **observable** — `GET /v1/health` carries `pending_reprocess` — was not asked for and is the better answer: I confirmed the field is served and is `null` when nothing is outstanding. I could **not** observe the non-null window live, because against the fake the task completes in milliseconds; that half rests on their test and on `health.go:100` reading the key, and I say so rather than claim it |
| U-1 | live: the operation names the message **and** the message names the operation — `{"kind":"send_text","message_id":"msg_cbc8a295-…"}` and `{"id":"msg_cbc8a295-…","operation_id":"op_01a0795d-…"}`. Both directions, written together |
| U-2 | the gap I named is closed: plant **V4** now fails `internal/api` too, at `TestSlice2_AStateChangeReachesAnSSEClient`. Waiting for the snapshot frame before publishing, so the test cannot pass by racing the subscription, is the right shape |

## Gate findings 3, 4 and 6 — re-verified at `dbcd716`

Unchanged from the run recorded above: `operations.message_id` filled for
`send_text` and `send_media`; `participant=` matching `+12025550123`,
`2025550123` and `(202) 555-0123` to one row each; and the SSE stream opening
with `account.state` per account and then `account.state_changed` on a real
sign-out with `state_reason: "credentials"`.

## Plant table — fifth confirm pass

| # | Site | Mutation | Result |
|---|---|---|---|
| V1 | `cmd/agent-gm/serve.go:493` | skip `MarkUnresumable` | **killed** — `TestOneUnreadableSessionDoesNotStopTheAccountsBehindIt` |
| V3 | `internal/core/operations.go:298` | skip `LinkMessageOperation` | **killed** — `TestSlice2_TheEchoedMessageNamesTheSendThatProducedIt` |
| V4 | `internal/accounts/state.go` | make `feed.publish` return early | **killed** — `TestEveryTransitionIsAuditedAndStreamed`, `TestASlowSubscriberDoesNotBlockTheSupervisor`, **and now** `TestSlice2_AStateChangeReachesAnSSEClient` |

(A fourth mutation of mine, V2, was a no-op — my pattern did not remove the
`continue` it was aimed at. It is listed for completeness, not as a result; the
live run above is what proves the walk carries on.)

Running total across seven passes: **37 plants, 32 killed, 5 survivors** — all
five since fixed, and every one of them dies today.

## V-1 — the SSE `at` renders variable-width fractional seconds

§4.4: *"Every JSON surface renders them as RFC 3339 UTC with **millisecond**
precision."* Every DTO surface pads to three digits — `…T21:57:58.139Z`,
`…T12:00:00.000Z`. The SSE feed does not:

```
"at":"2026-09-07T00:56:12.507Z"      ← three digits
"at":"2026-09-07T00:56:13.52Z"       ← two, same stream, seconds apart
```

`announce` truncates to the millisecond correctly, but `StateChange` marshals
`At` as a bare `time.Time`, and Go's RFC3339Nano encoding **strips trailing
zeros** — so the width depends on the value. A client parsing with a fixed
`.SSS` format works until a timestamp lands on a multiple of 10ms, which is
1-in-10 frames, and fails intermittently — the worst way to fail.

This is S-5's shape a third time: the SSE feed is the one JSON surface that
does not go through the DTO layer, so every rendering rule has to be
re-applied there by hand. **Fix:** render `At` through the same millisecond
formatter the DTOs use (`rfc3339` in `internal/api/dto.go`), in
`StateChange.MarshalJSON` where `from` and `state_reason` are already
special-cased; and assert three digits in the SSE shape test. Worth considering
the general form: a single `internal/api` helper for "an instant on the wire",
used by both layers, so the next rule does not have to be remembered twice.

Not a blocker, and not a live-gate concern.

## Housekeeping

Working tree clean at `dbcd716` apart from this file. All plants reverted with
`git checkout --`. Fixtures and corrupted session copies live in `/tmp`. I did
not touch `/home/nick/code/agent-gm-live`. No process killed by name or
pattern — only PIDs I started. No pairing, no Google contact, no message to any
number, no real phone number anywhere in this file.

---

# Sixth confirm pass — `ac59436`

**Verdict: accept with one required fix. Not yet ready to tag.**

```
$ devbox run check                   0 issues … no-real-numbers: clean   EXIT=0
$ go test ./... -race -count=1 -v    481 top-level / 1023 with subtests
                                     0 FAIL, 0 SKIP                      EXIT=0
```

**481 / 1023 confirmed exactly as claimed.**

## V-1 — fixed on the SSE feed, and the fix is a good one

`internal/wire` renders an instant once, from a literal `.000` rather than
from `RFC3339Nano`, and `StateChange.MarshalJSON` renders `at` through it with
the field typed `string`. I drove 24 SSE frames off the running server:

```
$ grep -o '"at":"[^"]*"' … | fractional-digit widths
   24 frames, every one 3 digits — including .600, .790, .911 and .049
```

`.600` and `.790` are the cases that used to render `.6` and `.79`. Testing the
rendering directly against values chosen to strip, rather than only over a
stream, is the right instinct: a stream test alone catches this one run in ten,
which is why it survived in the first place.

**Plants.** Changing `wire`'s layout to `.999` (RFC3339Nano's stripping form)
is killed by three tests across two packages —
`TestStateChangeRendersExactlyThreeFractionalDigits`,
`TestSlice2_AStateChangeReachesAnSSEClient`,
`TestSlice2_TheSSESnapshotWritesNullNotEmptyString`. Typing `at` back to a
`time.Time` **does not compile**, which is a stronger defence than a test.

§4.7's two shapes of `signed_out` are recorded, with `session_present` as the
load-bearing part.

## W-1 — the same bug is still live on `/v1/health` and `/v1/accounts/{id}` (**required before the tag**)

`internal/accounts`'s `AccountHealth`, `BackfillHealth` and `SweepHealth` still
declare `*time.Time` fields (`lifecycle.go:294, 302, 330`) marshalled by Go's
default encoder. `StateChange` was converted; these were not — and they are
served straight out of the accounts package by `GET /v1/health` and
`GET /v1/accounts/{account_id}`, which is the same "does not go through the DTO
layer" path V-1 was about.

Demonstrated deterministically rather than by luck. I stopped the server, wrote
three values chosen to strip — `.500`, `.000`, `.120` — into `accounts`, and
restarted:

```
GET /v1/accounts        (DTO layer)
  {"last_event_at":"2026-09-07T16:53:20.120Z"}                       ← 3 digits

GET /v1/accounts/{id}   (accounts-package block)
  {"backfill":"2026-09-07T16:53:20Z",                                ← NO fraction
   "sweep":"2026-09-07T16:53:20.5Z"}                                 ← 1 digit

GET /v1/health
  {"last_event_at":"2026-09-07T16:53:20.12Z",                        ← 2 digits
   "backfill":"2026-09-07T16:53:20Z",
   "sweep":"2026-09-07T16:53:20.5Z"}
```

Three things are wrong, in increasing order of seriousness:

1. §4.4 requires millisecond precision on **every** JSON surface. `…:20Z`,
   `.5Z` and `.12Z` are none of the three.
2. **The same field, for the same account, renders differently on two routes**
   — `last_event_at` is `.120Z` on `/v1/accounts` and `.12Z` on `/v1/health`.
3. §7.5 says `GET /v1/accounts/{account_id}` returns *"the same per-account
   object `GET /v1/health` embeds"*. Those two do agree with each other here,
   and both disagree with the list route — so the asymmetry the spec calls
   deliberate has quietly become an inconsistency it does not describe.

This is not a new class of defect; it is V-1, unfinished, in the package V-1
was fixed in. The `wire` helper is exactly the right answer — it just has two
more callers than were converted. I found it only because I checked the DTO
surfaces for a regression after the change, and then made the values
deterministic rather than trusting one sample: at 1-in-10 per timestamp, a
single reading of `/v1/health` looks fine most of the time. That is the
property that has now let this survive two passes.

**Fix:** render `AccountHealth.LastEventAt`, `BackfillHealth.CompletedAt` and
`SweepHealth.LastSweepAt` through `wire.InstantPtr`, typing the fields
`*string` so a `*time.Time` cannot come back — the same shape as the
`StateChange` fix, which does not compile when reverted. Then assert it the way
`TestStateChangeRendersExactlyThreeFractionalDigits` does: against values
chosen to strip (`.000`, a multiple of ten milliseconds, an ordinary value),
not over a live response. And sweep the tree once for any remaining
`time.Time` on a `json:` tag — that grep is the general form of this finding
and is worth running before the tag rather than after.

## Everything else

Nothing else changed in this commit, and everything verified in the previous
seven passes still holds. Running total: **39 plants, 34 killed, 5 survivors**,
all five long since fixed.

## The tag

I would not tag `ac59436`. W-1 is small and entirely mechanical, but it is a
public-surface contract violation on the health route — the one an operator
polls — and it makes two routes the spec says serve the same object disagree.
Fix it, and I will confirm on the next SHA; nothing else is outstanding from my
side.

The live gate may still run on this build: W-1 does not affect pairing,
sending, media, reactions, deletion or group creation, and the gate reads
`agm health` for its account rows rather than parsing timestamps.

## Housekeeping

Working tree clean at `ac59436` apart from this file. All plants reverted. I
did not touch `/home/nick/code/agent-gm-live`. No process killed by name or
pattern — only PIDs I started. No pairing, no Google contact, no message to any
number, no real phone number anywhere in this file.

---

# Seventh confirm pass — `77abfa9`

**Verdict: accept. Nothing outstanding. Ready to tag.**

```
$ devbox run check                   0 issues … no-real-numbers: clean   EXIT=0
$ go test ./... -race -count=1 -v    484 top-level / 1026 with subtests
                                     0 FAIL, 0 SKIP                      EXIT=0
```

**484 / 1026 confirmed exactly as claimed.**

## W-1 — fixed, verified on the same deterministic fixture that exposed it

I restored the three stored values that made it visible — `.000` for the
backfill completion, `.500` for the sweep, `.120` for the last event — and read
all three routes:

```
GET /v1/accounts       {"last_event_at":"2026-09-07T16:53:20.120Z"}
GET /v1/accounts/{id}  {"last_event_at":"…20.120Z","backfill":"…20.000Z","sweep":"…20.500Z"}
GET /v1/health         {"last_event_at":"…20.120Z","backfill":"…20.000Z","sweep":"…20.500Z"}
```

`…20Z` and `.5Z` and `.12Z` are gone; the two routes §7.5 says serve the same
object now agree with each other **and** with the list route. I then swept
every read route for any instant not at three digits:

```
/v1/accounts  /v1/accounts/{id}  /v1/health  /v1/conversations
/v1/operations  /v1/contacts  /v1/admin/audit        → all 3-digit
```

**Plants.**

| # | Mutation | Result |
|---|---|---|
| X1 | revert `SweepHealth.LastSweepAt` to `*time.Time` | **does not compile** — `cannot use msToTime(...) (value of type *string) as *time.Time`. The strongest form of the defence, and now on all three fields |
| X2 | render `wire`'s layout with RFC3339Nano | **killed** — five tests across three packages: `TestStateChangeRendersExactlyThreeFractionalDigits`, `TestHealthBlocksRenderExactlyThreeFractionalDigits`, `TestHealthForRendersStoredTimestampsAtAFixedWidth`, `TestSlice2_AStateChangeReachesAnSSEClient`, `TestSlice2_TheSSESnapshotWritesNullNotEmptyString` |
| X3 | add a bare `time.Time` field with a `json` tag to `reactionDTO` | **killed** — `TestNoJSONFieldIsABareTime`, naming `internal/api/dto.go:240`, the field, and what to do instead |

## On turning the grep into a test

This is the right response to the finding, and better than what I asked for. I
checked the one thing that would have made it hollow: `isTime` unwraps
`*ast.StarExpr`, so **`*time.Time` is caught** — the exact shape of W-1. A
guard against the pointer-free form only would have passed W-1 unchanged and
looked like protection.

Removing `StateChange`'s now-inert `json` tags is the same instinct applied
one level up, and the reasoning is right: a second statement of the shape that
nothing enforces is how the rendering came to be written twice and differently
in the first place. Skipping dot-directories so my review worktree does not
report this tree's findings back at the wrong paths is a detail I would not
have thought to ask for.

**One hole, a note rather than a finding.** `isTime` does not unwrap
`ast.ArrayType` or `ast.MapType`, so a `[]time.Time` or
`map[string]time.Time` carrying a `json` tag passes:

```
plant: `Planted []time.Time `json:"planted"`` on reactionDTO
       → ok  github.com/thisnick/agent-gm/internal/wire
```

No such field exists today and none is planned, so nothing is wrong now — but
the guard is worth two more lines while the file is open, since the whole point
of it is to hold for the case nobody thought of.

---

# Final verdict — Slice 2

**Accept. `77abfa9` is ready to tag, and the coordinator may run the live gate.**

Nine reviews, and the shape of them is worth recording. The first pass was a
reject on three blockers that every test in the suite passed through: a
`Backfiller` and `Sweeper` that existed, were tested, and were wired to
nothing; a `From` that turned every Google-layer error into `internal_error`;
and a `serve` that never resumed an account. Two more of that exact shape
turned up later — an attachment state nothing could set to `available`, and a
`pending_reprocess` key nothing read. Every one of them was invisible to a
green suite and visible within minutes of driving the binary, which is the
argument for driving the binary.

What changed my confidence was not that the findings were fixed but *how*:
each fix went to the root rather than the symptom, several found a second bug
on the way in, and the ones that mattered are now defended by something that
does not compile when reverted rather than by a test that has to be
remembered. The two spec defects, the D32 contradiction and the §7.7 table,
were fixed with a check that walks every table in the spec. The
`config_version_stale` retirement and D34 both came from live gates and both
ended with the upstream fact they depend on pinned by a fixture assertion.

**Counts at `77abfa9`:** 484 top-level tests, 1026 with subtests, 0 failures, 0
skips, 11 live-gated, 21 fixture assertions against `be48a58`. `devbox run
check`, `pin-consistency`, `fixture-validation` and `no-real-numbers` all
green, and `go vet` clean under `-tags live` and `-tags fixtures`.

**Plants:** 43 across nine passes, 38 killed on first planting, 5 survivors —
R-13 (three, `serve.go` untested), S-3 (D34 undefended) and S-4 (the
`COALESCE`). All five are fixed and all 43 die at this SHA.

**Rubric §18.1: 2.**

**Outstanding: nothing.** The `[]time.Time` hole in the bare-time guard is a
note, not a condition.

## Housekeeping

Working tree clean at `77abfa9` apart from this file. All plants reverted with
`git checkout --` or from a copy taken first. Fixtures live in `/tmp`. I read
`/home/nick/code/mautrix-gmessages` read-only and wrote nothing there. I did
not touch `/home/nick/code/agent-gm-live` at any point in this review. No
process was killed by name or pattern — only PIDs I started. No pairing, no
Google contact, no message to any number, and no real phone number anywhere in
this file.
