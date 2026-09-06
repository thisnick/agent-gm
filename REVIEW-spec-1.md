# REVIEW-spec-1 — plans/AGENT_GM_SPEC.md @ 6bf4034

Reviewer: adversarial pass against sources. Library verified against
`/home/nick/code/mautrix-gmessages` at `be48a58` (read-only). Reference
conventions verified against `/home/nick/code/agent-mx`.

## Verdict

**Accept with required changes.**

The spec is unusually careful and its Agent MX port is faithful (zero Matrix
leakage — see F-21). But §3 opens with "Everything in this section is derived
from mautrix-gmessages at the pinned commit", and three of its load-bearing
claims are not in that tree at all: the status-4 stale-ConfigVersion story that
justifies decision D3, the `events.BrowserActive` takeover signal that is the
only evidence for OQ-2, and the `events.QR` event. Two more — the
multiple-devices error and "cookie refresh needs a full re-pair" — are
contradicted by the code. Fix §3 before Slice 1 starts; the rest is bookkeeping.

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

### F-14 (m) devbox.json — `AGENT_GM_HOME` is not a spec variable

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

## Rubric — "every name means what it means in Google Messages"

**Score: 1 / 2.**

The vocabulary is disciplined, Matrix-free, and the `delivery_state` mapping in
§4.4 is numerically correct against `MessageStatusType` at the pin. It fails on
one axis: three names assert Google behaviours that do not exist.

| Name | Where | Why it fails |
|---|---|---|
| `events.BrowserActive` → "another device has taken over" | §3.4 | Google reports session activity, not takeover. Event never fires (F-2) |
| `pairing_multiple_devices` (error code) | §3.5, §7.2 | Google never reports this condition; the library picks a device (F-4) |
| "paired-device slot" | §3.2, OQ-2 | Not a Google Messages concept at this pin (F-11) |
| `config_version_stale` (error code) | §3.7, §7.2 | Names an Agent GM diagnosis as if it were Google's answer (F-1) |

Smallest change to reach 2: delete the first two, restate the third as an
observed symptom in Agent GM's own vocabulary (it already is —
`config_version_stale` is fine *if* §3.7 stops claiming Google returns 4 for
it), and drop the slot language.

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
- Not answerable from source, correctly left to the owner: OQ-3, OQ-4, OQ-5,
  OQ-6, OQ-7, OQ-9.

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
