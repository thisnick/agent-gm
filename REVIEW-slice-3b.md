# Review — Slice 3b (the MCP protocol layer is the official Go SDK, D36)

**Range reviewed:** `34c6e26..efa7136` (17 commits, branch `slice-3b/mcp-sdk`).
**Verdict is against `efa7136`.**
**Reviewed on:** `review/slice-3b`, worktree `.claude/worktrees/review-slice-3b`.
**Spec:** `plans/AGENT_GM_SPEC.md` §8.1, §8.2, §9.2, §12.4, §13.1, §16 Slice 3 (tests 2, 14–23, 25, 28), §18 D36;
`docs/mcp.md`, `docs/oauth.md`, `docs/deploy.md`, `docs/README.md`.
**Prior review:** `git show origin/review/slice-3:REVIEW-slice-3.md`. Its method — drive the real
binary, never a look-alike harness — is the method here, and every security invariant its table
records was re-driven behind the SDK.

Everything below was driven against **`bin/agent-gm serve`** (the binary built from `efa7136`),
`AGENT_GM_BACKEND=fake` + `AGENT_GM_ALLOW_FAKE=1`, tokens minted through the **whole** OAuth flow
(admin bootstrap → enrollment code → DCR → authorization screen → owner approval → completion →
token endpoint), with `curl`, the **official MCP TypeScript SDK client v1.30.0**, and the pinned
conformance suite. No phone, no real number, no contact with the production container.

## Verdict

**Accept with required fixes.** The security boundary held everywhere I pushed it, and the SDK
swap did not cost a single invariant the Slice 3 review verified. The required fixes are all
test-and-docs gaps, none of them a wrong answer on the wire.

- **Main may be fast-forwarded to `efa7136` and the production image rebuilt from it.**
- **The claude.ai / ChatGPT connector gate (test 29) may run on it.** The one thing that gate
  would have tripped over before this slice — a JSON-RPC error tearing the connector's session
  down — is fixed and I confirmed it with the TypeScript client itself.
- Fix **B-1** (the pin guard is unguarded) before anyone runs `go get -u`, and **B-5**
  (`docs/README.md:7`) before the docs are shown to anybody.

## Evidence I ran

```
$ devbox run check                                                  # efa7136
0 issues.
ok  ... (19 packages ok, 5 without test files)
no-real-numbers: clean
EXIT=0

$ CGO_ENABLED=1 go test ./... -count=1 -v
=== RUN 1263 | --- PASS 1263 (579 top-level) | --- FAIL 0 | --- SKIP 0
                              # the claim of 579 / 1263 reproduces exactly, 0 skips, race-clean

$ devbox run pin-consistency
pin-consistency: go.mod, internal/gm/pin.go and spec section 3.6 all name be48a58
pin-consistency: go.mod and spec D36 both pin github.com/modelcontextprotocol/go-sdk at v1.7.0

$ devbox run conformance
Total: 7 passed, 23 failed
conformance: 30 scenarios
conformance: the baseline holds in both directions
EXIT=0
        # and the direction is real: deleting the `prompts-get-simple` baseline entry gives
        # `conformance: NEW FAILURE prompts-get-simple`, exit 1.

$ gh run view 34082018394 --json jobs -q '.jobs[]|"\(.name): \(.conclusion)"'
pin-consistency: success | lint-names: success | no-real-numbers: success
fixture-validation: success | check: success | image: success | conformance: success
        # headSha efa71362f031714a29d22b1753bbeda97ad72b8e, conclusion success. Claim verified.
```

## Findings

### B-1 — The SDK pin guard has nothing guarding it; deleting it is invisible (**medium — D36**)

D36 makes the `go-sdk` pin "a fact CI keeps, like the libgm one". The libgm half of
`scripts/pin-consistency.sh` is backed by `internal/gm/pin_test.go`, and `lint-names.sh` has a
meta-test. The `go-sdk` half (`scripts/pin-consistency.sh:70-88`) has neither.

Driven — plant **P6**, deleting the whole step-6 block:

```
$ devbox run pin-consistency
pin-consistency: go.mod, internal/gm/pin.go and spec section 3.6 all name be48a58
EXIT=0                                  # green, and the SDK line is simply gone
$ go test ./... -count=1                # 19 ok, 0 failures
```

**SURVIVED** the whole suite *and* its own CI job. The guard works while it is there — I bumped
`go.mod` to `v1.7.1` alone and it exits 1 with the right message — but nothing keeps it there.
The production consequence: the one mechanism that makes an SDK bump "a deliberate slice, never a
drive-by commit" can be removed in the same drive-by commit that bumps the SDK, and CI stays green.

**Fix:** a Go test that runs `scripts/pin-consistency.sh` over a temporary tree with the SDK
version desynchronised and requires a non-zero exit — the shape `internal/lint`'s meta-test
already uses.

### B-2 — A served argument description can be rewritten and nothing fails (**medium — §8.2 test gap**)

Plant **P8b**: shortened `send_message.text`'s description from
`"The message body. Optional only when `upload_ids` is present, in which case it is the caption."`
to `"The message body."` (`internal/mcp/tools.go:476`).

```
$ go test ./... -count=1      # 19 ok, 0 --- FAIL
```

**SURVIVED.** The catalogue is asserted for *presence* of a description on every argument and for
banned words (`TestNameLint` killed plant **P8**, which used "chat room"), but not for content.
`docs/mcp.md` carries a tool table; the "First five minutes" block is pinned byte-for-byte to the
served `instructions` (plant **P12** proved that), and nothing comparable pins the tool table to
the catalogue. The production consequence is the one §8.2 exists to prevent: the sentence that
tells a model `text` is optional-with-an-upload is the only place that fact is stated to the model,
and it can go missing silently.

**Fix:** extend the docs-parity test of `internal/mcp/slice3_docs_test.go` to the tool table —
every tool name, description and argument description in `docs/mcp.md` compared to the catalogue,
the way the instructions block already is.

### B-3 — `TokenInfo.UserID` claims a session-hijacking guard that nothing tests (**low — §8.1**)

`internal/mcp/handler.go:496` sets `UserID: auth.ID` and the comment calls it "the SDK's
session-hijacking guard … which is the property section 8.1 used to get for free by having no
sessions at all". Plant **P11** blanked it (`UserID: ""`):

```
$ go test ./... -count=1      # 19 ok, 0 --- FAIL
```

**SURVIVED.** Today it is harmless — `Stateless: true` means there is no session to hijack, so the
field is inert — but the comment asserts a property the code does not have a test for, and it is
exactly the property that would matter if `Stateless` were ever reconsidered. Either test it or
say in the comment that it is inert under a stateless transport and kept for the day it is not.

### B-4 — `TestTest25ConcurrencyBudget` kills a regression by hanging for ten minutes (**low**)

Plant **P13** multiplied both limits by 1000 (`handler.go:35-37`). It **is** killed, but:

```
panic: test timed out after 10m0s
	TestTest25ConcurrencyBudget (9m59s)
FAIL	github.com/thisnick/agent-gm/internal/mcp	600.141s
```

The test holds eight calls in flight and blocks waiting for a `429` that a widened budget never
sends. A regression therefore costs ten minutes of CI and reports as a package timeout rather than
as a named assertion. **Fix:** bound the wait (a `select` on a short timer) and fail with the
message.

### B-5 — `docs/README.md:7` still says nothing under `docs/` exists (**low — required**)

> "Nothing under `docs/` exists yet except this index — the pages are deliverables of the slices in
> spec §16 …"

Seven pages exist and two of them were edited in this slice. The sentence is the first thing a
reader of the docs tree sees and it is false. **Fix:** replace it with the delivered/outstanding
split, keeping the "what each one owes" table.

### B-6 — `sdk.go`'s `ProtocolVersion` comment is wrong about `initialize` (**low — carried into §8.1**)

`internal/mcp/sdk.go:519-521`: "`ProtocolVersion` is what `initialize` negotiates to when the
client asks for it, asks for something newer, or asks for nothing."

Driven against the binary — a raw `initialize` asking for `2026-07-28`:

```
"protocolVersion":"2025-11-25"
```

and asking for `2025-11-25`, `2025-06-18` or `1999-01-01` likewise all land on `2025-11-25`. This
is the SDK's own rule and it is correct: `negotiatedVersion` (`mcp/shared.go:67-77`) caps
`initialize` at `2025-11-25` because `initialize` is *deprecated* in `2026-07-28`; a client that
wants the newer revision uses `server/discover`, which is what the reference client does and what
`internal/mcp/slice3_behaviour_test.go:462` really asserts. So the server is right and the
comment is wrong, in the one place a reader would go to check. **Fix:** say that `initialize`
answers `2025-11-25` by the SDK's design and `2026-07-28` is reached through `server/discover`.
§8.1's prose is fine as written; nothing on the wire changes.

### B-7 — `sameOrigin` compares the whole URL, not scheme and host (**low — carried from Slice 3**)

`internal/mcp/handler.go:632-634` says "The comparison is on scheme and host only, because that is
all an `Origin` is", and then compares the trimmed strings. `AGENT_GM_PUBLIC_URL` is allowed to
carry a path (`internal/config/config.go:165-187` refuses only a query or a fragment), and with one
every browser client on the *correct* origin is `403`ed, because an `Origin` header never carries a
path. No impact on `https://gm.agent-wx.app`. **Fix:** parse both and compare
`scheme://host`, or refuse a path in `loadPublicURL`.

### Notes (not required)

- `docs/mcp.md:191-195` and `docs/oauth.md:114-118` print the challenge folded over three lines. The
  real header is one line (I read it off the wire). Obsolete line folding; a reader copying it gets
  a header no server would send. Cosmetic.
- The `/mcp` `401` message passes the SDK's own sentence through
  (`"the token was refused: invalid token"`). It is the same string for revoked, expired and
  never-existed, so it is not an oracle — I checked all three.
- `account.resumed` means *a re-pair onto an existing row* (`internal/accounts/state.go:141`), while
  the startup log line `"resumed"` means *sessions reloaded from disk*. Two meanings of one word,
  both pre-existing. Not a slice-3b regression; worth one of them being renamed one day.

## The auth boundary, as driven against `bin/agent-gm serve`

| Probe | Result | Code |
|---|---|---|
| No bearer | `401`, `WWW-Authenticate: Bearer resource_metadata="…/.well-known/oauth-protected-resource/mcp", scope="messages:read messages:write"`, §7.1 envelope `invalid_token`, one header, **no `realm`** | `handler.go:328,395` |
| Garbage bearer | `401`, same challenge byte for byte | `handler.go:449` |
| Revoked, on the request **after** `DELETE /v1/admin/authorizations/{id}` | `200` before, `401` after, on the same client | `handler.go:452`; `TestSlice3bRevocationEndsALiveMCPSession` drives it on an **open SDK session** |
| Token with no messaging scope | not obtainable through any flow — `admin` is not enrollable and the bootstrap session carries all four scopes, which is itself the right answer. The `403 insufficient_scope` path is covered in-package (`TestSlice3Test14ScopeGating`) and plant **P1** (OR→AND) is killed by it | `handler.go:363` |
| `messages:read` token calling `send_message` | **`isError` result**, not a status: `structuredContent.error.code = "insufficient_scope"`, `details.required_scope = "messages:write"`, HTTP `200` | `sdk.go:211,347` |
| Two `Authorization` headers, **the first one valid** | `401`. Also `Basic` → `401`, `Bearer <a> <b>` → `401`, lowercase `bearer` → `200` | `handler.go:479` |
| `Origin: https://evil.example`, **no credential at all** | `403` `invalid_request` "this endpoint may only be reached from …", before any parse or store lookup. Exact origin → `200`; `Origin: null` → `403`; absent → accepted | `handler.go:186` |
| RFC 9728 ↔ discovery ↔ challenge | `resource` == `AGENT_GM_PUBLIC_URL` + `/mcp`; `issuer` == `AGENT_GM_PUBLIC_URL`; the challenge's `resource_metadata` == that + `/.well-known/oauth-protected-resource/mcp`; the `/mcp` and root documents `diff`-identical; `Access-Control-Allow-Origin: *`, `OPTIONS` → `204`, and §9.9's four headers survive underneath | `oauth/metadata.go` |
| Secrets in logs | access, refresh, admin-session and admin-secret values: **0 occurrences** in 75 log lines at `AGENT_GM_LOG_LEVEL=debug`. Refusals log `request_id` + `code` only | `handler.go:417` |
| `429` shape | 40 parallel calls → 18×`200`, 22×`429` with `Retry-After: 1` and §7.1's `rate_limited` envelope | `handler.go:225` |

## Protocol, as driven with both official clients

**Go SDK client** (in-suite) and **TypeScript SDK client v1.30.0** (mine, against the binary):

```
connected. server: {"name":"agent-gm","version":"1.0.0-slice2+efa7136…","websiteUrl":"https://github.com/thisnick/agent-gm/tree/efa7136…"}
tools: 21
args with no description: 0 []
domain failure isError: true {"error":{"code":"not_found",…}}
unknown tool error: McpError: MCP error -32602: unknown tool "no_such_tool"
SESSION SURVIVED unknown tool. tools: 21
unknown prompt: McpError: MCP error -32602: unknown prompt "x"
SESSION SURVIVED. tools: 21
resources/list: []            templates: ["agm://attachments/{attachment_id}"]
resources/read unknown: -32602 MCP error -32602: Resource not found
SESSION SURVIVED read failure. tools: 21
```

That is the whole point of the 200-status override, confirmed by the client the override exists
for: three JSON-RPC errors in a row and the connector keeps working. Plant **P4** (restore the
SDK's 4xx) is killed by `TestAJSONRPCErrorDoesNotEndTheSession`.

Also driven, raw, at protocol `2026-07-28` (`Mcp-Protocol-Version`, `Mcp-Method`, `Mcp-Name` and
the three `_meta` keys SEP-2575 requires):

- `server/discover` → `supportedVersions: ["2026-07-28","2025-11-25","2025-06-18","2025-03-26","2024-11-05"]`,
  `_meta` carrying `app.agent-gm/commit`, `app.agent-gm/source_url` and the SDK's `serverInfo`
  (name, `version` with the commit as semver build metadata, `websiteUrl`) — §1.4's AGPL obligation
  on the handshake a 2026-07-28 client actually performs.
- `initialize` → `2025-11-25` for every requested revision (B-6), instructions block attached.
- `tools/list`: **21** with all three messaging scopes, **11** with `messages:read` alone; every
  argument of all 21 has a non-empty description; `additionalProperties: false` on all 21; an
  `outputSchema` on all 21; all four annotation hints serialised on all 21.
- `tools/call` unknown tool → `-32602` at HTTP **200**; unknown prompt → `-32602` at **200**;
  domain failure → `isError` result with `structuredContent.error`.
- `resources/list` → `[]`; `resources/templates/list` → the attachment template with
  `messages:read`, **`[]`** with `messages:write` alone; `resources/read` of an unknown URI, of
  `file:///etc/passwd`, and by a caller without `messages:read` → all three the identical
  `-32602 Resource not found` (the last one leaks nothing).
- `GET /mcp` → `405 Allow: POST` authenticated, `401` unauthenticated; `DELETE` → `405 Allow: POST`;
  `Content-Type: text/plain` → **415**; `Accept` with only one media type → **400** (both ways);
  a malformed body → **400** with §7.1's envelope; 1 MiB + 1 unauthenticated → **413**.
- The `instructions` block read off the wire is **byte-identical** to `docs/mcp.md`'s
  "First five minutes" (`diff` clean; 7816 bytes).

## R-10, as driven under the real `serve` wiring

Paired a fake account through `POST /v1/pairing/start` (fixture cookies, `AGENT_GM_BACKEND=fake`),
re-paired it onto the same `acct_` row, signed it out, killed the process and started a second one
over the same data directory, then removed the account:

```
account.removed
account.signed_out
account.state_changed
account.resumed          <- the re-pair
account.state_changed
account.paired           <- the first pairing
account.state_changed
```

and the two derived kinds, each written exactly once by the read that first saw the expiry:

```
enrollment.expired          # a code read after oauth.enrollment_default_ttl=1m elapsed
authorization.expired       # a pending request read after oauth.authorization_request_ttl=1m
```

All six kinds of §12.4 are written by production paths, not by a test double. `account.paired`'s
payload carries `{"device_count":1,"device_index":0,"resumed":false}` and no credential.
Plant **P7** (drop the `account.signed_out` row) is killed by
`TestServePairingWritesTheAccountLifecycleAuditRows`. Note for the record: a **server restart**
resume writes no audit row — correctly, because `account.resumed` means a re-pair
(`internal/accounts/state.go:141`), not a reload.

## Spec coherence

- §8.1, §8.2, §9.2 and D36 each match what the binary does, clause by clause; the D36 "what it
  forced" table is accurate in all eight rows (I drove seven of them; the eighth, `-32002`, is a
  fact of the SDK version).
- **No retired wording survives in a live-contract sentence.** `realm` appears only in the `/v1`
  challenge (which really does carry it — a different string, and both docs say so), in D36's
  before/now table, and in three sentences that say there is no longer one. `-32002` appears only
  in D36's table and in two comments explaining what it *was*. "either media type" appears only in
  D36's before column; the live rule is stated as **both** in §8.1, `docs/mcp.md:181` and on the
  wire. "Session" in `docs/mcp.md` means either an account session (`get_session`) or the thing
  that does not exist here, and says so.
- `docs/mcp.md`, `docs/oauth.md` and `docs/deploy.md` are accurate against the code, including the
  `serverInfo`/`websiteUrl` change and the RFC 9728 CORS paragraph.
- `docs/README.md:7`: **B-5**.
- The 21-tool catalogue really is byte-identical: `git diff 34c6e26..efa7136 -- internal/mcp/tools.go`
  is gofmt alignment only, and `schema.go`, `outputschema.go` and `instructions.go` are untouched.
- The prior review's open items are closed: **R-1** (`resources/read` is now the SDK's `-32602`
  and the reference client reads it), **R-2/R-3** (`TestSlice3SDKReferenceClientGetsNoCodeWithoutApproval`),
  **P20** (`TestSlice3TokenResourceIsCheckedWithoutSpendingTheCode`, `…IsOptional`),
  **P25** (`TestTest25ConcurrencyBudget`), **R-10** (above).

## Plants

Thirteen mutations, all mine, all at `efa7136`, all reverted (`git status` clean).

| # | Mutation | file:line | Result |
|---|---|---|---|
| P1 | scope OR → AND in `hasAnyMessagingScope` | `internal/mcp/handler.go:411-418` | **killed** — `TestSlice3Test14ScopeGating` (+ `TestSlice3Resources`, `TestSlice3SDKReferenceClientCompletesTheFlow`, `TestSlice3bRevocationEndsALiveMCPSession`) |
| P2 | drop the `Origin` check | `internal/mcp/handler.go:186` | **killed** — `TestSlice3Test21Transport/a_foreign_Origin_is_403_before_anything_is_parsed` |
| P3 | drop the dual-`Authorization`-header check | `internal/mcp/handler.go:483` | **killed** — `TestTwoAuthorizationHeadersAreRefusedBeforeTheSDKSeesThem`, `TestSlice3Test21Transport/two_Authorization_headers_are_refused_rather_than_resolved` |
| P4 | remove the 200-status override (keep the SDK's 4xx) | `internal/mcp/handler.go:597` | **killed** — `TestAJSONRPCErrorDoesNotEndTheSession/an_unknown_tool_name`, `TestSlice3Test19IsErrorSemantics`, `TestSlice3Resources` |
| P5 | challenge omits `resource_metadata` | `internal/mcp/handler.go:394` | **killed** — `TestTheChallengeIsTheSDKsFormat`, `TestSlice3Test2MCPChallenge`, `TestSlice3Test21Transport/no_token_is_401_with_the_challenge` |
| P6 | delete the `go-sdk` half of the pin guard | `scripts/pin-consistency.sh:70-88` | **SURVIVED** — 19 packages ok, and `devbox run pin-consistency` exits **0**. → **B-1** |
| P6b | bump `go.mod` to `v1.7.1` alone | `go.mod` | **killed** — `devbox run pin-consistency` exit 1, "go.mod pins … v1.7.1, spec D36 states v1.7.0" (the guard works while it exists) |
| P7 | drop the `account.signed_out` audit row | `internal/accounts/supervisor.go:599` | **killed** — `TestServePairingWritesTheAccountLifecycleAuditRows` |
| P8 | rewrite `send_message`'s description using "chat room" | `internal/mcp/tools.go:469` | **killed** — `TestNameLint`, and `devbox run lint-names` exit 1 |
| P8b | shorten `send_message.text`'s argument description (no banned word) | `internal/mcp/tools.go:476` | **SURVIVED** — 19 packages ok, 0 failures. → **B-2** |
| P9 | `scopeGate` stops turning a `tools/call` scope refusal into an `isError` result | `internal/mcp/sdk.go:211-214` | **killed** — `TestSlice3Test14ScopeGating` |
| P10 | add `messages:delete` to `ChallengeScopes` | `internal/mcp/handler.go:61-63` | **killed** — `TestSlice3Test2MCPChallenge` |
| P11 | blank `TokenInfo.UserID` | `internal/mcp/handler.go:496` | **SURVIVED** — 19 packages ok, 0 failures. → **B-3** |
| P12 | change one sentence of `docs/mcp.md`'s "First five minutes" | `docs/mcp.md` | **killed** — `TestSlice3Test20InstructionsMatchTheDocs` |
| P13 | multiply both concurrency limits by 1000 | `internal/mcp/handler.go:35-37` | **killed** — `TestTest25ConcurrencyBudget`, but by a 10-minute package timeout. → **B-4** |
| C1 | delete the `prompts-get-simple` baseline entry | `scripts/mcp-conformance-baseline.yaml:84` | **killed** — `conformance: NEW FAILURE prompts-get-simple`, exit 1 |

## Rubric — §18.1, one axis: every name means what it means in Google Messages

**Score: 2.** The names this slice adds are protocol and plumbing, not vocabulary: `ServerVersion`,
`serverFor`, `scopeKey`, `scopeGate`, `stampBuild`, `sdkTool`, `sdkAnnotations`,
`attachmentTemplate`, `errorResult`, `scopeRefusal`, `sessionFor`, `caller`, `envelopeWriter`,
`isJSONRPCError`, `errorForStatus`, and the two `_meta` keys `app.agent-gm/commit` and
`app.agent-gm/source_url`. None of them reaches a model or an owner. The 21 tool names, every
description and the instructions block are byte-identical to Slice 3, which is where the axis was
scored 2 and where it stays. The one wobble is not new and not user-facing: `account.resumed` (a
re-pair) versus the startup log's `"resumed"` (sessions reloaded). The smallest fix, if it is ever
worth making, is renaming the log line to `"reconnected"`.

## Spec clauses — verified / gap / n/a

**verified `efa7136`:** §8.1 transport order, `Origin`, 1 MiB, single `Authorization`, messaging-scope
OR, 8/32 budget with `Retry-After`, `415`, `Accept`-both `400`, `405 Allow: POST` on `GET`/`DELETE`,
`401` before auth on `GET`; §8.1 `serverInfo` (`name`, semver-build-metadata `version`,
`websiteUrl`) and the `_meta` build keys on both handshakes; §8.2 the 21-tool catalogue, schemas,
descriptions, annotations, `outputSchema`, scope-gated `tools/list`, the `tools/call` re-check as a
result, the `isError` envelope, `resources/list`/`templates/list`/`read` incl. `-32602`, the
JSON-RPC-error-at-HTTP-200 rule; §8.3 instructions parity with `docs/mcp.md`; §9.2 both
protected-resource documents, byte-exact `resource`/`issuer`, the challenge without `realm`, RFC
9728 CORS and preflight, §9.9 headers; §9.6 revocation on the next request, on an open session;
§12.4 the four `account.*` kinds and the two `*.expired` kinds; §12.2 no secret in the log; §13.1
the fake backend; §16 Slice 3 tests 2, 14–23, 25, 28; §18 D36 including the pin's own CI job.

**gap:** the pin guard's own guard (**B-1**); served descriptions vs `docs/mcp.md`'s tool table
(**B-2**); `TokenInfo.UserID` (**B-3**); the concurrency test's unbounded wait (**B-4**).

**n/a this review:** §16 Slice 3 tests 26/27 (image and compose — unchanged by this slice and green
in CI run 34082018394), test 29 (the live connector gate, the coordinator's), §3.6's libgm live
gate (D36 explicitly does not need one), everything in §16 Slice 4.
