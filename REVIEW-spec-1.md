# REVIEW-spec-1 — plans/AGENT_GM_SPEC.md @ 6bf4034

Reviewer: adversarial pass against sources. Library verified against
`/home/nick/code/mautrix-gmessages` at `be48a58` (read-only). Reference
conventions verified against `/home/nick/code/agent-mx`.

## Verdict

**Accept with required changes.** Do not start Slice 1 against §3 as written.

Two independent classes of problem.

**§3 is not derived from the pin, though it says it is.** Its preamble reads
"Everything in this section is derived from mautrix-gmessages at the pinned
commit". Three load-bearing claims are absent from that tree entirely — the
status-4 stale-ConfigVersion story that justifies decision D3 (F-1), the
`events.BrowserActive` takeover signal that is the sole evidence for OQ-2
(F-2), and `events.QR` (F-3). Two more are contradicted by the code: the
multiple-gaia-devices error (F-4) and "refreshing cookies needs a full re-pair"
(F-5), the latter changing the answer to OQ-1. An implementer following §3
literally would write code that waits on events that never fire.

**Slice 2 cannot execute itself.** No credential in Slice 2 can hold a
messaging scope, so nothing can call the routes its own tests exercise (F-22);
several promised response fields have no column or DTO behind them (F-23); and
`gm.Backend` is missing the methods that `/v1/health`, `/typing`,
`contacts?top=` and the media-pending path require, while §2.2 forbids reaching
past it (F-24).

Everything else is bookkeeping, and there is a great deal of it done right: the
Agent MX port is faithful clause by clause with zero Matrix leakage (F-21), and
every constant, signature and enum value in §3 that I could check against
`be48a58` — thirty-odd of them — was correct.

Findings are `F-n`. Severity: **C**ritical (wrong contract, will produce wrong
code), **M**ajor, **m**inor.

---

## Findings

### F-1 (C) §3.7 — "status 4 = stale ConfigVersion" has no source

The spec makes this the centrepiece of §3.7, mints error code
`config_version_stale`, adds two `GET /v1/health` fields for it, and justifies
decision **D3** (the whole pin-and-bump policy) with it.

Evidence: nothing in the pinned tree mentions it.
`grep -rni "config.*stale|stale.*config|status 4"` over the whole checkout
returns zero hits. The proto declares only `UNKNOWN=0, SUCCESS=1,
CREATE_RCS=3` (`pkg/libgm/gmproto/client.pb.go:181-183`), and the only
non-SUCCESS handling upstream is a generic
`no conversation data in response (status: %s)`
(`pkg/connector/startchat.go:233,238`).

Smallest change: keep the error code and the health fields, but relabel the
status-4 row and D3 as a **field observation with its own source line**, not a
library citation, and make the detection rule the ConfigVersion diff alone
(`status != SUCCESS && compiled.Y/M/D != live.Y/M/D`). Do not assert `4` is the
symptom until something outside this spec says so.

### F-2 (C) §3.4 — `*events.BrowserActive` is never emitted

`events.BrowserActive{SessionID}` is defined at
`pkg/libgm/events/useralerts.go:3-11`, but `NewBrowserActive` **has no callers**
anywhere in the tree, and the only code that switches on the type is
`pkg/libgm/gmtest/main.go:131`. Agent GM will wait for an event that never
arrives.

Worse, the spec's interpretation is also wrong. The real signal is
`*gmproto.UserAlertEvent` with `AlertType_BROWSER_ACTIVE`
(`pkg/libgm/event_handler.go:248-252`), and the session-ID comparison the spec
quotes lives in the *connector*
(`pkg/connector/handlegmessages.go:301-327`): it compares the connector's own
cached `gc.sessionID` against `Client.CurrentSessionID()` and means **"our
session changed, resync"** — not "another device took over".

Smallest change: delete the `BrowserActive` row; add `BROWSER_ACTIVE` to the
`AlertType` list in §3.4 with the resync semantics, and remove "this is the
signal that the owner opened Google Messages Web".

### F-3 (M) §3.4 — `*events.QR{URL}` is never emitted either

`grep -rn "events.QR{"` over the tree: zero hits. `events.QR` (`events/qr.go:7`)
is dead at this pin. QR payload strings are only ever **return values** of
`StartLogin` (`pair.go:18-31`) and `RefreshPhoneRelay` (`pair.go:111-132`).
Smallest change: delete the row; §3.2 step 3 and §11.4 already have it right.

### F-4 (C) §3.1, §3.2 step 3, §3.5 — multiple gaia devices do not error

Spec: "picks **the single primary**… if there is more than one,
`ErrHadMultipleDevices`", and §3.5 maps `pairing_multiple_devices` as a
standalone 409.

Code (`pkg/libgm/pair_google.go:353-370`): zero primaries →
`ErrNoDevicesFound`; **more than one → it sorts by `LastSeen` newest-first,
logs a warning, and picks `primaryDevices[c.GaiaHackyDeviceSwitcher % len]`**.
`ErrHadMultipleDevices` is returned **only** wrapped inside
`ErrPairingInitTimeout`, and only when the CLIENT_INIT round-trip times out and
`len(primaryDevices) > 1` (`pair_google.go:391-397`).

This matters operationally: a Google account with two Android devices will pair
against the wrong phone, silently, and the only remedy is
`Client.GaiaHackyDeviceSwitcher` — which §3.1 does not list at all.

Smallest change: correct §3.1 and §3.2 step 3; demote
`pairing_multiple_devices` to a `details` field on `pairing_init_timeout`; add
`GaiaHackyDeviceSwitcher` to the §3.1 contract table and an
`agm pair --google --device-index N` flag to §11.3.

### F-5 (C) §3.2, OQ-1 — cookie refresh without re-pair **is** supported

Spec: "Refreshing cookies without a full re-pair is **not supported**", and
OQ-1 asks the owner to choose a flow on that basis.

Code: `pkg/connector/login.go:246-252` (`StartWithOverride`) and `:255-289`
(`SubmitCookies`) re-authenticate an **existing** pairing with fresh cookies —
gated on `Session.TachyonAuthToken != nil && Session.PairingID != uuid.Nil`
(`:249`), verified same-account via
`Config.GetDeviceInfo().GetEmail() == Session.Mobile.GetSourceID()` (`:268`),
then `FetchConfig` + `Connect` + save. On failure it restores by
`SetCookies(nil)` (`:289`).

Smallest change: replace the sentence with the re-auth path; add
`agm pair --google --refresh-cookies`; restate OQ-1 without the "only recovery
is a full re-pair" premise. Also: the cross-reference "(§18 OQ-4)" is wrong —
OQ-4 is the retention question, and no OQ covers this.

### F-6 (M) §3.2 flow B — the required cookie set is wrong

Spec: "at minimum `SAPISID`, plus the standard auth cookies".

Code (`pkg/connector/login.go:201-210`): required =
**`SID, HSID, OSID, SSID, APISID, SAPISID`**; optional `__Secure-1PSIDTS`.
Domains matter: **`OSID` is host-scoped to `messages.google.com`** (`:202`),
every other one to `.google.com` (`:203-208`). `SAPISID` alone is not
sufficient; its separate job is the `SAPISIDHASH` `Authorization` header
(`client.go:66-70`, `http.go:83-86`).

Smallest change: name the six required cookies and their two domains in §3.2
and in `docs/pairing.md`. This is also the load-bearing input to the pairing-UX
decision below.

### F-7 (M) §3.2 step 1 / §12.1 — cookies are kept for the whole session

Spec: "the file is read once and not copied."

Code: the cookies live in `AuthData.Cookies` and are attached to **every**
subsequent request — `http.go:58`, `client.go:407`,
`session_handler.go:62,88,151,382`, `longpoll.go:519` — and are rotated in
place by `UpdateCookiesFromResponse` (`client.go:73-81`). `HasCookies()` is
part of `IsLoggedIn()` (`client.go:361`) and selects the unpair path
(`pair.go:164`).

Consequence the spec does not state: **`session.enc` contains live,
full-privilege Google account cookies**, not a scoped token. That is a much
larger blast radius than a tachyon token and it belongs in §12.1 and in the
threat model for the headless/`--server` deployment.

### F-8 (M) §3.4 — fatal poll errors are 401 **or 403**

Spec keys credential invalidation on `ListenFatalError` whose "error string is
`"http 401 while polling"`". `longpoll.go:539-547` makes
`StatusUnauthorized` **or `StatusForbidden`** fatal, both rendered
`http %d while polling` (`events/ready.go:84-87`). A 403 would fall into the
"supervisor retries `Reconnect` with backoff" branch and loop forever on dead
credentials.

Smallest change: match on `errors.As(err, &events.HTTPError{})` with
`Resp.StatusCode ∈ {401, 403}`, never on the string.

### F-9 (M) §3.4 — library dedup drops the rest of the batch, not just the dupe

Spec describes `deduplicateUpdate` as dropping the duplicate update. It does
not: on a hit the loop `return`s, abandoning **every remaining part of the
batch** — `event_handler.go:263-266` (conversations) and `:272-275`
(messages). That is message *loss*, not duplication, and Agent GM's own dedup
(§5.4) cannot recover what it never saw.

Smallest change: say so in §3.4, and require a periodic reconciliation sweep
(`ListConversations` + `FetchMessages` since the last stored timestamp) in §5.4
rather than treating the live stream as complete.

### F-10 (M) §3.7, §4.4 — tombstone range and unmapped statuses

`MessageStatusType` tombstones run **200–279**, not "200–299", and
`MESSAGE_DELETED = 300` sits outside the range entirely
(`pkg/libgm/gmproto/*.pb.go`). §4.4 maps neither `300`, nor
`INCOMING_DOWNLOAD_CANCELED(110)`, nor `INCOMING_UNKNOWN_CONTENT_TYPE(116)` —
all three fall to `delivery_state = unknown` silently, which is exactly the
failure mode the reviewer brief calls out.

Smallest change: `200–279` for tombstones; map `300 → deleted`, `110 →
download_failed`, `116 → received`; replace the two glob rows
(`INCOMING_*_DOWNLOADING`, `INCOMING_DOWNLOAD_FAILED*`) with the explicit
numeric list, since §16 slice-1 test 4 already demands enumeration over
sampling for the error table.

### F-11 (M) §3.2, OQ-2 — the paired-device slot claim is unsupported

§3.2 asserts "a small, undocumented number of simultaneously paired web
devices", that pairing Agent GM "may evict" Messages Web and vice versa, and
OQ-2 asks the owner to give up Messages Web on that basis.

Nothing at the pin supports it. The only device-state signals are
`BROWSER_INACTIVE(1)`, `BROWSER_INACTIVE_FROM_TIMEOUT(7)` and
`BROWSER_INACTIVE_FROM_INACTIVITY(8)`
(`pkg/connector/handlegmessages.go:298,335-340`), all of which describe *this*
session going idle and are paired with a `BROWSER_ACTIVE` recovery that
triggers a resync — i.e. coexistence with flapping, not eviction. The gaia
device enumeration itself expects and tolerates several devices (F-4).

Smallest change: restate §3.2 as "concurrent Messages Web use causes
`BROWSER_INACTIVE`/`BROWSER_ACTIVE` flapping and repeated resyncs" and delete
the eviction sentence, or cite a non-code source. Rewrite OQ-2 accordingly —
see "Open questions answerable from source" below.

### F-12 (M) §13.3, OQ-10 — real phone numbers in a public repository

`+1<APPROVED_DIRECT_NUMBER>`, `+1<APPROVED_GROUP_NUMBER_1>` and `+1<APPROVED_GROUP_NUMBER_2>` are committed to a repo that
§1.4/D2 requires to be public. The spec's own §13.3 forbids putting a real
number in "a fixture, or an example, or a doc" — and then puts three in a doc.

Smallest change: move the approved list to an untracked
`testdata/live-numbers.local` (or `AGENT_GM_LIVE_NUMBERS`), and have §13.3 and
OQ-10 refer to it by name only.

### F-13 (M) §3.1, §6.3 — `TmpID` must be a bare UUID, not Agent GM's operation ID

The spec sets all three `TmpID` fields to "Agent GM's operation ID", which per
§4.1 is a prefixed opaque ID (`op_…`). Upstream generates them with
`util.GenerateTmpID()` = `uuid.NewString()`, carrying the comment
**"Matches what the native app does"** (`pkg/libgm/util/func.go:9-12`), and the
phone echoes the value back on the remote message
(`connector/handlegmessages.go:157,841`, `connector/backfill.go:66,158`).
Sending a non-UUID transaction ID diverges from every other client of this
protocol for no benefit.

Smallest change: `TmpID` is a fresh bare UUID minted per send attempt, stored
in an indexed `operations.tmp_id` column; correlation in §6.3 is
`tmp_id → operation_id`, not identity.

### F-14 (M) §7.5, §7.6, §8.2, §4.1 — reactions are a closed enum, not a free string

The spec exposes `emoji` as a free-form string on `POST
/v1/messages/{id}/reactions`, `DELETE /v1/messages/{id}/reactions/{emoji}`,
`add_reaction`/`remove_reaction`, and the `reactions[].emoji` read field, and
derives `react_` IDs as `UUIDv5(ns, message ID ‖ participant ID ‖ fully-qualified
emoji)` (§4.1:721). Google Messages does not work that way.

`gmproto.EmojiType` is a **closed 14-value enum**: eleven canonical reactions
with fixed code points (`emojitype.go:3-29`), plus `CUSTOM=8` (arbitrary
unicode in `ReactionData.Unicode`) and `EMOTIFY=13`. Three consequences the
spec must handle:

1. **Normalisation.** `UnicodeToEmojiType` accepts both `"❤"` and `"❤️"` as
   `RED_HEART` (`emojitype.go:53-54`), but `EmojiType.Unicode()` always renders
   `"❤️"` (`:25-26`). A caller who adds `❤` and then calls
   `DELETE …/reactions/❤` will miss, and the `react_` UUIDv5 for the same
   reaction differs between the write and the read. Agent GM must canonicalise
   through `MakeReactionData` → `EmojiType` → `Unicode()` **before** deriving
   the ID or matching the path segment, and §4.1 should say
   "canonical `EmojiType.Unicode()`", not "fully-qualified emoji".
2. **`EMOTIFY` has no emoji.** Upstream renders it as the literal `:custom:`
   (`connector/handlegmessages.go:542-543`), and any other unrecognised type
   with an empty `Unicode()` is **silently skipped**
   (`handlegmessages.go:546-548`). `reactions[].emoji` needs a defined value
   for this case — `null` plus a `type` field is the honest shape.
3. **The eleven canonical reactions are what Google's own picker offers.** The
   spec should say that anything else becomes `CUSTOM` and may not render on
   the recipient's phone, rather than implying any emoji works.

Smallest change: add the `EmojiType` table to §3.1, add a `type` field beside
`emoji` in the reaction read shape, and make §4.1's `react_` derivation use the
canonical unicode.

### F-15 (m) devbox.json — `AGENT_GM_HOME` is not a spec variable

`devbox.json` exports `AGENT_GM_HOME=$PWD/data`. The spec uses only
`AGENT_GM_DATA_DIR` (§3.3:482, §4.2:756, §14.1:2774, §15.1:2861). A developer
shell therefore does not point the binary at `./data`. Rename it.

### F-15 (m) §3.1 — `AuthNetwork()` returns `""` for QR, not `"Bugle"`

Spec: QR pairing "uses the `Bugle` network (`util.QRNetwork`)".
`AuthData.AuthNetwork()` (`client.go:96-101`) returns `util.GoogleNetwork` for
gaia and **empty string** otherwise; `util.QRNetwork` appears only in the two
pairing HTTP payloads (`pair.go:93,115`). Cosmetic, but it is in a table the
spec declares to be the contract.

### F-16 (m) §3.4 — `PhoneNotResponding` has two trigger paths

Emitted on the **first** unanswered ping, or after `alertTimeoutCount` (default
**4**, `client.go:184`) missed pings in a row — `longpoll.go:134-147,196-212`.
The one-line "phone unreachable" hides the second path, and §15.4's runbook
timing depends on it.

### F-17 (m) §3.1 — `FAILURE_4` wording

The spec quotes an upstream *comment* "not default sms app?". At this pin it is
a user-facing string: `"Google Messages is not your default SMS app"`
(`pkg/connector/errors.go:41-42`). Cite that instead; §3.7's
`not_default_sms_app` mapping is otherwise correct.

---

## Carried-over conventions from Agent MX

Faithful, verified both sides: strict parameter validation; error envelope and
`WWW-Authenticate`; MCP `isError` / `structuredContent` semantics and the
"JSON-RPC error only for transport-level faults" rationale; scopes and the
`admin`-never-enrollable rule; idempotency (required, caller+kind+key scoped,
`idempotency_conflict`); OAuth 2.1 (PKCE S256, RFC 7591 DCR, RFC 8707 resource
indicators, RFC 9207 `iss`, RFC 9728 + 8414 metadata, identical token
lifetimes, two-step enrollment approval, the `localhost` deviation recorded as
D12); media tickets (typed audiences, single-use upload, 15 min / 5 redemption
download, 2 h reservation, URL from `AGENT_GM_PUBLIC_URL`); CLI exit codes.
The cold-agent `instructions` standard is carried and **tightened** (byte
identity with `docs/mcp.md`, asserted by a test).

### F-18 (m) Five changes are unrecorded in §18.1/§18.3

Each needs one row, no design change:

1. `conv_` is reinstated as the conversation prefix; Agent MX rooms-v1
   explicitly retired it in favour of `room_`.
2. Dropped error codes `authentication_required` (folded into `invalid_token`),
   `operation_failed`, and `history_incomplete`-as-a-code (survives as a
   backfill warning).
3. `GET /v1/events` (SSE), its replay ring and cursor semantics are gone
   entirely. §18.3 records dropping sync tokens but not the stream.
4. **Client ID Metadata Documents** and the three-tier client resolution
   (preregistered → CIMD → DCR) with its SSRF-safe fetcher are gone; §9 is
   DCR-only. Either record the drop or restore it.
5. `--wait-for` changed default (`accepted` → `sent`) and value set
   (`accepted` dropped, `read` added).

### F-19 (m) §10.3 — download ticket misses the re-check sentence

Agent MX states that **both** ticket redemptions re-check the issuing
authorization's scope and revocation state; §10.3 states it for upload only
(spec :2146-2148). Append the same clause to the download row.

### F-20 (m) §18.1 D6 overstates itself

D6 says `account_key` is in *every* derived ID; the §4.1 table derives `att_`,
`react_` and `part_` from parent IDs. Transitively true, literally not. Reword
to "every root derivation".

### F-21 — no Matrix leakage

Grepped for `matrix|mxid|MSC\d|portal|ghost|puppet|appservice|intent|room_id|
event_id|m\.room|homeserver|federat|space|invitation|provider|outbox|
decryption`. Every hit is a non-goal negation (§1.3), a decisions/history row
(§18.3, which the name lint explicitly carves out), an upstream source citation
(`connector/handlematrix.go` etc.), the name lint itself (§13.5), or a
different word sense ("exit-code matrix", log "redaction",
`decryption_key` = libgm's media AES-GCM key). The §13.5 lint is real and
enforced as a Slice 3 acceptance item.

---

## Internal consistency

Cross-referenced every §7 route against §11.3 (CLI) and §8.2 (MCP), every MCP
tool against §9.7, every promised response field against the §4.2 schema, and
every §16 acceptance test against §13.1 (fake) and §13.3 (live gates). Spot-checked
the highest-severity items against the file myself; line numbers are into
`plans/AGENT_GM_SPEC.md`.

### F-22 (C) Slice 2 has no credential that can call its own routes

§9.7:2058 gives `admin` exactly `/v1/admin/*` and pairing "**and nothing
else**". Slice 2's only auth mechanism is `POST /v1/auth/admin-session`
(:2045, :3044). So in Slice 2 nothing can hold `messages:read`/`:write`/
`:delete`, and therefore **nothing can call any route that slice-2 tests 11–20
exercise**. Test 22 (:3112, "three scopes do not multiply the allowance") and
test 18's exit-code matrix (:3101, needs exits 3 and 4) are unreachable for the
same reason. Smallest change: say explicitly that the admin bootstrap session
may mint messaging scopes for the owner, or move the messaging routes' tests
into Slice 3.

### F-23 (C) Undefined DTOs and columns behind promised reads

- **No `operation` object is defined anywhere.** `{operation, message_id}` and
  `operation: null` are returned by four routes (:1479, :1482, :1483) and one
  tool (:1588); §6.4 gives statuses only, never a field list.
- **`messages` has no `updated_at_ms`** (:823-843, verified) yet
  `delivery.updated_at` is served (:1460) and slice-2 test 2 asserts on it
  (:3055).
- **No `backfill_state` table** (:4.2) though §5.2 step 6 (:1083) resumes from
  it after restart.
- **No redemption counter**, but download tickets promise `max_redemptions: 5`
  (:2110, :2192) while §12.1:2442 says tickets are "signed, not stored". A
  stateless token cannot enforce a 5-use cap; slice-2 test 15 (:3093) is
  unimplementable as written.
- `session.paired_at` / `session.last_event_at` (:1402) are in no `server_meta`
  key (:766-768); `peer_typing_until` (:552) is in no DTO (:1439-1450);
  `coverage` on `/v1/search/messages` (:1426) is never defined; `media_cache`
  LRU excludes "unpinned" entries (:2194) with no pin column (:925-930).

### F-24 (C) `gm.Backend` cannot serve what the API promises

The §2.3 interface (:189-223) has no `FetchConfig`, `IsBugleDefault`,
`CurrentSessionID`, `ListTopContacts`, `GetConversationType`,
`UpdateConversation`, `SetTyping` or `GetFullSizeImage`. But §7.4 health needs
the first three (:1401, :655-665), `POST /v1/conversations/{id}/typing`
(:1480) has **no backend method at all**, `contacts.list?top=true` (:1427)
needs `ListTopContacts`, and the `media_pending` path (:2124) needs
`GetFullSizeImage`. §2.2 forbids anything above `gm` from touching `libgm` and
§13.1's fake implements only `Backend`, so none of it is producible or
testable. Also `ResolveConversation` (:207) returns no status, yet slice-2 test
10 (:3076) requires distinguishing `GetOrCreateConversation` status 1 / 3 / 4.

### F-25 (M) Google Messages capabilities that are read but never writable

`conversations.folder`, `.pinned` and `.unread` are stored (:778-780),
filtered (:1419) and served (:1440), and `UpdateConversation` is declared
in-contract for "archive/unarchive/mark-unread/pin" (:343) — but **no route,
tool or CLI command archives, unarchives, pins, unpins or marks unread**.
Either add them or say in §7.7 why they are read-only.

### F-26 (M) §16 acceptance tests that cannot run where they are placed

- Slice 1 test 4 (:3017) maps §3.5 errors onto §7.2 codes, but §7 REST is
  "explicitly not in this slice" (:3006).
- Slice 1 test 10 (:3032) needs a **live** `ConfigVersion` and
  `is_default_sms_app` — a real round trip — but is not marked a live gate
  (unlike 5–9), and per F-24 the fake cannot produce either.
- Slice 2 test 15 (:3093) revokes an authorization between redemptions;
  authorizations and revocation arrive in Slice 3 (:3043).
- Slice 2 test 20 (:3105) compares against a REST **`effect` field** that
  exists in no DTO (`effect` is a table column caption at :1490).
- Slice 2 test 8 (:3070) waits out a 24 h `pending_timeout` with no fake clock
  specified in §13.1.
- Slice 3 test 21 (:3211) needs claude.ai/ChatGPT to reach a deployed
  container, but the Dockerfile and deployment are Slice 4 (:3220) — while
  :2986 says `main` advances only when each slice's gate passes.
- Slice 3 test 19 runs `devbox run conformance` and §12.1:2444 runs
  `devbox run gen-secret`; **neither script exists in `devbox.json`**, which
  §13.6:2746 claims is exhaustive. `devbox run test-live` also lacks the
  `-tags live` that §13.3:2633 requires.
- Slice 4 test 4 (:3232) installs on darwin-x64 and darwin-arm64 with no macOS
  runner anywhere in §13, and is not named a live gate.

### F-27 (M) Routes, tools and scopes that do not line up

- `whoami` and `logout` are granted by `messages:read` in §9.7:2055 and exist
  as CLI commands (:2294-2295), but as **no route and no tool**.
- `POST /v1/auth/admin-session` (:2045, :3044) is the only Slice 2 credential
  and appears in **no route table**; `/v1/auth/refresh` (:2047) is referenced
  only negatively and never defined.
- `agm session --watch` "streams state changes" (:2265) with no streaming
  route; `agm operations wait` (:2291) with no wait route, though the *server*
  setting `operations.wait_timeout` exists (:2886); `agm admin settings get`
  (:2297) where §7 has only list and `PATCH` (:1502).
- No MCP tool and no stated exclusion for `GET /v1/messages/{id}/attachments`
  (:1425), `GET`/`DELETE /v1/uploads/{id}` (:1485), or `GET /v1/health`
  (:1401) — `get_session` covers pairing state only, so `backfill`,
  `is_default_sms_app` and `config_version_stale` are MCP-invisible even though
  §15.4 makes them the primary diagnostic. Only `typing` gets an explicit
  reason (:1597).
- `get_operation` is filed under Writes/`messages:write` (:1588), in the
  `readOnlyHint: true` row (:1630), and in the §7.5 **reads** table (:1430).
  Three placements of one route.
- `create_upload` is `openWorldHint: true` (:1631) though a reservation is
  purely local, contradicting the rationale at :1635.

### F-28 (M) Contradictions between sections

| Where | Contradiction |
|---|---|
| :565 vs :2673 | §3.4 "the full enum has **27** values"; §13.4 asserts "`AlertType` still has **28** values". **28 is correct** — I counted 28 `AlertType` constants in `pkg/libgm/gmproto/events.pb.go`. Fix §3.4. |
| :1199-1200 | worst-case send latency "roughly 4 × 60s + 31s" (= 271 s) but "the HTTP handler enforces a **240**-second deadline" — the final retry can never complete |
| :2171 vs :1479, :1582, :1730, :2279 | "one attachment per message" vs `upload_ids` being an array on REST, MCP and the instructions block, and `--file` being repeatable |
| :994 vs :1163, :2566, :3016 | `sending` is defined in §4.4 and promised in §8.3 but appears in **no** walk, no fake script and no test. Verified: the word occurs once in the whole spec |
| :712-714 vs :1459, :1367 | §4.1 forbids raw Google values on public surfaces; `delivery.state_raw`, `details.google_type` and `details.status` are exactly that |
| :1261 vs :2315 | "terminal" means `succeeded\|failed\|unknown` for an operation but additionally `delivered` for `--wait-for` — yet `delivered → read` is legal (:1010) |
| :1352 vs :1516, :1213 | `not_paired` is both a top-level 409 code and an `unsupported_capability` reason; an unpaired send has two answers |
| :2666 vs :687, :1004 | §13.4 #3 requires every `MessageStatusType` mapped "exactly once, no value unmapped"; §3.7 carves out tombstones and §4.4's `unknown` row absorbs "unmapped values" |
| :985 vs :2110, :2146 | "millisecond precision" on every JSON surface vs media examples with none |
| :2190 vs :2110, :2146 | download life 15 min / upload life 2 h vs example expiries of +30 min / +2 h 30 min against one sample clock |
| :720 vs :2102 | `att_` is a UUIDv5 in §4.1 but rendered `att_01k4…`, the v7/ULID shape of `upl_` |
| :1821-1826 | "every OAuth error body is `{error, error_description}`" and, three lines later, unknown `/oauth` paths use the REST `not_found` envelope |
| :1272-1276 | §6.4's transitions omit `pending → failed`, so a `pending` operation whose echo reports failure has nowhere legal to go |
| :1233 vs :1483, :2233 | idempotency key arrives by header, by body field, and by `?client_request_id=` query param; only two are documented |
| :2242-2253 | no exit code for `idempotency_conflict` (409), `payload_too_large` (413) or `media_unsupported_type` (415), though slice-2 test 18 wants "every code" |
| :2868 vs :2581, `implementer.md:31` | `AGENT_GM_ALLOW_FAKE` is required to start with `AGENT_GM_BACKEND=fake` but is in no config table and absent from both §13.1 and the implementer agent's instructions |
| :2870 vs :2402 | `AGENT_GM_URL` is declared and used nowhere; §11.5's precedence table omits it |
| `CONTRIBUTING.md:25` | cites "spec §14" for Devbox; §14 is Packaging |
| `CONTRIBUTING.md:29` | points at `docs/security.md`, in no `docs/README.md` row and no slice |
| `docs/README.md:22` | assigns `upstream-pin.md` to Slice 1; §16 Slice 1 deliverables (:2999) do not include it |
| `README.md:15` | states the CLI **is** published as `@agent-gm/cli`, which OQ-6 (:3338) records as undecided |

### F-29 (M) The §13.5 name lint would fail this spec

The lint (:2722-2729) reads "every markdown page under `docs/` and `plans/`"
and bans `room`, `portal`, `event_id`, `provider`, `redact`, `generation`,
`outbox`, and "a `mode` argument", exempting only §1.3 and §18 as history.
`outbox` is a §6.1 heading (:1186), `mode` is a §7.6 column (:1495), and
`provider` is a §2.2 package rule (:173) — all outside the exemption. Either
narrow the lint to the served catalogue plus `docs/`, or widen the carve-out.
`send_mode` (:777, :1440, :1479) also collides with the banned `mode`.

### F-30 (m) Indexes missing for filters the API advertises

`conversations?query=` is defined as a substring match over the thread name
**and every participant's name and number** (:1419, :1708) with no index on
`conversations.name` or `participants.display_name` — a full scan plus join on
the busiest MCP entry point. Also unindexed: `contacts.display_name` (:1427);
`conversation_type`, `unread`, `is_group`, `deleted_at_ms` (:1419);
`messages.delivery_state` and `messages.kind` on the account-wide
`GET /v1/messages` (:1421); `operations(authorization_id)` for the
caller-scoped `GET /v1/operations/{id}` (:1430); and
`audit(authorization_id)` (:1505). `search?syntax=literal` is the **default**
(:1426) and `messages_fts` serves fts5 only, so the default search path has no
index at all.

### F-31 (m) Reaction uniqueness contradicts SWITCH, and `react_` is dead

`UNIQUE (message_id, participant_id, emoji)` (:881) permits several emoji per
person per message; §7.6:1482 turns an add into `SWITCH` "if the owner already
has a different reaction on that message", i.e. one per person, which needs
`UNIQUE (message_id, participant_id)`. Separately, `react_` IDs are minted
(:721) and served (:1466) but accepted by **no** route or tool — removal is
keyed by emoji in the path (:1483, :1586). See also F-14.

### F-32 (m) Two sources of truth for cached media

`attachments.cache_path`/`.size_bytes` (:863, :867) duplicate
`media_cache_entries.relative_path`/`.size_bytes` (:927-928). §10.3:2210 says
the eviction sweep finds files "through those rows" without naming the
authority, and slice-2 test 17 (:3098) asserts "no orphan row", singular.

### F-33 (m) CLI cannot reach several route parameters

`include_deleted` (:1419), `delivery_state` and `include_tombstones` (:1421),
`force_rcs` (:1479), and search's `sender`/`after`/`before`/`has_attachment`
(:1426) have no CLI flag (:2268-2279) — yet slice-2 test 11 (:3079) demands a
route-by-route strict-rejection test. `GET /v1/messages` (account-wide) is
CLI-unreachable at all, since `agm messages list` requires a conv ID (:2275).
`DELETE /v1/pairing/{id}` (:1405) and `POST /v1/session/reconnect` (:1407)
likewise have no command.

---

## Rubric — "every name means what it means in Google Messages"

**Score: 1 / 2.**

The vocabulary is disciplined and genuinely Matrix-free (F-21), and the
`delivery_state` mapping is numerically correct against `MessageStatusType` at
the pin. It fails on two counts: raw `gmproto` and SQLite internals reach the
public surface, and several names assert Google behaviours that do not exist.

**Below 2 because of these three, each on a served surface:**

| Name | Where | Why it fails | Smallest change |
|---|---|---|---|
| `send_mode: auto \| xms \| xms_latch` | column :777, **public Conversation DTO :1440**, error condition :1479 | `xms`/`xms_latch` are raw `gmproto` internals. They exist nowhere in Google Messages' UI and are opaque to the cold agent §8.3 addresses | drop `send_mode` from the DTO; express `force_rcs` eligibility through `capabilities` (:1447) |
| `tombstone` — `messages.kind`, `include_tombstones` filter, MCP argument | :827, :1421, :1570, :687 | Google Messages has no "tombstone"; the UI shows in-thread system text. And `m.room.tombstone` is **Matrix** vocabulary — exactly what §13.5's lint exists to ban, and the ban list (:2726) misses it | `kind='system'` / `include_system`; add `tombstone` to the lint |
| `syntax: "fts5"` | :1426, :1573 | a SQLite extension name as a public JSON Schema enum value. Google Messages search has no "syntax" | `syntax: words` (or drop it and always FTS with a literal fallback) |

**Also failing, from the library pass:** `events.BrowserActive` → "another
device has taken over" (§3.4, F-2 — Google reports session activity, not
takeover, and the event never fires); `pairing_multiple_devices` (§3.5, F-4 —
Google never reports it); "paired-device slot" (§3.2, OQ-2, F-11 — not a Google
Messages concept); `config_version_stale` (§3.7, F-1 — an Agent GM diagnosis
named as Google's answer).

**Weaker but worth fixing while the names are cheap:** `folder` /
`inbox` (:778, Google's UI says All / Archived / Spam & blocked, and `inbox` is
email vocabulary); `delivery.state_raw` and `details.google_type` /
`details.status` (:1459, :1367 — raw Google integers on a public surface, which
§4.1:712 explicitly forbids); `agm messages unreact` (:2283 — disagrees with
MCP `remove_reaction` and REST `DELETE …/reactions/{emoji}`, and §11.3:2321
makes same-words-on-all-three a contract); `agm conversations read` (:2272 —
reads as "read the conversation", means "mark it read"; MCP and REST both get
it right); `type: sms|rcs` (:776 — makes MMS unnameable although §8.3:1703
tells the agent about "SMS/MMS"); `participants` on reads vs `recipients` on
writes (:795, :1442 vs :1478 — two names, one noun); `queued` and `canceled`
delivery states (:993, :999 — Google's UI says *Sending…*, and offers no
cancel); `part_` missing from §8.3's "every ID here" list (:1695 vs :1442).

---

## Pairing UX assessment (§11.4 and wherever pairing appears)

**What the gaia flow actually needs** (F-6): `SID`, `HSID`, `OSID`, `SSID`,
`APISID`, `SAPISID`, optionally `__Secure-1PSIDTS`. All of these are
`httpOnly`, so `document.cookie` and any injected-JS approach cannot read them —
that rules out anything short of the browser's own cookie store. `OSID` is
host-scoped to `messages.google.com`, so a read of `.google.com` alone silently
returns an unusable set. And per F-7 these are retained for the session's whole
life, not consumed at pairing.

**(A) `agm pair --google --chrome` — launch Chrome with a dedicated profile and
a CDP port. Feasible, and the right gaia path.** Required shape:
`chrome --user-data-dir=<dedicated> --remote-debugging-port=<random>` bound to
`127.0.0.1`. The dedicated `--user-data-dir` is **mandatory, not stylistic**:
current Chrome refuses `--remote-debugging-port` against the default profile
directory. Nothing else is an "automation flag" — no `--enable-automation`, no
`navigator.webdriver`, no infobar, so Google's sign-in sees an ordinary Chrome.
The CLI then calls `Storage.getCookies` / `Network.getCookies` (both return
`httpOnly` cookies, unlike JS) for `https://messages.google.com` **and**
`https://www.google.com`.

Four things the spec must say if it adopts (A):

1. Navigate to `https://messages.google.com/web/config` and **wait for `OSID`
   to exist** before reading; it is not set by the accounts.google.com sign-in
   alone. Read `__Secure-1PSIDTS` last, since it rotates.
2. The debugging port is a live credential channel — anything that can connect
   reads every cookie in the profile. Bind loopback, random port, and terminate
   Chrome as soon as the cookies are captured.
3. A brand-new empty profile is a *new device* to Google and may trigger 2FA or
   device verification. That is a one-time UX cost, not a blocker, but the CLI
   must not treat the extra challenge as a failure.
4. The captured cookies go straight into `AuthData` and then into
   `session.enc`; the CLI must never write them to a file of its own.

**(B) reading the user's existing Chrome profile via a cookie-store library —
reject.** On Linux the cookie DB key lives in the login keyring (already a
known-fragile area on this host); on Windows Chrome has used app-bound
encryption since 127; and the DB may be locked while Chrome runs. Platform-forked
and brittle, for no gain over (A).

**(C) QR only — keep as the primary.** It needs no cookies, no browser and no
Google account access at all; §11.4 already renders it CLI-only; `libgm` owns
the token refresh; and F-5 means the gaia flow's expiry story is less dire than
the spec feared, so gaia's remaining advantage is small. Recommend: **(C)
primary, (A) as the gaia path, (D) documented fallback, (B) never.**

**(D) `--cookies-file` / curl paste — fallback only.** It already exists in
§3.2 and should stay, explicitly demoted, since it is the only path that works
on a machine with no Chrome at all.

**Headless case.** `agm pair --google --chrome --server https://gm.agent-wx.app`
is the right shape: Chrome runs on the laptop, the CLI POSTs only the six
cookies to the server over TLS. The spec must add that the server then **holds
those cookies for the life of the session** (F-7), so the trust decision the
owner is making is "this server holds my Google account cookies", not "this
server saw them once". That sentence belongs in §11.4, §12.1 and
`docs/pairing.md`.

---

## Open questions answerable from source

- **OQ-1 (which pairing flow is primary).** The premise is wrong: cookie
  refresh without a re-pair **is** supported (F-5,
  `pkg/connector/login.go:246-289`). Recommendation stands anyway — **QR
  primary**, gaia via (A), because QR needs no Google account credentials at
  all and `session.enc` then holds only a tachyon token rather than live
  account cookies (F-7).
- **OQ-2 (give up Google Messages Web).** **No.** Nothing at `be48a58` shows
  eviction or a device-slot limit (F-11). Concurrent use produces
  `BROWSER_INACTIVE`/`BROWSER_ACTIVE` alerts and extra resyncs
  (`pkg/connector/handlegmessages.go:298-340`), which cost bandwidth and
  backfill churn, not the pairing. The question should be re-asked as "is the
  owner willing to tolerate resync churn", which is a much cheaper yes.
- **OQ-8 (group-messaging MMS setting).** Still an owner question — but the
  spec's stronger claim, "a phone set the other way cannot create a group at
  all", is not visible in the pinned tree; `CreateGroup`
  (`pkg/connector/startchat.go:181-233`) fails only on `< 2` participants or a
  missing conversation body. Soften it or source it.
- **OQ-10 (approved numbers).** Independent of the owner's answer, the numbers
  must leave the public repo — F-12.
- **OQ-5 (who approves an OAuth authorization request).** Not decidable from
  source, but note it is currently moot: per F-22 there is no path by which a
  non-admin authorization can hold a messaging scope at all. Answer F-22 first.
- **OQ-6 (npm scope).** Not answerable from source, but `README.md:15` already
  states `@agent-gm/cli` as settled fact — fix that either way (F-28).
- Not answerable from source, correctly left to the owner: OQ-3, OQ-4, OQ-7,
  OQ-9.

---

## Verified against the pin — no change needed

`ListConversations` sends `BUGLE_ANNOTATION` on the first call per process and
`BUGLE_MESSAGE` after (`methods.go:9-20`, `client.go:141`) · `TmpID`,
`TmpID2` and the request `TmpID` all equal (`connector/handlematrix.go`) ·
`ErrPhoneNotResponding` at `responseHardTimeout = 60s`, with the "server
already accepted it" semantics (`session_handler.go:20-32,231`) ·
`FAILURE_2`/`FAILURE_3` retried on `[3s, 8s, 20s]`
(`connector/handlematrix.go:112-117`) · timestamps in microseconds
(`connector/backfill.go:126`, `chatsync.go:51`, `client.go:440-445`) ·
`CREATE_RCS` retried once with `CreateRCSGroup=true` and `RCSGroupName`
defaulted to `""` (`connector/startchat.go:214-219`) · `AuthData` field list
and JSON tags verbatim (`client.go:28-49`) · `RefreshTachyonBuffer` 1 h
(`client.go:105`) · `recentUpdates [8]` (`client.go:138`) ·
`DefaultBugleDefaultCheckInterval` 2 h 55 m (`client.go:115`) ·
`GaiaInitTimeout` 20 s (`pair_google.go:288`) · `util.ConfigMessage` =
2026/9/2 V1=4 V2=6 (`util/config.go:7-13`) · `isFatalRefreshError`
(`client.go:477-489`) · `completePairing` sleeps 2 s before reconnecting
(`pair.go:64-73`) · `PairCallback` suppresses `PairSuccessful`
(`client.go:145`, `pair.go:62`) · `DoGaiaPairing` reconnects in a goroutine
(`pair_google.go:311-318`) · `FinishGaiaPairing` returns
`"<mobile sourceID>/<destRegDevice int>"` and both key-derivation versions
(`pair_google.go:446-472`) · `UploadMedia`/`DownloadMedia` take no `context`,
`Size` is the plaintext length (`media.go:91-121,265`) · `MimeToMediaType`
with the prefix fallback (`media.go:27,92-94`) · old conversation events and
old user alerts dropped by the library, old messages delivered with
`IsOld=true` (`event_handler.go:249-251,262-267,272-280`) ·
`hackyLoggedOutBytes = {0x72, 0x00}` → `GaiaLoggedOut`
(`event_handler.go:226-232`) · `*gmproto.RevokePairData` triggered directly
(`pair.go:50-51`) · `ClientReady`, `PingFailed{Error,ErrorCount}`,
`AccountChange{…,IsFake}` shapes (`events/ready.go:11-14,107-110`,
`events/ready.go:22-25`) · every `MessageStatusType` number cited in §4.4.

---

# Re-review — spec at `b485ce4` (4243 lines, 11 commits)

Method: rebased onto the rewritten `origin/main`, re-read §§2-3, 8-13, 15-18,
and re-verified **every** new `path:line` citation in §3 against
`/home/nick/code/mautrix-gmessages` at `be48a58`.

## Verdict

**Accept with required changes.** All 33 findings were engaged; 28 are fully
closed, 4 partially, 1 has a residual. The §3 rewrite is the strongest part of
the change: I checked roughly forty new citations and all but two land exactly
on the symbol claimed, and several fixes are better than what I asked for —
`config_version_stale` is now correctly framed as Agent GM's own diagnosis with
the ConfigVersion diff as the whole detection rule, and unnamed
`GetOrCreateConversation` statuses became `google_undocumented_status` carrying
the bare integer rather than a name the spec made up.

The required changes are the ones the **new** D25 privilege concentration
demands (R-1 below), plus one false universal claim that a named Slice 3 test
asserts (R-2). Neither blocks Slice 1.

## Section 3 — re-verified against `be48a58`

| Finding | Verdict | Spec line | Library check |
|---|---|---|---|
| F-1 status 4 | **closed** | :817-834 | claim withdrawn; `gmproto/client.pb.go:181-183` enum ✓, `startchat.go:233,238` ✓ |
| F-2 `BrowserActive` | **closed** | :665-668, :685 | named as never-emitted; `BROWSER_ACTIVE(2)` row matches `handlegmessages.go:301-327` ✓ |
| F-3 `events.QR` | **closed** | :467-470 | zero emitters ✓ |
| F-4 gaia device pick | **closed** | :322, :505-518 | `client.go:143` = `GaiaHackyDeviceSwitcher int` ✓; `pair_google.go:364` = `primaryDevices[…%len]` ✓; `:385-397` wrap ✓; `--device-index` added |
| F-5 cookie refresh | **closed** | :553-566 | `login.go:246-289`, `:249`, `:268`, `:289` all ✓; `--refresh-cookies` + `pairing_wrong_account` |
| F-6 cookie set | **closed** | :476-497 | all seven + domains match `login.go:200-210` ✓; OSID host-scoping called out |
| F-7 cookies retained | **closed** | :540-551 | five call sites ✓; blast radius stated in §3.2, §3.3, §11.4, §12.1 |
| F-8 401/403 | **closed** | :641-651 | `longpoll.go:539-547` ✓; `errors.As`, "never on the string" |
| F-9 dedup loses batches | **closed** | :698-710, D26 :4138, §5.4 sweep | `event_handler.go:263-266,272-275` ✓; `core.reconcile(since)`, 15 m interval, three triggers, health fields |
| F-10 status ranges | **closed** | :836-846 | 1-27 / 100-118 / 200-279 / 300 ✓ (`OUTGOING_FAILED_TO_ENCRYPT=27` is the max) |
| F-11 device slot | **closed** | :568-579 | eviction claim deleted |
| F-12 phone numbers | **closed** | :3315-3326, :3597 | scrubbed from the tree **and from history** (`git log --all -S` finds nothing); `.gitignore` + `no-real-numbers` CI job |
| F-13 `TmpID` | **closed** | :400-410 | `util/func.go:9-12` ✓; four echo cites ✓; `operations.tmp_id` |
| F-14 reaction enum | **closed** | :855-897 | `conversations.proto:70-85` ✓; `emojitype.go:25-26,53-54` ✓; `{"emoji": null, "type": "emotify"}` |
| F-15 devbox | **closed** | `devbox.json` | `AGENT_GM_DATA_DIR`; `-tags live`; `conformance`, `gen-secret`, `lint-names`, `no-real-numbers`; `openssl` |
| F-16 `PhoneNotResponding` | **closed** | :653-656 | `longpoll.go:72-78` is exactly `!firstPingDone \|\| timeouts >= alertTimeoutCount` ✓ |
| F-17 `FAILURE_4` | **closed** | :373 | `connector/errors.go:41-42` ✓ |

Two citation drifts, both harmless to the meaning but wrong as addresses:
`event_handler.go:186-200` is cited for `deduplicateUpdate`, which lives at
`:168-181` (`:186-200` is `HandleRPCMsg`); `events/ready.go:75-83` is cited for
`RequestError.Is`, which lives at `:69-77`. §3 declares its citations to be the
contract, so fix the addresses.

## The rest

F-18, F-19, F-20, F-21, F-22, F-24, F-25, F-26, F-29, F-30, F-31, F-32:
**closed**, each with a located line. Highlights: D25 gives the admin session
messaging scopes so Slice 2 can test itself; §2.3 gained `SessionID`,
`FetchConfig`, `IsDefaultSMSApp`, `GetConversationType`, `ListTopContacts`,
`SetTyping`, `UpdateConversation` and a `ResolveResult` carrying the raw
status; `PATCH /v1/conversations/{id}` makes folder/pin/unread writable;
`download_tickets` became a table with `redemptions`/`max_redemptions` (D24)
because a signed blob cannot count; reaction uniqueness is now
`(message_id, participant_id)` (D23) and `react_` is addressable; the lint
exempts `plans/` in full so it no longer fails its own spec.

**F-23 — partial.** Eight of nine closed. Still open: the conversation DTO
(:1852-1865) has no deleted flag, though `include_deleted` is a filter on both
`GET /v1/conversations` (:1814) and `list_conversations` (:2017). `messages`
got `is_deleted` (:1893); conversations did not.

**F-27 — partial, and this is R-2.** :2005-2011 names three deliberately
excluded routes and then asserts "**Every other `/v1` route has a tool**". That
is false for `/v1/auth/whoami` and `/v1/auth/logout` (:1767-1768, both
`messages:read`, so no admin argument excuses them), `/v1/auth/admin-session`,
`/v1/auth/refresh`, `/v1/session/events`, `/v1/session/reconnect`,
`/v1/session/unpair`, all four `/v1/pairing/*`, and every `/v1/admin/*` route.
Slice 3 test 16 (:3991) is a two-way table test over exactly these two
inventories, so it fails as written. Also **:2001 says "Nineteen tools" and the
three tables list 20** (10 + 8 + 2), which is what Slice 3 test 14 (:3982)
asserts.

**F-28 — 18 of 21 rows closed.** Not closed: the millisecond-precision rule was
*strengthened* to name `expires_at` explicitly (:1225-1226) while the two
examples still read `"2026-09-06T10:11:07Z"` and `"…T12:11:07Z"` (:2599,
:2635) — the contradiction is now sharper than before. And `att_` is still a
UUIDv5 in §4.1 (:913) but rendered `att_01k4…` five times in §10.1, the ULID
shape of `upl_`. Partial: `AGENT_GM_ALLOW_FAKE` reached the config table
(:3596) but §13.1 (:3257) and `.claude/agents/implementer.md:30-31` still say
`AGENT_GM_BACKEND=fake` alone — an implementer following either will find the
server refuses to start.

**F-33 — closed** (:2838-2845, "every `/v1` route parameter has a flag"), with
one unstated exception: `GET`/`DELETE /v1/uploads/{id}` are excluded from MCP
explicitly but not from the CLI.

## D25 — does the admin session reopen a hole? (R-1, required)

D25's reasoning is sound and its blast radius is stated honestly. What it
changes is that `POST /v1/auth/admin-session` is now the single most valuable
endpoint in the system: one successful secret guess yields `admin` **plus** all
three messaging scopes, and via `/v1/admin/diagnostics` the raw Google view.

Already adequate: ≥43-character secret enforced by the server (:3085);
environment-only, excluded from process arguments, logs and audit payloads
(:3098); a failure budget of 5 / 15 min per source and 20 / 15 min globally
with exponential cooldown (§12.3); rotating the secret revokes every prior
admin authorization on next start (:3085); the two credential paths never cross
and neither crossing attempt revokes anything (:2516-2520); `admin` still never
enrollable; token hashes only, never values (:2500). `session.enc` remains
unreachable from any API (:3.3), so an admin session cannot exfiltrate the
Google cookies of F-7.

Five gaps the new concentration requires closing before Slice 2:

1. **No constant-time comparison is specified anywhere.** `grep -in
   "constant.time\|subtle"` over the spec returns nothing. This is now the
   highest-value check in the system. §12.1 should require
   `subtle.ConstantTimeCompare`, with a test.
2. **No TTL for admin-session tokens.** §9.6's table is entirely `oauth.*`
   settings, and §9.6 itself says the admin bootstrap "is a separate credential
   path". So the strongest credential has no stated access-token TTL and no
   refresh idle/absolute TTL. Add `admin.access_token_ttl` and
   `admin.refresh_token_{idle,absolute}_ttl` to §15.1 with the OAuth defaults.
3. **`/v1/auth/refresh` sits outside §9.8.** §9.8 budgets unauthenticated
   endpoints and names only `/oauth/revoke` and the `refresh_token` grant,
   including a durable 30-invalid-tokens / 15-min limit. `/v1/auth/refresh`
   (:1766) accepts a bearer value from an unauthenticated caller in exactly the
   same shape and gets only the generic per-source limit. Extend §9.8 to it by
   name.
4. **Admin-session mint and failure are not audited.** §12.4 (:3190-3196) audits
   enrollment, authorization requests, revocations and refresh-token reuse — but
   not the mint of the credential that outranks all of them. Add
   `auth.admin_session_minted` and `auth.admin_secret_failed` (source, outcome
   and granted scopes; never the secret).
5. **Narrowing may be undone by a refresh.** §9.6 says an OAuth refresh's
   `scope` "may only narrow"; §7.4's `/v1/auth/refresh` row (:1766) says only
   "rotates it". State that an admin refresh may not widen beyond the scopes the
   session was minted with — otherwise the narrowing that Slice 2's exit-3 and
   exit-4 tests depend on is one refresh away from undone.

## Rubric — re-score

**2 / 2.** Every name I named is fixed, and fixed at the root rather than
renamed: `send_mode_raw` is internal and never served, derived into
`capabilities` (:968, :1304); `tombstone` → `kind="system"` / `include_system`
and added to the lint's ban list (:3434); `fts5` is gone and search `mode` is
`words|exact`; `delivery_state_raw` is admin-only, surfaced solely by
`/v1/admin/diagnostics` (:1031, :1942); `queued` collapsed into `sending`
because Google's UI says *Sending…* (:1235) and there is no queue (:1569);
`inbox` → `active`; `type` → `sms_mms|rcs`, so MMS is nameable; `unreact` gone,
`mark-read`/`add-reaction`/`remove-reaction` identical across CLI, MCP and REST
(:2852-2854); `participants` and `recipients` explicitly disambiguated
(:1873-1876); `part_` added to the instructions block's ID list (:2153).

## Smaller residuals

- §13.6 (:3473) still claims "there is no command in this repository that is not
  a `devbox run <script>`", but `pin-consistency`, `fixture-validation`,
  `build-matrix` and `image` have no script — the same defect that was fixed for
  `conformance` and `gen-secret`.
- §13.5 says the lint reads markdown "under `docs/` **and `plans/`**" (:3432)
  and then that markdown is checked "under `docs/` **only**" (:3444).
- §13.4 assertion 3 (:3353) still reads absolutely ("no value unmapped"); the
  200-279 carve-out appears only in assertion 12 and §4.4.
- `devbox run check` — the command `.claude/agents/reviewer.md` tells a reviewer
  to run — does not include `no-real-numbers`. `lint-names` is covered
  incidentally because `go test ./...` runs `TestNameLint`.
- §3.2 (:493) and §11.4 say all seven cookies are `httpOnly`. `APISID` and
  `SAPISID` are not — they are readable by JavaScript, which is how Google's own
  web apps compute the `SAPISIDHASH`. The operative conclusion is unchanged
  (five of the seven are `httpOnly`, so CDP is still required), but the sentence
  will mislead anyone reasoning about the capture.
- `GetParticipantThumbnail` and `DownloadAvatar` are named in §3.1 as the source
  of `contacts.avatar_hash` and `conversations.group_avatar_url`, but neither has
  a `gm.Backend` method, which D10 says means those fields cannot be served. The
  contacts DTO is not defined anywhere, so this is latent rather than live.

## Pairing UX

§11.4 adopts the assessment in full and correctly: the dedicated
`--user-data-dir` is called mandatory rather than stylistic, the capture waits
for `OSID` to exist before reading, it reads over
`Storage.getCookies`/`Network.getCookies` from **both** `messages.google.com`
and `www.google.com`, `__Secure-1PSIDTS` is read last because it rotates, the
debugging port is loopback-bound on a random port and Chrome is killed on
completion, and the new-profile 2FA challenge is named as a one-time cost rather
than a failure. QR is primary (D19). Nothing further required beyond the
`httpOnly` correction above.
