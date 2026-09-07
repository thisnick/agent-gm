# Review — Slice 3 (OAuth 2.1, MCP, image)

**Range reviewed:** `d597219..40387c4` (6 commits, branch `slice-3/oauth-mcp-image`),
then the delta `40387c4..2f51bf7` (one commit, the three admin-DTO fixes from live gate 25).
**Verdict is against `2f51bf7`.**
**Reviewed on:** `review/slice-3`, worktree `.claude/worktrees/review-slice-3`
**Spec:** `plans/AGENT_GM_SPEC.md` §8, §9, §12, §13.5/13.6, §14.1/14.2, §16 Slice 3, §17, §18 (D25, D29, D35);
`docs/mcp.md`, `docs/oauth.md`, `docs/deploy.md`.
**Prior review:** `git show origin/review/slice-2:REVIEW-slice-2.md` — its lesson (drive the real
binary; doubles hide wiring bugs) is the method here. Every OAuth and MCP claim below was driven
against `bin/agent-gm serve` (fake backend, `AGENT_GM_TRUSTED_PROXY_CIDRS=127.0.0.1/32` so each probe
group had its own `X-Forwarded-For` source), with `curl`, the real `bin/agm`, the official MCP
TypeScript SDK client, the pinned conformance suite, and Docker.

## Verdict

**Accept with required fixes** (against `2f51bf7`).

Every security invariant I could name held when I drove it: code single-use with replay revoking the
grant, PKCE S256 with `plain`/missing refused and a wrong verifier consuming the code, refresh
rotation with reuse revoking the family, byte-exact `iss`/`resource`/challenge, the §9.3 redirect
table row by row, the generic enrollment message byte-identical across four failure kinds, the
eleventh attempt refused per source and per context, the durable 30-failure cooldown surviving a
restart, approval required before any token exists, revocation effective on the next request, the
cookie-handle binding, CSRF form token, hidden-echo checks, Origin on `/mcp`, §9.9 headers on every
OAuth answer shape, and nothing secret in the database, WAL, shm, log or audit. The counts reproduce
(546 top-level / 1218 with subtests at `40387c4`; 547 / 1224 at `2f51bf7`; 0 skips; race-clean).
Conformance (30 run, 5 pass, 25 baselined, both directions really enforced), the image (nonroot,
no shell, healthcheck subcommand, stamped commit on both surfaces, multi-arch build, compose with
three variables) all reproduce.

It is not a clean accept because:

- **R-1** the `resources/read` failure shape is not a valid MCP result — the official SDK client
  throws before the model sees anything. Deviation 4 is wrong and is a live-gate-29 risk for any
  client that follows a `resource_link`.
- **R-2 / R-3** two load-bearing invariants of §9.5 — *approval before a code* and *the request ID
  alone conveys no authority* — are correct in production but have **no test**: my plants P24 and
  P15 survived the full suite. A future edit can remove either silently.
- **R-4** `internal/oauth` has no comparison-site audit although `compare.go:5` says it has one;
  plant P2 (`==` on a secret compare) survived.
- Five smaller spec deviations (R-6 … R-10) and the untested race windows (R-5).

**Live gates.** Gate 25 already passed on `40387c4` per the coordinator. **Gate 29 may run on
`2f51bf7`** for the flow it names (connect, enrol, approve, list, read a message, send one text,
revoke) — none of that touches `resources/read`. **R-1 must land before the connectors are left
in the owner's hands**, because claude.ai will follow the `resource_link` that `get_attachment`
returns for any attachment over the inline limit and will get a protocol error rather than an
answer. Watch items for the gate, none of them defects: an authorize request that omits
`resource` is refused `invalid_target` by §9.4 (verified) — if ChatGPT omits it the connector
fails by design; a DCR request naming `client_secret_basic`/`client_secret_post` is refused
(verified) — the metadata advertises `none` only; `initialize` with an unknown protocol version
is answered with `2026-07-28` (verified), which a client must accept or disconnect.

---

## Evidence I ran

```
$ devbox run check                       # at 40387c4 and again at 2f51bf7
0 issues.
ok  github.com/thisnick/agent-gm/cmd/agent-gm … (18 packages ok, 5 without tests)
?   github.com/thisnick/agent-gm/internal/oauth   [no test files]
no-real-numbers: clean
EXIT=0

$ CGO_ENABLED=1 go test ./... -count=1 -v      # 40387c4: RUN 1218, top-level 546, FAIL 0, SKIP 0
                                                # 2f51bf7: RUN 1224, top-level 547, FAIL 0, SKIP 0
$ devbox run conformance
Running active suite (30 scenarios) … Total: 5 passed, 25 failed
conformance: 30 scenarios
conformance: the baseline holds in both directions
EXIT=0
$ devbox run image-smoke
image: USER is nonroot:nonroot / no shell, no curl / agent-gm healthcheck exited 0 inside the container
image: agent-gm 40387c4cb61c4ba73e6f88356ce82e5eff1a55d8 (libgm pinned at be48a58)
$ devbox run image
image: built linux/amd64,linux/arm64 (not pushed)
$ docker compose -f compose.example.yml -p rs3compose up -d --no-build --wait   # smoke image, 3 env vars
Container rs3compose-agent-gm-1 Healthy ; healthz: {"status":"ok"} 200
user=nonroot:nonroot readonly=true health=healthy
```

**The server I drove:** `bin/agent-gm serve` on `127.0.0.1:18795` (data dir `/tmp/rs3/data2`,
`AGENT_GM_BACKEND=fake`, `AGENT_GM_ALLOW_FAKE=1`, `AGENT_GM_TRUSTED_PROXY_CIDRS=127.0.0.1/32`), with
two fake accounts paired through `POST /v1/pairing/start`, real sends, real uploads, real
attachments. Rebuilt and restarted at `2f51bf7` for the delta. My first instance (`18790`, no
proxy) is where I discovered that 25 registrations from one source trip §9.3's 20/hour budget
and every later probe from that source silently ran with an empty client — the server was right,
my batch was not, and every result from that batch was redone on the second instance.

---

## Findings

### R-1 — `resources/read` failures are not valid MCP results; the official client throws (**high — live-gate-29 risk**)

**Spec:** §8.2 "Resources"; MCP spec (2025-06-18 and 2025-11-25), *Resources → Error Handling*:
"Servers SHOULD return standard JSON-RPC errors for common failure cases: Resource not found:
`-32002`; Internal errors: `-32603`." `ReadResourceResult` requires `contents`; `isError` is a
field of `CallToolResult` only.

**Evidence.** `internal/mcp/resources.go:60,64,69` answer a scope refusal, an unknown URI, and a
missing attachment as `rpcResult(errorResult(...))` — a result with `isError: true`,
`structuredContent.error`, and no `contents`. Driven:

```
$ curl …/mcp -d '{"method":"resources/read","params":{"uri":"agm://attachments/att_nope"}}'
{"result":{"content":[{"text":"No such attachment.","type":"text"}],"structuredContent":{"error":{"code":"not_found",…}},"isError":true}}
$ node client.mjs …                      # @modelcontextprotocol/sdk, StreamableHTTPClientTransport
readResource unknown scheme THREW: $ZodError
readResource att_nope THREW: $ZodError
readResource real OK: {"n":1,"mime":"image/png","blob":2680,"text":0}     # the success path is fine
```

The success path is correct (text media as `text`, everything else as `blob`, verified for
`text/plain` and `image/png`). The failure path never reaches the model at all: the SDK rejects the
response before the application sees it. The implementer read §8.2's *isError semantics* paragraph
as covering resources; that paragraph is about tool results (the shape it names,
`structuredContent.error` plus a text block, exists only for `tools/call`). **Deviation 4 is
wrong; the JSON-RPC error is correct.**

**Fix required.** In `resources.go`: unknown URI and missing attachment → JSON-RPC error
`-32002` with `data: {"uri": …}`; a scope refusal on `resources/read` → JSON-RPC error (`-32002`
is defensible: to that caller the resource does not exist; or `-32603`), never an `isError`
result. Update the three `resources-read-*` reasons in `scripts/mcp-conformance-baseline.yaml`
(they will still fail on the `test://` fixtures, but the "same reporting difference" sentence
becomes false). Add one sentence to `docs/mcp.md` and one to spec §8.2 saying the `isError`
rule is for tool calls and resources use `-32002`. Test 19's "both directions" should gain a
`resources/read` row.

### R-2 — No test proves approval is required before a code is minted (**high — test gap on §9.5's central invariant**)

**Spec:** §9.5 "There is no self-service. A connector cannot get a token unless the owner does two
separate things … Expired, completed or still-pending answers `409`."

**Evidence.** Plant **P24** (`internal/oauth/requests.go:157`, `case store.AuthRequestApproved,
store.AuthRequestPending:` — `complete` mints a code for a pending request) **survived the full
suite at both SHAs** (18 packages ok). Production is correct — driven twice:

```
-- complete BEFORE approval:  status=409 {"error":"invalid_request","error_description":"this request is not waiting to be completed"}
-- tokens for that client afterwards: 0
```

**Fix required.** A test in `cmd/agent-gm/slice3_oauth_test.go` against `buildServer`'s real
handler: submit a valid code, do **not** approve, `POST /complete` with the cookie and form token
→ `409`, and `authorizations` for that client is empty; then deny → `303` `error=access_denied`
(also untested: my drive shows it works). Doc-comment the plant and date.

### R-3 — No test proves the request ID alone conveys no authority (**medium — test gap**)

**Spec:** §9.5 "Without a valid cookie every `/oauth/requests` route answers `404`: the request ID
alone conveys no authority." Test 7 covers only the *no cookie* case.

**Evidence.** Plant **P15** (`internal/oauth/requests.go:46`, the `equalHashHex(sum(domainCookieHandle,
handle), req.HandleHash)` binding removed) **survived** (18 ok). With it removed, any browser that has
loaded *any* authorization screen holds a valid context cookie and can `GET`, poll, and `complete`
somebody else's request by ID — consuming it (a completed request can never mint a second code)
and reading the code from the `Location` header. PKCE stops the code being exchanged, so the
consequence is denial of the victim's login plus an oracle on request state, not a token; still,
it is the exact property the spec sentence exists for. Production is correct — driven:

```
-- page with a DIFFERENT valid cookie (other browser): 404 "No such route."
-- other browser's cookie with first form (POST /oauth/authorize): 400 "this page does not belong to this browser session"
```

**Fix required.** Extend test 7: two screens, two cookies; cross them on `GET`, `/status` and
`/complete` → `404` each time.

### R-4 — `internal/oauth` has no comparison-site audit; `compare.go` claims one (**medium — §12.1**)

**Spec:** §12.1 "A unit test enumerates every comparison site and fails on a `==` or a `bytes.Equal`
against a secret-derived value."

**Evidence.** `internal/oauth/compare.go:5` says "comparison_audit_test.go enforces that by parsing
this package's own source, exactly as `internal/authz` does." `ls internal/oauth/*_test.go` → **0
files**; `go test` reports `internal/oauth [no test files]`; `internal/authz/comparison_audit_test.go:122`
reads only its own directory. Plant **P2** (`compare.go:211`, `equalConstantTime` → `presented ==
expected`) **survived** (18 ok). That function guards the form token (`authorize_post.go:81`,
`requests.go:142`) and the hidden echoes. Production today is constant-time everywhere I read
(`hmac.Equal` over fixed-length digests in `compare.go:59,196,211,219`; the enrollment hash is
plain SHA-256 by design so `sha256sum` can reproduce it — verified equal).

**Fix required.** Either add the file the comment names, or make the authz audit walk
`internal/oauth` too. The audit must fail on P2.

### R-5 — The atomic halves of rotation and consumption are untested (**medium — test gap**)

**Spec:** §9.6 "A rotation, its audit record, and the family revocation that reuse triggers all commit
in one transaction"; §9.8 "the transaction re-reads the row before cascading, so revocation stays a
single atomic decision".

**Evidence.** Plants **P4** (`internal/authz/service.go:488`, the in-transaction `cur.Spent() ||
cur.Revoked()` re-read disabled) and **P22** (`internal/store/oauth.go`, `ConsumeAuthorizationCode`'s
`AND consumed_at_ms IS NULL` dropped) both **survived** (18 ok). Test 9 and test 8 exercise
sequential reuse, which the read-only pre-check catches; only the concurrent case — two
presentations of one refresh token racing the single writer, the everyday "two tabs refreshed at
once" — reaches the transactional re-check, and nothing tests it. With P4, both racers mint and the
family is never revoked. Plant **P5** (`service.go:411`, refresh token no longer bound to
`client_id`) also **survived**; production refuses it (driven: other client → `invalid_grant`,
rightful client afterwards → still works).

**Fix required.** A concurrency test: N goroutines present one refresh token through the real
service; assert exactly one succeeds and, if any second one reached the transaction, the family is
revoked and `auth.refresh_token_reuse` was written. Same shape for one authorization code. Add the
wrong-`client_id` case to test 9.

### R-6 — `approve` with `{"scopes": []}` approves instead of `invalid_request` (**low — §9.5**)

**Spec:** §9.5 "`scopes` … may only narrow the browser-selected set; widening or **empty** is
`invalid_request`."

**Evidence.** `internal/oauth/admin.go:136` `if len(in.Scopes) > 0 { … }` treats present-but-empty
as absent. Driven: `empty []: {"status":"approved","granted":["messages:read","messages:write"]}`.
Widening → `invalid_request` and narrowing → granted subset both verified.

**Fix required.** Distinguish `nil` from `[]` (decode into `*[]string` or track presence) and answer
`invalid_request` with `details.parameter: "scopes"` for an empty list.

### R-7 — `/oauth/revoke` without `client_id` revokes admin-bootstrap sessions and any client's tokens (**low — §9.6**)

**Spec:** §9.6 "`POST /oauth/revoke` takes `token` and `client_id`"; "The admin bootstrap … is a
separate credential path and never crosses endpoints."

**Evidence.** `internal/authz/oauth.go:464` `if clientID != "" && auth.Client != clientID { continue }`
— an absent `client_id` skips the binding, and nothing checks `auth.Kind`. Driven:
```
/oauth/revoke token=<admin access> (no client_id): 200 ; admin access afterwards: 401
/oauth/revoke token=<oauth access> (no client_id): 200 ; oauth access afterwards: 401
```
The presenter already holds the token, so this is a path-crossing and a contract deviation rather
than an escalation.

**Fix required.** Require `client_id` at `/oauth/revoke` (`invalid_request` when absent) and ignore
tokens whose authorization `Kind != oauth`, still answering `200`.

### R-8 — `POST /oauth/requests/{id}/complete` accepts an absent `Origin` (**low — §9.5**)

**Spec:** §9.5 "`/complete` requires the cookie, the waiting page's form token, and a same-origin
`Origin`." (Contrast §9.4's `/oauth/authorize`, where it is "when present".)

**Evidence.** `requests.go:133` calls the shared `checkOrigin`, whose `authorize_post.go:330` returns
nil for an empty `Origin`. Driven: completion with no `Origin` → `303` (the conformance harness
relies on it). CSRF is still covered by the derived form token, so this is a contract deviation.

**Fix required.** Require a present, same-origin `Origin` on `/complete` only, and send one from
`scripts/conformance-harness.mjs` and the `agm auth login` test.

### R-9 — An unstamped build reports a `source_url` that does not resolve (**low — test 28**)

**Spec:** §16 test 28 "a `source_url` that resolves — the AGPL §13 obligation of §1.4".

**Evidence.** `cmd/agent-gm/serve.go:642` `buildCommit()` falls back to `"unknown"` and the URL is
built as `sourceURLBase + "/tree/" + commit`. Driven with `go build -buildvcs=false` and with
`-X main.commit=unknown`: both surfaces agree (good) on
`"source_url":"https://github.com/thisnick/agent-gm/tree/unknown"`, which is a 404. The image is
stamped (`COMMIT` build arg; verified `40387c4…` inside the container), and a git-tree build reads
`vcs.revision`, so this only bites ad-hoc builds; the tests have no unstamped row.

**Fix required.** When the commit is unknown, `source_url` = the repository root (which resolves),
and a test row for the unstamped build asserting both surfaces agree and the URL has no `/tree/unknown`.

### R-10 — Expiry audit rows are never written; pairing under `serve` writes no `account.*` rows (**low — §12.4**)

**Spec:** §12.4 lists "enrollment-code … expiry" and "authorization request … expiry", and
`account.paired`/`account.resumed` with `account_id`.

**Evidence.** Expiry is derived at read time (`requests.go:301` comment "derived rather than stored")
and the admin list shows `expired` correctly (5 rows after 15 min), but
`select kind, count(*) from audit_events group by kind` after a session with 5 expired requests and
an expired enrollment code contains no `*.expired` kind. The same query after two pairings and three
resumes on the real binary: **no `account.*` kind at all, and `count(*) where account_id is not
null` = 0.** The pairing path is Slice 2 code; recorded here because §12 is in this review's scope
and the owner should decide whether "what happened to this account" is answerable today.

**Fix required (expiry):** either write the expiry row at the moment expiry is *observed* (first
derived read), or amend §12.4 to say expiry is derived and not audited. **Flag to the owner
(pairing):** verify `account.paired` is emitted by `serve`'s pairing path and open a Slice 2 fix.

### Notes (not required)

- Plants **P16** (durable budget not consulted on the `authorization_code` grant), **P20** (`resource`
  at the token endpoint unchecked), **P25** (the 8/32 in-flight gate ×1000) survived. §9.8's list of
  durable-budgeted endpoints does not include the code grant, §9.6 makes `resource` optional there,
  and §8.1's gate has no acceptance test; all three are implemented (read and, for the first two,
  driven) but a test each would cost little.
- `send_message`'s first response under the fake carries `message_id: null` (`operation.status:
  succeeded`); the idempotent repeat carries the ID. §8.3 step 4 promises "the result carries a
  `message_id`". Slice 2 behaviour (REST answers the same); worth a sentence in the instructions
  block or a fix in the send path.
- `agm auth login` writes to `$XDG_STATE_HOME/agent-gm/credentials.json` when
  `AGENT_GM_CREDENTIALS_FILE` is unset. The implementer's test isolates it (`t.TempDir()`). My own
  first run did not, and wrote a `default` profile for my throwaway server into the real store; I
  removed it (the file then held only that profile). I cannot prove whether a prior profile existed
  there — the lock file predates my run — so the owner may need `agm auth login --admin` once.
- The reviewer's process note from Slice 2 stands: a per-source budget you trip yourself looks
  exactly like a broken server. Give every probe group its own source.

---

## The five declared deviations

| # | Deviation | Assessment |
|---|---|---|
| 1 | `/mcp` challenge names `messages:read messages:write` | **Accept.** §9.2 is literal; verified byte-exact on 401 and 403; plant P8 killed |
| 2 | Enrollment budget 10, eleventh refused | **Accept.** §9.4/test 6 say "eleventh … is `429`"; verified `1:200 … 10:200 11:429(Retry-After: 60)` per source and per context, fresh page does not reset, survives restart |
| 3 | `conversation_messages_list` as an exclusion | **Accept**, spec needs the row. §8.2's table omits `GET /v1/conversations/{id}/messages`; the reason given (same rows as `list_messages` with `conversation_id`) is sound. Spec-writer: add it to the exclusions table so test 16 and the spec agree |
| 4 | `resources/read` unknown URI → `isError` result | **Reject.** See R-1 |
| 5 | `agm auth login --admin` rejects `--no-browser` | **Accept.** Verified: `agm: --no-browser belongs to the OAuth path; --admin presents the admin secret and opens no browser` |

---

## Security invariants, as driven

Every row: what I did, what happened, where the code is. SHA `2f51bf7` unless noted; the OAuth and
MCP source is byte-identical between the two SHAs except `internal/oauth/admin.go`.

| Invariant | Driven | Result | Code |
|---|---|---|---|
| Code single-use; replay revokes the first exchange | exchanged, pinged OK, replayed | `invalid_grant`; access → 401 on `/mcp` and `/v1`; refresh → `invalid_grant`; `authorization.revoked` reason `authorization_code_replayed`, `tokens_revoked: 2` | `authz/oauth.go:186,340` |
| PKCE S256; wrong verifier consumes | wrong then right verifier | both `invalid_grant` | `oauth/token.go:87`, `authz/oauth.go:197-220` |
| `plain` / missing method refused, never defaulted | authorize with each | 302 `error=invalid_request` + `iss` + `state` | `oauth/authorize.go:163-168` |
| Refresh rotates; reuse revokes family + authorization | rotate, reuse old | reuse `invalid_grant`; new access → 401; new refresh → `invalid_grant` | `authz/service.go:391,488` |
| Widening on refresh does not spend | widen then plain | `invalid_scope`; same token then succeeds | `service.go:445` |
| Refresh bound to `client_id` | other client presents | `invalid_grant`; rightful client still works | `service.go:411` (untested, R-5) |
| Audience binding | tokens minted under `PUBLIC_URL` used on `/v1` and `/mcp` | accepted on both (one audience by design); the audience is inside the hash, so a URL change resolves to nothing | `authz/compare.go:294` |
| `iss` / `resource` byte-exact | both discovery docs, callback, error redirects | `issuer` == `AGENT_GM_PUBLIC_URL`; `resource` == it + `/mcp`; `iss` on every redirect | `oauth/metadata.go` |
| `resource` at authorize | wrong / missing | `invalid_target` redirect | `authorize.go:172` |
| `resource` at token | wrong / matching / absent | `invalid_target` (code not consumed) / OK / OK | `token.go:64` (untested, P20) |
| Redirect table §9.3 | 17 URIs at `/oauth/register` | every row as specified incl. `localhost:1` accepted, the four look-alikes refused, `https://` IP literal refused, fragment/credentials refused, `myapp:/cb` refused, `com.example.app:/cb` accepted, 501 chars refused, 11 URIs refused | `oauth/redirect.go` |
| Loopback port wildcard | registered `127.0.0.1/cb` | `:53211/cb` 200; `:53211/cb2` 400; `127.0.0.2/cb` 400; token endpoint requires the exact bound URI (port mismatch → `invalid_grant`) | `redirect.go:135` |
| Unknown client / unregistered redirect never redirect | both | 400 JSON, no `Location` | `authorize.go:79-98` |
| `state` | missing | `invalid_request` redirect without `state`; `agm` verifies `state`, `iss` presence and equality | `authorize.go:157`; `cli/oauthlogin.go` |
| Enrollment generic message | one screen, unknown/revoked/consumed/expired | 200, four pages `cmp`-identical, no pending request created, code canonicalised (lowercase, hyphenless accepted) | `authorize_post.go:165-171` |
| Enrollment budget | 11 failures per source; 11 per context across 11 sources; fresh page; restart | 11th `429 Retry-After: 60` both ways; locked source refuses a *valid* code; another source redeems it; 429 persists after restart (`cooldown_steps 1`) | `authorize_post.go:37,135`; `authz/oauth.go:101` |
| Durable token budget | 30 paced failures past the in-memory bucket | 30th sets `cooldown_steps 1`; `429 Retry-After: 60`; survives restart (`58`); `/oauth/revoke` shares it; a success does not clear the counter; in-memory bucket (burst 20) is forgiven on restart as §9.8 allows | `authz/oauth.go:81,173,435` |
| DCR budget | 21 registrations | 21st `429 Retry-After: 3600` | `register.go:321` |
| Admin-secret budget | 7 wrong secrets | `401×5` then `429` | Slice 2 |
| Approval before token | complete before approve; deny | `409`; no authorization row; deny → `303 error=access_denied` + cookie cleared; approve after deny `idempotency_conflict`; second complete `409` | `requests.go:137-159` (untested, R-2) |
| Owner revocation | `DELETE /v1/admin/authorizations/{id}` | next `/mcp` 401; refresh `invalid_grant`; repeat `changed:false` | `authz/oauth.go:486` |
| `/oauth/revoke` | nobody's / other client's / own | always `200`, empty body; other client's attempt leaves the token valid; own → 401 next call | `authz/oauth.go:430` (R-7 for absent `client_id`) |
| Credential paths | admin refresh at `/oauth/token`; OAuth refresh at `/v1/auth/refresh` | `invalid_grant` / `401 invalid_token`; **neither revokes** (both grants used afterwards) | `service.go:406` |
| `admin` never enrollable | `scope=admin`, `messages:read admin`, enrollment `scopes`/`allow_scopes` admin | `invalid_scope` redirect; enrollment `invalid_request` | `authorize.go:184`, `oauth/admin.go` |
| Challenges | no token; admin-only token | `401` / `403 insufficient_scope`; `WWW-Authenticate` byte-exact both, one header | `mcp/handler.go:256` |
| Origin on `/mcp` | evil / exact / `null` / absent | 403 before auth / 200 / 403 / accepted | `handler.go:121` |
| One `Authorization` header | two headers; `Basic`; two tokens in one value; lowercase `bearer` | 401 / 401 / 401 / 200 | `handler.go:351` |
| Transport order | 1 MiB+1 body without auth; `text/plain`; `Accept: text/html`; `GET` | 413 / 400 / 400 / 405 `Allow: POST` | `handler.go:108-151` |
| Constant-time | read every compare in `oauth/compare.go`, `authz/compare.go`, `service.go:381`, `oauth.go:451` | all `hmac.Equal` over fixed-length digests; plant P2 shows no audit over `internal/oauth` (R-4) | |
| CSRF / echoes | bad form token; tampered `state` echo; no cookie; other browser's cookie; foreign Origin on POST | 400 / 400 / 400 / 400 / 403; the code stays redeemable afterwards | `authorize_post.go:51-103` |
| §9.9 headers | screen, `poll.js`, error bodies, 404, discovery, 405 | CSP (`default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; script-src 'self'`), `no-store`, `no-referrer`, `nosniff`, `DENY` on every one | `oauth/errors.go:562` |
| Cookie | screen | `Path=/oauth; HttpOnly; Secure; SameSite=Lax`; cleared on complete and deny | `authorize.go:239` |
| Sentinel scan | admin secret, access, refresh, code, verifier, enrollment (both forms), cookie value, form token | none in `agent-gm.sqlite3`, `-wal`, `-shm`, the log, or any audit payload; `enrollment_codes.code_hash` == `printf '%s' CANON \| sha256sum`; `tokens.token_hash` 64-hex only; files `0600` in `0700` | |
| Source resolution | `X-Forwarded-For: 1.1.1.1, 127.0.0.1` / `1.1.1.1, junk` with trusted `127.0.0.1/32` | `1.1.1.1` / `127.0.0.1` (junk stops the walk); `client_source_mode=trusted_proxy` logged once | Slice 2 |
| Settings | nine §9.6 TTLs | defaults and bounds as the table; out-of-bounds `PATCH` → `invalid_request` | |

## MCP, as driven

- `tools/list`: 21 tools, names and argument sets exactly §8.2's; every argument described (counted);
  `additionalProperties: false` everywhere and enforced (`folderr` → `invalid_request details.field`);
  enums with per-value `oneOf` on `folder`, `type`, `direction`, `delivery_state`, `mode`; every tool
  has `outputSchema`; annotations match the §8.2 table row by row; every write tool's description ends
  with the fresh-key sentence; `add_reaction.emoji` names the eleven; `client_request_id` required on
  writes. Scope gating: read → 11, write → 8, delete → 2, read+write → 19, write+delete → 10;
  `tools/call` re-checks (`insufficient_scope` result naming `required_scope`); templates offered only
  with `messages:read`. `get_operation` is write-gated and `readOnlyHint: true`.
- Instructions block: byte-identical to `docs/mcp.md` "First five minutes" after unquoting (`diff`
  exit 0) and to spec §8.3; plant P13 killed by test 20; the name lint catches `room`, `portal`,
  `tombstone` in a description, a docs page and the instructions, and passes a fenced block.
- Multi-account on the real binary (test 17): `list_accounts` two rows with states; reads without
  `account_id` cover both and carry it; a write without it → `invalid_request` with
  `details.accounts` naming both, and retrying with one succeeds; the same `client_request_id`
  against the other account → `invalid_request`; `get_session` with/without; `get_health` two rows
  plus `accounts_summary`.
- Attachments (test 22): a 2 KB PNG → first block text summary, second block `image` (base64), ticket
  present; after `PATCH /v1/admin/settings {"media.inline_mcp_image_max_bytes":1024}` the same
  attachment → `resource_link`, ticket present; `text/plain` → `resource_link` + `resources/read`
  returns `text`; an upload can be sent only once. SDK `readResource` on the real URI OK.
- isError (test 19): `not_found`, `invalid_request` (unknown field, missing `q`, Google ID in place
  of `msg_`), `unsupported`, scope refusals are results; unknown tool `-32602`, unknown method
  `-32601`, batch/non-object body `-32700`; SDK sees `McpError` for those and results for the rest.
  `resources/read` failures: R-1.
- `serverInfo`: `name`, `version`, `commit`, `source_url` equal to `/v1/health` (and to
  `agent-gm version`) inside the smoke container and on the host; unstamped: R-9.
- Concurrency 8/32: implemented (`handler.go:213`), no test (P25); 12 parallel calls all 200 (they
  do not overlap long enough to prove it either way).

## Conformance and lint

- `devbox run conformance`: token minted through the whole flow (the harness's ten stages logged),
  30 scenarios, 5 pass, 25 baselined with reasons; **C1** (a baselined failure removed) → exit 1
  `NEW FAILURE prompts-list`; **C2** (passing `ping` added) → exit 1 `ping is in the baseline and
  now PASSES`. Both directions real.
- `devbox run lint-names` and its meta-test: LINT-M1..M4 above; the `mode` rule is whole-name (only
  `search_messages.mode`, a search vocabulary, is served).

## Image (tests 26, 27, 28)

`devbox run image-smoke` and `devbox run image` as quoted; `compose.example.yml` up with only the
three variables, `read_only`, `nonroot`, healthy through the image's own `HEALTHCHECK`; plant P23
(USER removed) killed by the smoke (`image runs as '65532', want nonroot:nonroot`). CI: every job
is a `devbox run` script; GHCR login and `AGENT_GM_IMAGE_PUSH` gated to `push` on `main` or a `v*`
tag; PRs build both architectures without pushing. `build-matrix` is Slice 4.

## The `40387c4..2f51bf7` delta

Six files. Pending requests carry `scopes` (effective = granted else selected) and
`authorization_id` (from `authorization_codes` via the new `Store.AuthorizationIDForRequest`);
authorizations carry `client_name` (an `OAuthClientByID` per row — fine at this size);
`approve` returns `status/scopes/granted_scopes/authorization_id`; both revocations return
`changed/revoked_at/effect` with the effect strings shared from `apierr.RevocationEffects()` so the
CLI's confirm text and the API agree. Driven on the rebuilt binary: every field present, repeat
revoke `changed:false`, unknown id `not_found`, `agm admin … --json` shows the same. One nit:
`adminAuthorizationsRevoke` maps any `RevokeAuthorization` error to `not_found`. `check`, the full
plant set, conformance and the image all re-run at `2f51bf7`.

---

## Plants

Mutations by me, applied at a named line, full suite run (`go test ./... -count=1`), reverted with
`git checkout --`. Table is the run at **`2f51bf7`**; every row had the same outcome at `40387c4`
(P15/P24/P25 were first run at `2f51bf7`). Two earlier attempts were invalid (compile errors) and
are not counted.

| Plant | Site | Mutation | Outcome |
|---|---|---|---|
| P1 | `internal/oauth/compare.go:219` | `verifyPKCE` accepts any verifier | killed: `TestSlice3Test8CodeReplayRevokesAndWrongVerifierConsumes` |
| P2 | `internal/oauth/compare.go:211` | `equalConstantTime` → `==` | **SURVIVED** (18 ok) — R-4 |
| P3 | `internal/authz/oauth.go:341` | replay no longer revokes | killed: `TestSlice3Test8…` |
| P4 | `internal/authz/service.go:488` | in-transaction reuse re-check disabled | **SURVIVED** (18 ok) — R-5 |
| P5 | `internal/authz/service.go:411` | refresh not bound to `client_id` | **SURVIVED** (18 ok) — R-5 |
| P6 | `internal/oauth/authorize.go:163` | omitted method defaulted to S256 | killed: `TestSlice3Test4Authorize` |
| P7 | `internal/oauth/redirect.go:90` | loopback becomes a suffix match | killed: `TestSlice3Test3DynamicClientRegistration` |
| P8 | `internal/mcp/handler.go:259` | challenge names three scopes | killed: `TestSlice3Test2MCPChallenge` |
| P9 | `internal/mcp/handler.go:121` | Origin check disabled | killed: `TestSlice3Test21Transport` |
| P10 | `internal/mcp/dispatch.go:189` | no call-time scope re-check | killed: `TestSlice3Test14ScopeGating` |
| P11 | `internal/mcp/tools.go:254` | `limit` loses its description | killed: `TestSlice3Test15CatalogueClaims` |
| P12 | `internal/mcp/tools.go:31` | fresh-key sentence drifts one word | killed: `TestSlice3DocsCoverTheServedCatalogue` |
| P13 | `internal/mcp/instructions.go:22` | instructions drift one comma | killed: `TestSlice3Test20InstructionsMatchTheDocs` |
| P14 | `internal/oauth/authorize_post.go:37` | per-source enrollment check removed | killed: `TestSlice3Test6EnrollmentAttemptsAreBudgeted` |
| P15 | `internal/oauth/requests.go:46` | cookie-handle binding removed | **SURVIVED** (18 ok) — R-3 |
| P16 | `internal/authz/oauth.go:173` | durable check skipped on the code grant | **SURVIVED** (18 ok) — note |
| P17 | `internal/mcp/exclusions.go:249` | a messaging route leaves every category | killed: `TestSlice3Test16TheThreeWaySplit`, `TestSlice3ExclusionsAreTheOnesSection82Names` |
| P18 | `internal/oauth/authorize.go:34` | D29 disclosure drifts | killed: `TestSlice3Test18TheScreenDisclosesGlobalScope` |
| P19 | `internal/mcp/handler.go:356` | two `Authorization` headers resolved | killed: `TestSlice3Test21Transport` |
| P20 | `internal/oauth/token.go:64` | `resource` unchecked at the token endpoint | **SURVIVED** (18 ok) — note |
| P21 | `internal/oauth/register.go:93` | client-chosen `client_id` accepted | killed: `TestSlice3Test3DynamicClientRegistration` |
| P22 | `internal/store/oauth.go` (`ConsumeAuthorizationCode`) | UPDATE unconditional | **SURVIVED** (18 ok) — R-5 |
| P23 | `Dockerfile:64` | `USER nonroot` removed | killed: `image-smoke` |
| P24 | `internal/oauth/requests.go:157` | complete mints for a pending request | **SURVIVED** (18 ok) — R-2 |
| P25 | `internal/mcp/handler.go:30-32` | in-flight gate ×1000 | **SURVIVED** (18 ok) — note |
| C1 | `scripts/mcp-conformance-baseline.yaml` | `prompts-list` removed | killed: `NEW FAILURE prompts-list` |
| C2 | `scripts/mcp-conformance-baseline.yaml` | `ping` added | killed: `ping is in the baseline and now PASSES` |
| L1–L4 | `tools.go` / `docs/mcp.md` / `instructions.go` / fenced block | `room` / `portal` / `tombstone` / exempt | killed ×3 by `TestNameLint`; L4 passes as it must |

**31 plants, 22 killed, 9 survived** (P2, P4, P5, P15, P16, P20, P22, P24, P25). Each survivor is
either a finding above or a note. After the fixes, P2, P4, P5, P15, P22, P24 must die.

---

## Rubric — §18.1, one axis: every name means what it means in Google Messages

**Score: 2.**

Names added in this slice, checked one by one: the 21 tools (`list_conversations`,
`get_conversation`, `list_messages`, `get_message`, `message_context`, `search_messages`,
`get_attachment`, `list_contacts`, `get_session`, `get_health`, `list_accounts`, `send_message`,
`start_conversation`, `mark_read`, `add_reaction`, `remove_reaction`, `update_conversation`,
`create_upload`, `get_operation`, `delete_message`, `delete_conversation`); their arguments
(`conversation_id`, `message_id`, `attachment_id`, `reaction_id`, `account_id`, `participant`,
`folder` = inbox/archived/spam, `type` = rcs/sms_mms/unknown, `recipients`, `emoji`,
`reply_to_message_id`, `force_rcs`, `upload_ids`, `client_request_id`, `delivery_state`,
`direction`, `sender`, `unread_only`, `group_only`, `include_deleted`, `include_system`, `top`,
`mode` = words/exact); the resource `agm://attachments/{attachment_id}` named `attachment`; the
scopes `messages:read/write/delete`; the OAuth nouns (`enrollment code`, `authorization request`,
`authorization`, `client`) and their routes `/v1/admin/enrollment-codes`,
`/v1/admin/authorization-requests[/{id}/approve|deny]`, `/v1/admin/authorizations`,
`/v1/admin/clients`; the CLI `agm auth login [--no-browser|--admin]`, `agm admin
enrollment-codes create|list|show|revoke`, `agm admin authorization-requests
list|show|approve|deny`, `agm admin authorizations list|show|revoke`, `agm admin clients
list|show|revoke`; the audit kinds `enrollment.*`, `authorization.*`, `client.revoked`; the
settings `oauth.*`, `admin.*`, `media.inline_mcp_image_max_bytes`; the DTO fields added at
`2f51bf7` (`client_name`, `scopes`, `authorization_id`, `changed`, `revoked_at`, `effect`).
Every messaging name is Google Messages vocabulary (conversation = thread, contact, RCS/SMS,
delete-for-me stated in the descriptions); every OAuth name is RFC vocabulary; no Matrix word
survives the lint. The one name that could mislead is `get_session`, which is a Google Messages
*session* (pairing state, `phone_responding`), not an OAuth session — its description says so
and the instructions block uses it correctly, so it passes.

---

## Spec clauses — verified / partial / gap / n/a

SHA-qualified: `verified 2f51bf7` unless a different tag is shown.

**§8.1** transport order, Origin, 1 MiB, Content-Type, Accept, single Bearer, at-least-one scope
401/403 with challenge — verified. Protocol revisions 2026-07-28 / 2025-11-25 accepted, unknown →
2026-07-28 — verified. Stateless (no session id, notifications 202) — verified. Concurrency 8/32 —
**partial** (implemented, untested, P25). `serverInfo` — verified.
**§8.2** 21 tools, arguments, required `q`/`client_request_id`, enums, closed schemas enforced by the
decoder, `outputSchema`, annotations table, fresh-key sentence, `isError` for tools, unknown tool as
JSON-RPC error, scope gating on list and call, `remove_reaction` neither → `invalid_request`,
`resources/list` empty, template only with read, attachment inline rule with ticket either way and
summary first — verified. Three-way exclusion rule — verified at `2f51bf7` via test 16 + plant P17;
`conversation_messages_list` is a deviation the spec should adopt. `resources/read` failure shape —
**failed 2f51bf7** (R-1). Emoji canonicalisation through `EmojiType` — **unverified** (Slice 2
code; description asserts it).
**§8.3** instructions byte-identical to docs and spec — verified.
**§8.4** whole-flow token, zero-scenario failure, both directions — verified.
**§9.1** routes, root 404, error shape incl. 413/405/`Allow`, unknown `/oauth` path REST 404 —
verified. **§9.2** both documents byte-exact, challenge — verified. **§9.3** DCR rules, redirect
table, 10/500 bounds, 20/hour, loopback wildcard, exact at token, DCR-only — verified; 24 h expiry
and the 60 s sweep with audit — **partial** (code `serve.go:686`, `register.go:371`, implementer's
`TestSlice3ExpiredRegistrationsAreSwept`; not observable in-session). **§9.4** parameters, defaults,
no-redirect cases, redirect error table, cookie attributes, form contents, disclosure line above the
checkboxes with accounts, POST verification order, 303, generic message, ceiling re-render, eleventh
attempt per context and per source, fresh page — verified. **§9.5** enrollment create 200 / hash only
/ `expires_in` bounds / `scopes` vs `allow_scopes` / never `admin`; list, show, revoke idempotent;
waiting page and `/status` shape and `no-store`; 404 without cookie; approve narrow-only, widen
`invalid_request`, not-pending `idempotency_conflict`; deny; complete 303 with code/state/iss/scope
and cookie cleared; denied → `access_denied`; 409 for other states; second code impossible;
`GET /oauth/requests/{id}` 200 for every state — verified. Approve with empty `scopes` — **failed**
(R-6). Expired request → `invalid_request` on approve — **unverified** (no pending request had
expired unapproved during the session; the list derives `expired` correctly). Complete requires a
present `Origin` — **failed** (R-8). **§9.6** all token rules, TTL table, admin schedule, admin
never-widen (Slice 2), paths never cross — verified; `/oauth/revoke` without `client_id` — **failed**
(R-7). **§9.7** scope table, admin bootstrap carries the three, `admin` never enrollable — verified.
**§9.8** three budgets 60/min burst 20 + durable 30/15 min doubling cooldown surviving restart,
checked before the token, `Retry-After`, success does not clear, read-only lookup before the write
transaction — verified (the lookup order by code read `authz/oauth.go:177` and `service.go:371`, and
by the implementer's `TestThePresentedValueIsLookedUpBeforeAnyWriteTransaction`). **§9.9** —
verified on every answer shape.
**§12.1** constant-time everywhere — verified by reading and by P1; the audit's coverage of
`internal/oauth` — **failed** (R-4). Secrets never in argv; hashes only — verified. **§12.2** logs:
no bodies, no tokens, no addresses (`acct_` only) — verified over the session log. **§12.3** table
rows exercised: enrollment, DCR, admin-secret, unauthenticated token/revoke — verified; `/mcp` in
flight — partial; reads/mutations/admin per-authorization buckets — n/a (Slice 2). Client source —
verified. **§12.4** kinds `enrollment.created/consumed/revoked`, `authorization.created/approved/
denied/revoked/token_issued/token_refreshed/token_failed`, `auth.admin_secret_failed`,
`settings.changed` — verified present with ID-only payloads; `*.expired` — **gap** (R-10);
`client.revoked` — n/a in-session (24 h); `account.paired`/`account.resumed` with `account_id` —
**gap** (R-10, Slice 2 code); refresh-token reuse row — verified via `auth.refresh_token_failed`
after the reuse.
**§13.5** name lint three rules and meta-test — verified. **§13.6** every job a `devbox run`; `image`
job builds both arches and pushes only on `main`/tags — verified by reading `ci.yml`; CI run
34077011493 green per the coordinator (not re-run by me). **§14.1** Dockerfile as specified plus the
owned `/data` and `HEALTHCHECK` exec form — verified; **§14.2** tags and digest printing —
verified by reading `scripts/image.sh` (no push performed).
**§16 Slice 3 tests:** 1–15, 16 (test + plant), 17, 18, 19 (tools half; resources half failed), 20,
21, 22, 23, 24, 26, 27, 28 (stamped; unstamped row failed) — verified; **25** — live, passed per the
coordinator on `40387c4`, not run by me; **29** — live, not run; may run on `2f51bf7` with R-1 as a
declared limitation and should be re-run after R-1 lands.
**§17 / §18** D25 (admin session carries the three) — verified; D29 (global scopes, disclosure) —
verified; D35 (image in this slice) — verified.

**Could not verify:** emoji canonicalisation on the reaction path; the 24 h registration expiry and
its audit; approve on an expired request; the in-flight gate under real contention; CI itself;
gates 25 and 29.

---

## Required before the slice is closed

1. R-1 — `resources/read` JSON-RPC `-32002`/`-32603`; baseline reasons; docs/spec sentence.
2. R-2 — test: complete before approval is 409 and mints nothing (kills P24).
3. R-3 — test: cross-cookie `GET`/`/status`/`/complete` are 404 (kills P15).
4. R-4 — comparison-site audit over `internal/oauth` (kills P2).
5. R-5 — concurrency test for refresh reuse and code consumption; wrong-`client_id` refresh test
   (kills P4, P22, P5).
6. R-6, R-7, R-8 — the three contract deviations, each one line of logic and one test.
7. R-9 — unstamped `source_url` resolves; a test row.
8. R-10 — decide expiry auditing; owner to check `account.paired` under `serve`.
9. Spec: add `conversation_messages_list` to §8.2's exclusions; say the `isError` rule is for tools.

---

## Confirm pass — `34c6e26` (the implementer's fixes): **ACCEPT**

Synced with `git reset --hard 34c6e26` (23 files, +1350/−43). `devbox run check` green
(`check exit=0`, 0 issues, no-real-numbers clean). Driven on a fresh build against a fresh data dir:

- **R-1** `resources/read`: `test://x` and `agm://attachments/att_nope` → JSON-RPC `-32002`
  "no such resource: …"; a `messages:write`-only token → `-32600` "resources/read requires the
  messages:read scope". SDK client: `readResource … THREW: McpError code=-32002` (a clean
  protocol error the client can act on, no more `ZodError`); tool results unchanged. `docs/mcp.md`
  has the row; the three baseline reasons keep only the fixture half.
- **R-6** approve `{"scopes":[]}` → `invalid_request`, request still `pending`; absent key approves.
- **R-7** `/oauth/revoke` with an admin-bootstrap token, with and without `client_id` → `200`,
  admin access still `200` afterwards; own OAuth token with `client_id` → `200` then `401`.
- **R-8** `/complete` with no `Origin` → `403`; foreign → `403`; same-origin → `303`.
- **R-9** `-buildvcs=false -X main.commit=unknown` build: both surfaces report
  `commit: "unknown"`, `source_url: "https://github.com/thisnick/agent-gm"` (resolves, 200).

Plants re-planted at `34c6e26` (full suite each, reverted after):

| Plant | Site | Outcome |
|---|---|---|
| P2 | `oauth/compare.go:211` `==` | killed: `TestNoSecretIsComparedWithEquals` |
| P4 | `authz/service.go:504` in-tx reuse re-check off | killed: `TestARefreshThatLosesTheRaceIsReuse`, `TestSlice3ARacedRefreshRotatesOnce` |
| P5 | `authz/service.go:424` client binding off | killed: `TestSlice3RefreshIsBoundToItsClient` |
| P15 | `oauth/requests.go:46` handle binding off | killed: `TestSlice3TheRequestIDIsBoundToOneBrowser` |
| P22 | `store/oauth.go` consume UPDATE unconditional | killed: `TestAnAuthorizationCodeIsConsumedOnce` |
| P24 | `oauth/requests.go:163` complete mints for pending | killed: `TestSlice3CompletionNeedsAnApproval` |

All six former survivors die. Still open and deliberately left to the owner: **R-10** (`*.expired`
audit kinds; Slice 2 pairing rows carry no `account_id`) and the two spec edits (§8.2 `isError`
rule is `tools/call`-only; `conversation_messages_list` in the exclusions table). P16/P20/P25
remain untested-but-implemented notes, not blockers.

**Gate 29 may run on `34c6e26`.** Nothing must land first.
