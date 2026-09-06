I've read the full spec (4729 lines) plus all six supporting files. Findings below.

---

# TASK A — QR residue

## A. LIVE CONTRACT — must be removed or reframed

**A-1. §11.3 line 3090 — `agm unpair [--yes]`.** The command list still carries `agm unpair`. Under D30 there is no unpair: there is `agm logout --account <id>` (§4.7 line 1406) and `agm accounts remove <id>` (line 1441). No route in §7.5 backs `agm unpair`; the closest are `POST /v1/accounts/{id}/logout` and `DELETE /v1/accounts/{id}`. This also breaks §16 Slice 2 test 27 ("every CLI command maps to a route").

**A-2. §11.3 line 3178 — destructive-command list.** `Destructive commands (`unpair`, `conversations delete`, …)` — same stale command, and it is the one the confirmation-prompt contract is written around.

**A-3. §12.4 line 3562 — audit kinds.** `"pairing start, success, failure and unpair"`. `unpair` is not an audit kind any more; the account kinds enumerated two lines above are `account.paired`, `account.resumed`, `account.logged_out`, `account.removed`, `account.label_changed`, `account.state_changed`. Dangling live contract.

**A-4. §13.1 line 3622 — fake backend.** `"Run **both pairing flows** including `RefreshGoogleCookies`, a wrong-account refresh, and several primary devices with a settable `device_index`."` There is one flow (D19). "Both" is QR residue stated as a fake-backend requirement.

**A-5. §16 Slice 2 deliverables, line 4202.** `"the `agm` CLI of §11 including **both pairing flows**"`. Same residue in an acceptance-gated deliverable list. (Charitably "both" = Chrome + `--paste`, but §11.4 calls `--paste` a *fallback within the one flow*, not a second flow — the wording is exactly the QR-era phrasing.)

**A-6. §3.1 line 335 — `(*Client).Unpair` … `else `UnpairBugle`. **The only unpair call Agent GM makes.**`** Two problems: (a) `UnpairBugle` is the QR-network branch and is now unreachable, since every Agent GM session has cookies (§3.2 line 557 "Cookies are kept for the whole life of the session"); (b) after A-1/A-2 there is **no** caller of `Unpair` at all on any surface — no route, no tool, no CLI command. §3.1's own rule is "Agent GM calls exactly these symbols", so either a caller must be named or the row must go. Duplicated at §3.2 line 608-609.

**A-7. §2.3 line 214 — `Unpair(ctx context.Context) error` in the `gm.Backend` interface.** Same as A-6 at the interface level: the interface exposes a method no surface reaches. §13.2/§16 Slice 1 test 6 requires the fake to implement every method and "if a method is unimplementable against the fake, it does not belong in the interface" — the stronger rule ("if nothing calls it, it does not belong") is now violated.

**A-8. §3.2 line 443 — heading "### 3.2 Pairing flow**s** and what the owner does on the phone"** and line 484-485 step 1 `POST /v1/pairing/start {"method":"google", ...}`. The `method` field is explicitly abolished at §7.5 line 1978 ("There is one pairing flow, so there is no `method` field; a body carrying one is `invalid_request` naming it"). Line 485 is a **direct contradiction of a live route contract** — a spec example showing a body that the route must reject.

## B. §3.1 library-surface symbols Agent GM no longer calls (flagged per instruction)

**A-9. §3.1 line 337 — `(*Client).PairCallback` / `completePairing`.** The row's own text says *"Agent GM leaves it **nil**"*. §3.1's framing sentence is "Agent GM calls exactly these `libgm` symbols" — a field it deliberately does not set is not a call. Either move it to the "Upstream behaviours Agent GM depends on" prose block below (line 339, where the sibling `DoGaiaPairing`-reconnects note already lives), or keep it with an explicit "not called; listed because leaving it nil is load-bearing" marker.

**A-10. §3.1 line 313 — `(*Client).SetProxy` … "unused; Agent GM has no proxy support."** Same class: an explicitly unused symbol in a table headed "calls exactly these".

**A-11. §3.1 lines 306-307 / 314-315 — `ConnectBackground` ("Agent GM does **not** use it"), `SetPingInterval` / `SetDataReceiveCheckInterval` ("Agent GM leaves the default").** Same class, lower stakes.

## C. Legitimately retained / history — no action

- Line 209 `IsBugleDefault`, line 315 `DefaultBugleDefaultCheckInterval`, lines 354/819/1493 `MessageType_BUGLE_ANNOTATION`/`BUGLE_MESSAGE`, line 360, line 2021 — "Bugle" here is Google's own internal product name for the Messages app, unrelated to QR pairing. Correct as-is.
- Lines 325-326 and 932-933 — `util.QRNetwork` ("Bugle") "appears only inside the two pairing HTTP payloads … never as a runtime network value." Correct negative statement, but it is **stated twice verbatim** (§3.1 and §3.7). One is redundant.
- Lines 332, 343, 639 `PairSuccessful` — live and correct (it is the gaia flow's completion event).
- Line 4603 (D19), line 4727 (§18.3), line 4698 (§18.2 "Answered by the first spec review") — history/decisions. Correct.

## D. One accurate-but-under-explained hit

**A-12. §3.4 line 699 — `| *events.PairSuccessful{PhoneID, QRData} | pairing done; `QRData` is `*gmproto.PairedData` |`.** This is the real upstream field name and is factually right, but it is the single place in the live event catalogue where an agent-facing reader meets "QR" with no note. Add "the field name is vestigial upstream; it carries the gaia pairing result, not a QR code" — otherwise it reads as a surviving QR path.

## E. Confirmed absent (searched, zero hits)

`StartLogin`, `RefreshPhoneRelay`, `GenerateQRCodeData`, `qr-ascii`, `qr-file`, `--qr`, `method: "qr"`, half-block rendering, `QRNetwork` as a runtime value. Only §18.3 line 4727 names the first three, correctly as out-of-contract history.

---

# TASK B — single-account residue

## §1–§2

**B-1. §1.3 line 89 (N4).** `"(D28 — per-account scoping is deferred, not refused)"` — **wrong decision number**. Per-account scope deferral is **D29** (line 4612). D28 (line 4611) is the account-identifier decision. See also C-1.

## §3 — the Google layer contract (the largest cluster)

§3.3 through §3.5 are still written as if there is *one* session, one supervisor and one state machine. Specifically:

**B-2. §3.2 line 591 — `"Agent GM marks the session `logged_out`; reads keep working, **writes return `not_paired`**"`.** Direct contradiction of §4.7 line 1418, §7.8 line 2185 and D30: a `logged_out` account's writes must be `unsupported_capability` + `details.reason = "not_logged_in"`. `not_paired` now means *zero accounts on the server*. This is the single most load-bearing `not_logged_in` inconsistency in the spec.

**B-3. §3.2 line 612 — `"mark `unpaired`, stop the poller, require a fresh pair."`** `unpaired` is **not** in `accounts.state` (§4.7 line 1395-1404: `pairing|connected|degraded|error|logged_out|account_changed`), and §7.5 line 1984 says explicitly *"There is no server-level 'unpaired' state."* Also "the poller" — singular; it is that account's poller.

**B-4. §3.4 line 673 — `` `ErrRequestedEntityNotFound` → invalidate as `unpaired` ``.** Same undefined state, in the live event catalogue.

**B-5. §3.4 line 688 — `"Either match → invalidate the session as `bad_credentials`."`** `bad_credentials` is likewise not in the §4.7 state vocabulary. Presumably `logged_out` (cookies dead) or `error`.

**B-6. §3.4 lines 664-708, whole catalogue.** Every reaction column is written singularly — "mark `connected`", "session → `degraded`", "session → `connected`", "persist `AuthData`", "health flag `phone_responding=false`", "re-issue `SetActiveSession`". None says *which* account. §2.4 line 278 establishes one ingest goroutine per account, so the catalogue is per-account by implication only. Add a one-line preamble: "Every row below applies to one account's event stream; the reaction is scoped to that account's row and that account's `gm.Backend`."

**B-7. §3.4 line 674 — `PhoneNotResponding` → "health flag `phone_responding=false`".** In §7.5 the health DTO has `phone_responding` per account (line 2000), but the §3.4 text reads as a global flag. Same for lines 675, 730, 733 ("phone-health fields in `GET /v1/health`").

**B-8. §3.5 lines 756-757 — `events.ErrInvalidCredentials` and `events.ErrRequestedEntityNotFound` → `not_paired` (409).** Under D30 these are *per-account* credential failures on an account that still exists and is still readable. They must map to `unsupported_capability` / `not_logged_in` (409), not to `not_paired`, which §7.2 line 1879 and §7.8 line 2197 both define as "there are **no accounts at all**". Two contradicting mappings for the same condition.

**B-9. §3.5 — no `not_logged_in` row.** The taxonomy has no entry for the per-account condition at all; §7.8's `not_logged_in` reason has no library-error origin listed.

**B-10. §3.3 line 637 — `"Written **atomically** (`session.enc.tmp` + `fsync` + `rename`)"`.** Singular path, contradicting line 621's `sessions/<acct_id>.enc`. Should be `sessions/<acct_id>.enc.tmp`.

**B-11. §3.1 lines 305, 306, 312, 354 — "Agent GM calls this once before `Connect`", "the only connect path", "Recorded in the audit log on connect", "at least once **per process**".** Line 354's `ListConversations` rule is the sharpest: `conversationsFetchedOnce` is per **`Client`**, and there is one `Client` per account, so the rule is "at least once **per account's client**", not per process. As written, a two-account deployment could satisfy the letter of the rule with one call and leave the second account's conversation events untrustworthy. §3.7 line 817-821 repeats the same per-process error.

**B-12. §3.1 line 312 — `CurrentSessionID` "Recorded in the audit log on connect."** No `account_id` mentioned; §12.4 requires one.

**B-13. §3.6 / §3.7 — config version.** §3.7 line 866-873's health JSON block is the **old flat shape** (`"google": { config_version_compiled, config_version_live, … }`) with no `account_id`, contradicting §7.5 line 1998-2015 where `google` is nested per account and `config_version_compiled` moved to the top level. Two incompatible health DTOs in the spec.

## §4

**B-14. §4.2 line 1003 — `-- keys: pending_reprocess, upstream_commit, config_version_compiled`.** This list does **not** include `backfill_complete_at`, but §5.2 line 1504 writes it (see B-17). Either the key list or §5.2 is wrong.

**B-15. §4.2 line 1050 — `CREATE INDEX conversations_all ON conversations(last_activity_ms DESC, id DESC);`** This is the cross-account listing index, which is correct for the "read omitting `account` covers every account" rule (§7.3). But there is **no equivalent cross-account index for contacts** (`contacts_name` at line 1086 is name-only) and `GET /v1/contacts` (§7.6 line 2046) permits omitting `account`. Cross-account contact listing has no ordered index.

**B-16. §4.2 line 1210-1224 — `uploads` has no `account_id`.** Nothing binds a reservation to an account. §10.2 step 3 (line 2956) sends an upload into whatever conversation the caller names, so cross-account misuse is possible: reserve while thinking of account A, send into account B. That may be intentional (the conversation implies the account), but the spec never says so, and §7.3 line 1941 claims "Every DTO that can appear in a multi-account result carries `account_id`" while the upload DTO (§10.2 line 2936-2945) has none. Decide and state it.

**B-17. §4.7 line 1461-1463 — `accounts.max_concurrent` overflow.** *"beyond it, accounts are connected in `last_event_at_ms` order and the rest stay `degraded` with a stated reason."* But §4.7's own table (line 1401) defines `degraded` as "transient listen error; retrying" with **writes: yes, likely to fail**. An account parked for capacity is not in a transient listen error and its writes should not be attempted. Overloading `degraded` for two unrelated causes means an agent reading `state` cannot tell "retrying a blip" from "not scheduled". Needs a distinct state or an explicit `state_reason` field (which no DTO carries).

**B-18. §4.7 line 1422 — "Re-pairing resumes the same rows" and line 1436.** Correct, but the transition it produces is not in the §4.7 state table: there is no arrow from `logged_out` back to `pairing`/`connected` documented as a state machine, only prose. §7.5 line 1979's pairing poll returns `account_id?` "once the address is known" — the account is `logged_out` during that window while a *new* pairing is in flight, which the state table does not model.

## §5 — ingestion

**B-19. §5.2 line 1504 — `7. Set server_meta.backfill_complete_at.`** **Global key, per-account fact.** `accounts.backfill_complete_at_ms` exists (§4.2 line 1018) and `GET /v1/health` reports `backfill.completed_at` per account (§7.5 line 2007). Two accounts would race on one `server_meta` row: the first to finish marks the whole server complete. Must be `accounts.backfill_complete_at_ms` for that account.

**B-20. §5.2 lines 1513-1517.** `"GET /v1/health reports `backfill: {state, conversations_done, conversations_total}`, and every list response carries a `history_incomplete` warning until `backfill_complete_at` is set"`. Both singular: the health shape is per-account in §7.5, and the warning rule needs to be "until every account in scope has `backfill_complete_at_ms` set" — a cross-account read while account B is still backfilling must still warn even if account A is complete.

**B-21. §5.2 line 1507-1511.** `backfill.concurrency` (default 2) is stated without saying whether it is per account or global. With `accounts.max_concurrent = 8` that is the difference between 2 and 16 concurrent `FetchMessages` calls. Same ambiguity for `backfill.max_messages_per_conversation` and `backfill.horizon` — §18.2 line 4666-4668 says the horizon is "a per-server setting applied to **each** account", which should be stated in §5.2 and in the §15.1 settings table, not only in the open-questions preamble.

**B-22. §5.2 line 1508-1511 — backfill pause.** *"Backfill is **paused** while a `MOBILE_DATABASE_SYNC_STARTED`/`SYNCING` alert is outstanding"*. Which backfill? The alert comes from one account's phone; it must pause only that account's backfill. As written, one sleepy phone stalls every account.

**B-23. §5.3 line 1521-1522 — `"One goroutine, `core.ingestLoop`, drains `gm.Events()`"`.** Singular, and it says it drains *the* event channel. §2.4 line 278 says one goroutine per account. Contradiction; §5.3 must read "one per account". Step 3 (line 1532) does carry `account_id` correctly, and step 8 correctly says "for this account" — only the framing sentence is wrong.

**B-24. §5.4 line 1575-1577 — `"`last_activity_ms` is monotonic per conversation."`** Correct as stated (conversations are per-account), no change needed. Noted because the sibling rules are not.

**B-25. §5.4 line 1609-1610 — sweep triggers.** The timer row uses `last_sweep_at_ms` unqualified and `POST /v1/admin/backfill` "with no body" → `since = epoch`. §7.7 line 2170 says a bodiless `POST /v1/admin/backfill` re-opens backfill for **every** account — so the epoch sweep is an all-accounts full re-backfill, which is a very expensive default that neither section flags.

**B-26. §5.4 line 1614 — `"`GET /v1/health` reports `last_sweep_at` and `sweeps_total`."`** Singular; §7.5 nests these under each account's `sweep` block.

## §6 — operations

**B-27. §6.2 line 1673-1687.** The order is correct and account-aware (step 3 resolves the account, step 5 checks usability, step 9 says "that account's libgm client"). No finding. Step 5's error is written `unsupported_capability / not_logged_in` — consistent with §7.8. Good.

**B-28. §6.6 line 1808-1811 — crash recovery `UPDATE operations … WHERE status='running'`.** Global sweep across all accounts. That is almost certainly right, but the spec should say so, because an operator reading §4.7's "accounts are independent" would expect per-account recovery. Also, the operation reaper for `pending_timeout` (line 1819-1820) is likewise global and unstated.

**B-29. §6.3 line 1721-1729 — `tmp_id` correlation.** `operations_tmp_id` index (line 1195) is **not** account-scoped, while `messages_tmp_id` (line 1118) **is** `(account_id, tmp_id)`. §5.3 step 8 says "If `Message.TmpID` matches an operation's `tmp_id` **for this account**" — so the lookup is account-scoped but the index that serves it is not. A bare UUID collision across accounts is vanishingly unlikely, but the index and the stated query disagree. Make it `operations(account_id, tmp_id)`.

## §7 — routes

**B-30. §7.4 line 1946 vs §7.3 line 1943.** Line 1943 says "A cursor is bound to the `account` filter like any other (**§7.3** pagination)" — pagination is **§7.4**. §4.5 line 1365 makes the same error: "pagination cursor signing (**§7.3**)". Two broken cross-refs introduced when §7.3 became "Choosing an account".

**B-31. §7.5 line 1971 — `GET /v1/accounts` DTO** omits `label`… no, it has it. But it omits the `sweep` and `counters` blocks that `GET /v1/health` carries per account, and omits `google`, while `GET /v1/accounts/{id}` (line 1972) adds `google` only. Since `get_session` (the MCP tool, §8.2 line 2273) maps to `GET /v1/accounts/{id}`, a model calling `get_session` gets `google` but a model calling `list_accounts` does not — and §8.3 line 2480 tells the model *"`list_accounts` and `get_session` tell you whether it is: `state` and `phone_responding`"*. `phone_responding` is in `/v1/accounts` (line 1971) — OK — but the asymmetry should be stated.

**B-32. §7.6 line 2046 — contacts DTO `{id, display_name, phone, is_top, avatar_hash, updated_at}` has **no `account_id`**.** Direct contradiction of §7.3 line 1941-1942: *"Every DTO that can appear in a multi-account result carries `account_id`: conversations, messages, **contacts**, attachments, operations and search results."* `GET /v1/contacts` accepts `account` as an *optional* filter, so a cross-account contact list is unattributable — two accounts with the same person produce two rows the caller cannot tell apart.

**B-33. §10.1 line 2896-2906 — attachment DTO has **no `account_id`**.** Same contradiction with §7.3 line 1941 ("attachments"). `attachments.account_id` exists in the schema (line 1134) but is not served.

**B-34. §7.6 line 2049-2050 — `GET /v1/operations/{id}` and `GET /v1/uploads/{id}` take no `account` filter,** which is right (ID-addressed), but there is **no `GET /v1/operations` list route at all** — so an owner with two accounts cannot list operations per account. §11.3 has `agm operations show`/`wait` only. Possibly intended; not stated.

**B-35. §7.6 line 2052-2054 — `participant` / `sender` resolution.** *"`participant` accepts an E.164 number …, or a `part_`/`contact_` ID. `sender` accepts the same plus the literal `me`."* Two problems in a multi-account world: (a) a raw phone number matches participants in **every** account, and `participants` has no `account_id` column (§4.2 line 1056-1069) and `participants_phone` (line 1070) is a global index — so an account-filtered query needs a join the schema does not index; (b) **`me` is now ambiguous** — "me" is a different participant in each account. `sender=me` on a cross-account `GET /v1/messages` is undefined. This needs an explicit rule.

**B-36. §7.7 line 2137 — `POST /v1/conversations`.** Correct: `account_id` required when >1. Good.

**B-37. §7.7 line 2145-2147 — `POST /v1/uploads`** takes no `account_id`. See B-16.

**B-38. §7.7 line 2172 vs §12.4 line 3574 — audit filter list.** §7.7 lists `kind, kind_prefix, **account_id**, authorization_id, after, before, cursor, limit`. §12.4 lists `kind, kind_prefix, authorization_id, after, before` — **`account_id` dropped**. §12.4 line 3549-3552 is the section that *establishes* per-account audit rows, so dropping the filter there is self-contradicting.

**B-39. §7.5 line 1965-1966 — `GET /v1/health` scope is `messages:read`** and it returns every account's `google_address`. Consistent with D29 (global scopes), but worth one sentence: a `messages:read` token enumerates all of the owner's Google addresses.

**B-40. `GET /v1/session` — confirmed removed.** No such route exists in §7.5. Good. But two places still name it: §8.2 line 2251 and §18.3 line 4718 (see B-45, B-56).

## §8 — MCP

**B-41. §8.2 line 2251 — exclusion table row `GET /v1/session/events`.** **This route does not exist.** It is `GET /v1/accounts/{account_id}/events` (§7.5 line 1974). That route is scoped **`messages:read`**, so under §8.2's rule ("every `/v1` route carrying a `messages:*` scope has a tool, except the three named in the first three rows") it is a messaging route with neither a tool nor a correctly-named exclusion. §16 Slice 3 test 16 is a *two-way table test over exactly that statement* and would fail on this row.

**B-42. §8.2 line 2253 — the `admin`-scope exclusion row lists `/events`** among "`admin` scope, or a stream" — but `/v1/accounts/{id}/events` is `messages:read`, not `admin`. The same route is now covered by two exclusion rows, one under the wrong name and one under the wrong scope.

**B-43. §8.2 — `GET /v1/attachments/{id}/content` (messages:read) has no tool and is not an exclusion.** It is served as an MCP *resource* (`agm://attachments/{id}`, line 2371), which is a legitimate answer, but §8.2's rule and Slice 3 test 16 do not have a "served as a resource" category. Pre-existing, but it fails the same two-way table test.

**B-44. §8.2 line 2235 — "Twenty-one tools — eleven reads, eight writes, two deletes."** Correct against the tables (11+8+2). But **§16 Slice 3 test 14, line 4445, says "`tools/list` under `messages:read` returns exactly the **ten** read tools"** — not updated when `list_accounts` was added. Off by one in an acceptance test.

**B-45. §8.2 line 2265-2275 — read tool arguments.** `get_attachment` (`attachment_id`) and `get_conversation`/`get_message`/`message_context` are ID-addressed — fine. But **`get_attachment` returns a DTO with no `account_id`** (B-33), and `list_contacts` returns rows with no `account_id` (B-32), so a model doing cross-account work cannot attribute either.

**B-46. §8.3 instructions block — does a cold agent know to call `list_accounts`?** **Yes, and this is done well.** Lines 2399-2413 are explicit and correct: call it first, one-account default, writes must name one, `invalid_request` lists the candidates, `conv_`/`msg_` imply the account, non-`connected` is readable but `not_logged_in` on write. Three residual issues:
- Line 2438: *"`search_messages` needs `q` and searches **the whole account**."* Wrong under the §7.3 rule — omitting `account_id` searches **every** account. Should read "searches every account unless you pass `account_id`".
- Line 2428-2434 step 1 has a broken parenthetical: *"`list_conversations` with `participant` set to a phone number — add `account_id` to look in one account, or leave it out to search them all (`+15105550123`, or the bare digits, or a national form) returns the threads…"* The example list has been spliced into the middle of the multi-account clause; the sentence no longer parses.
- Line 2465-2466: *"One reaction per person per message"* — fine. But nothing in the block tells the model that `me` differs per account (B-35), nor that `create_upload` is not account-bound (B-16).

**B-47. §8.2 line 2332 — annotations table.** `list_accounts` correctly present. Good.

## §9 — OAuth

**B-48. §9.7 line 2841 — "**Scopes are global across accounts** (D28)".** **Wrong decision number** — that is D29 (line 4612). Same error as B-1.

**B-49. §9.7 line 2846-2848 — the authorization-screen statement.** *"the honest statement is the one the authorization screen makes: *'this will let the client read and send as any Google account on this server.'*"* But **§9.4 (lines 2654-2677), which specifies the authorization screen's actual form contents, never mentions accounts.** The form carries "the signed context, an anti-CSRF `form_token`, hidden echoes of the OAuth parameters, one `scope` checkbox per requested scope, and an `enrollment_code` text input" — no account disclosure. The quoted sentence exists only in §9.7 prose and is not in the screen's contract, so no test can assert it. Add it to §9.4.

**B-50. §9.5 — enrollment codes** carry a scope ceiling but no account dimension. Consistent with D29; no change needed, but the deferral could be noted here rather than only in §9.7.

## §10 — media

**B-51. Media tickets are not account-scoped.** Download tickets key on `attachment_id` (§4.2 line 1227-1235), which transitively implies an account — acceptable. Upload tickets key on `authorization_id` only (line 1210-1224) with no account — see B-16. §10.3 line 2982-2985 says "Every redemption re-checks the issuing authorization's scope and revocation state"; under D29 scopes are global, so there is nothing account-shaped to re-check. State explicitly that upload tickets are deliberately account-agnostic and the account is fixed at send time.

**B-52. §10.3 line 2995-3005 — the limits table and `media.cache_max_bytes` (2 GiB).** Global across accounts. With N accounts each backfilling, one account's media can evict another's. Not wrong, but the LRU is cross-account and the spec should say so — an operator would reasonably assume a per-account budget.

## §11 — CLI (the second-largest cluster)

**B-53. §11.1 lines 3033-3043 — global flags: there is no `--account`.** Yet `--account` is referenced as an existing flag at lines 3150, 3153, 3342, 3362, 4071 (implicitly), 4072, 4075, and `AGENT_GM_ACCOUNT` (§15.1 line 3986) is defined as *"Default `acct_` ID for `--account` (**§11.1**)"* — pointing at a section that does not define it. **The single most-referenced missing item in the spec.**

**B-54. §11.3 lines 3085-3141 — the command list has no account commands.** Missing entirely: `agm accounts list`, `agm accounts show`, `agm accounts label` (both named in the §11.4 pairing epilogue, line 3363), `agm accounts remove` (D30's *only* purge, named at lines 553, 1441, 4076), and `agm logout --account <id>` (§4.7 line 1406, §12.1 line 3457). Five commands that other sections treat as existing.

**B-55. §11.3 line 3091 — `agm session [--watch]`, line 3093 `agm health`, line 3092 `agm reconnect`.** All singular and account-less. `agm reconnect` maps to `POST /v1/accounts/{id}/reconnect` (line 3152) which **requires** an account ID in the path; the command has no way to supply one. `agm session` maps to `GET /v1/accounts/{id}` and needs `--account`.

**B-56. §11.3 line 3136 — `agm admin backfill [--conversation <conv-id>]`.** The route (§7.7 line 2170) takes `{"account_id"?, "conversation_id"?}`; the command has no `--account`. Breaks §16 Slice 2 test 27 ("Every `/v1` route parameter is reachable from a CLI flag").

**B-57. §11.3 lines 3095-3120 — no `--account` on any list/search command.** `conversations list`, `messages list`, `messages search`, `contacts list` all map to routes with an `account` parameter (§7.6 lines 2038, 2041, 2045, 2046). Line 3148-3150 says *"`agm messages list` with no conversation ID is `GET /v1/messages` across every account, or one account with `--account`"* — so the flag is asserted in prose and absent from the synopsis for every command.

**B-58. §11.3 line 3144 — "**Every `/v1` route parameter has a flag**, with one stated exception".** False as written: `account`/`account_id` on ~8 routes has no flag; `POST /v1/accounts/{id}/logout`, `DELETE /v1/accounts/{id}`, `PATCH /v1/accounts/{id}`, `GET /v1/accounts` have no commands. The "one stated exception" claim needs to become several, or the commands need adding.

**B-59. §11.3 line 3122-3123 — `agm operations show|wait`.** No `--account`; consistent with there being no operations list route (B-34).

**B-60. §11.4 line 3342 — heading `#### agm pair --refresh-cookies --account <acct-id>`** uses `--account` (B-53) and is the only place the flag appears in a heading.

**B-61. §11.4 line 3264-3270 — the kept Chrome profile.** *"**There is one browser profile, not one per account**"* — good, explicit, correct. But `agm pair --forget-browser` then destroys the shared credential for **every** account's future `--refresh-cookies`, and its `effect` sentence (line 3267) is not quoted anywhere, so it cannot be checked against the "same words on all three surfaces" rule (§11.3 line 3180).

**B-62. §11.2 lines 3049-3075 — exit codes.** `not_paired` → 10. `unsupported_capability` → 6, which is how `not_logged_in` surfaces. That is correct and distinct. **But** §11.2 line 3055 describes exit 6 as *"unsupported capability for this conversation or message"* — `not_logged_in` is a property of the **account**, not of a conversation or message. The exit-code gloss needs "…or account".

## §12 — security, logging, rate limits

**B-63. §12.1 line 3453-3462 — good.** Correctly states one key covers all accounts, blast radius grows per account, `agm logout` shreds one file. Only nit: it says `agm logout` (line 3457) which §11.3 does not define (B-54).

**B-64. §12.2 line 3478-3480 — Google account addresses redacted from logs, `acct_` ID instead.** Good and explicitly multi-account.

**B-65. §12.3 lines 3495-3506 — every rate limit is per **authorization** or per **source**, none per account.** Under D29 that is defensible, but it has a consequence nobody has stated: **one authorization sending to N accounts shares one 120 req/min mutation bucket**, so adding an account halves the effective per-account send rate. And §16 Slice 2 test 30 asserts "a session holding all four scopes gets one allowance per surface, not four" — the analogous multi-account assertion ("N accounts do not multiply the allowance", or "…do multiply it") is untested and unstated.

**B-66. §12.4 line 3549-3552 — good.** Explicitly per-account with the survives-removal rule. Only defect is the dropped `account_id` filter at line 3574 (B-38).

## §13 — testing

**B-67. §13.1 lines 3583-3590 — good.** One fake = one account, explicitly, with the reasoning. Excellent.

**B-68. §13.1 line 3622 — "both pairing flows"** (see A-4).

**B-69. §13.1 — the fake's scriptable-failure list (lines 3603-3621) has no multi-account cases.** Nothing scripts *divergent* behaviour between two accounts: account A healthy while B returns `ErrInvalidCredentials`, A's clock advancing while B's does not, A's dedup abandoning a batch while B's does not. §16 Slice 2 tests 31-35 need exactly this and the fake's contract does not promise it.

**B-70. §13.1 line 3625-3629 — "**The fake takes a clock**."** One clock. With N fakes, is it one shared clock or one per fake? Tests 31/32 (logout, re-pair) need account A's timers to advance while B's do not — or explicitly not. Unstated.

**B-71. §13.2 line 3641-3643 — "**ID derivation**, including that the `account_key` component is present and that **a different phone** produces different IDs."** Two errors: (a) `account_key` is not a term used anywhere else — §4.1 line 950 and D6 say `account_id`; (b) **"a different phone" is exactly backwards.** §3.2 line 543 and §4.1 line 976-979 establish that a *different phone on the same account* produces the **same** IDs; only a different **account** produces different ones. The test as specified would assert the opposite of the contract.

**B-72. §13.2 — no multi-account test in the unit/integration list.** The list covers ID derivation, delivery states, idempotency, crash recovery, ordering/dedup, cursors, error mapping — none of it two-account. §16 Slice 2 tests 31-35 exist but are not reflected here, and §13.2 is the section that says "nothing here is behind a gate".

**B-73. §13.3 line 3692-3705 — approved numbers.** Single set. A second account pairs to a **second phone**; the live-gate placeholder table has no `<APPROVED_SECOND_ACCOUNT>` and §17 line 4550-4553 correctly says two-account live gates need the owner and are run against fakes instead. Consistent — no change needed, but §13.3 should say so rather than leaving it to §17.

**B-74. §13.4 — the twenty fixture assertions are all account-agnostic.** Correct; they validate upstream. No finding.

## §14 — packaging

**B-75. §14.1 line 3888 — `ENV AGENT_GM_DATA_DIR=/data`** and `VOLUME ["/data"]`. Fine for N accounts (one `sessions/` dir). No finding.

**B-76. §14 — no statement of resource scaling.** N accounts = N long polls, N ingest goroutines, N sweep timers, N backfills. §15.1's `accounts.max_concurrent` bounds it; §14 never mentions memory/CPU implications for the container. Minor.

## §15 — operations

**B-77. §15.1 line 3975 — `AGENT_GM_DATA_DIR` "holds `agent-gm.sqlite3`, `sessions/` (one file per account, §3.3)…"** Good.

**B-78. §15.1 line 3985 — `AGENT_GM_PAIRING_TIMEOUT` `5m`, "§3.2".** §3.2 never states a 5-minute pairing timeout — it states `GaiaInitTimeout` = 20s and `ErrPairingTimeout` from Google. Dangling reference (pre-existing, not multi-account).

**B-79. §15.1 line 3995-4014 — the settings table marks only `ingest.sweep_interval` as "Per account".** Every other key's scope is unstated. From the text elsewhere: `backfill.*` are per-server-applied-to-each-account (§18.2), `accounts.max_concurrent` is global, `operations.*` are global, `media.cache_max_bytes` is global (B-52). Add a "Scope" column; this is exactly the kind of thing that gets implemented wrong.

**B-80. §15.2 line 4051-4053 — "the server starts **unpaired** and the owner re-pairs; because **`account_key` is derived from the phone** (§4.1), re-pairing **the same phone** keeps every existing `conv_` and `msg_` ID."** **Three errors in one sentence:** "unpaired" is not a state (B-3); `account_key` is not a term (B-71); and the derivation is from the **Google account address**, not the phone — §4.1 line 950 and §3.2 line 543 say explicitly that re-pairing a *different* phone on the same account keeps the IDs, which is the whole point of D28. As written it tells an operator the opposite of the contract. Should read: "…the owner re-pairs each account; because `acct_` derives from the Google account address, re-pairing the same **account** — even onto a different phone — keeps every existing `conv_` and `msg_` ID."

**B-81. §15.2 line 4042-4045.** Good — "leaves **every** account `logged_out` with its history intact". Correct and plural.

**B-82. §15.3 lines 4055-4063 — the health summary is the **old single-account shape**.** *"`GET /v1/health` for everything else: `session.state`, `google.*` (§3.7), `backfill`, `counters` (`dropped_events`, `unknown_events`, `pending_operations`), `version`, `commit`, `source_url`, and the `client_source`."* There is no `session.state` field; §7.5's DTO has `accounts_summary` + an `accounts[]` array with per-account `state`, `google`, `backfill`, `sweep`, `counters`. §15.3 also omits `accounts_summary` entirely. And line 4062: `agm session --watch` (B-55).

**B-83. §15.4 runbook — mostly good** (rows at 4070, 4071, 4072, 4075, 4076 are properly per-account). Two defects:
- Line 4071 and 4076 invoke `agm accounts list` / `agm accounts remove`, which §11.3 does not define (B-54).
- Line 4072/4075 invoke `agm pair --account <id>` / `agm pair --refresh-cookies --account <id>`, using the undefined `--account` (B-53).
- **No row for "an account is stuck `degraded` because `accounts.max_concurrent` is reached"** (B-17) — the operator has no way to diagnose it, since `degraded` also means "transient listen error".
- **No row for `account_changed`.** It is a state (§4.7 line 1404) that refuses writes and there is no runbook entry telling the owner what to do about it.

**B-84. §15.5 upgrading — no multi-account content.** Migrations are global; fine. But a migration that backfills `account_id` onto pre-existing rows is exactly the `pending_reprocess` case (§4.3 line 1295) and is not mentioned. Minor.

## §16 — acceptance tests, slice by slice

**Slice 1** — line 4155-4159 test 4 is a genuine two-account test and is well specified. Test 7 (line 4167) correctly says "the only pairing flow (D19)". **No defects.** Test 12 (line 4189) *"the session file reloads"* — singular, should be "each account's session file".

**Slice 2** — tests 31-35 (lines 4332-4360) are excellent and cover logout/history, re-pair/no-duplicates, remove/purge, ambiguity, per-account idempotency. Defects:
- **Line 4202 deliverables — "both pairing flows"** (A-5), and the deliverables do **not** mention the `internal/accounts` supervisor, the `agm accounts` commands, or `agm logout` — yet tests 31-33 exercise them.
- **Tests 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11 are all implicitly single-account.** Legal (the §7.3 default), but tests 2/3/4 (ingest, ordering, sweep) are the ones most likely to break under two accounts — interleaving two accounts' events is untested. Test 4's sweep test should be run per-account with a second account proving no cross-talk.
- **Test 12 (line 4257) "Every `/v1` route rejects an unknown query parameter … route by route, not a sample"** now has to cover the whole `/v1/accounts/*` subtree, which the Slice 2 deliverables list only implicitly.
- **Test 17 (line 4276) — upload path.** No cross-account case: reserving an upload and sending it into a *different* account's conversation (B-16) is unasserted either way.
- **Test 18 (line 4282) — download ticket** is single-account.
- **Test 20 (line 4289) — erasure order** is tested via a generic "purge"; it should be tested via `DELETE /v1/accounts/{id}` specifically, since §4.7 line 1449-1455 makes account removal *the* erasure-order path.
- **Test 27 (line 4319) "Every `/v1` route parameter is reachable from a CLI flag, and every CLI command maps to a route."** **This test currently fails against the spec itself** — `--account` is missing (B-53), `agm accounts *` and `agm logout` are missing (B-54), `agm reconnect` cannot supply the required path ID (B-55), `agm admin backfill` has no `--account` (B-56), and `agm unpair` maps to no route (A-1).
- **Test 36 (line 4361) — sentinel-secret scan** is single-account; with two accounts there are two cookie sets and two session files, and the §12.1 blast-radius claim ("one key covers all of them") is unasserted.
- **Tests 40-43 (live gates)** are all single-account, correctly per §17 line 4550-4553.
- **Missing entirely:** a test that `agm accounts remove` is the **only** purge — i.e. that logout, `--forget-browser`, unpair-by-phone (`RevokePairData`), cookie expiry and `account_changed` each delete **zero** rows. Test 31 covers logout only.
- **Missing:** a test that `accounts.max_concurrent` actually bounds concurrency and that the overflow accounts reach a diagnosable state (B-17).

**Slice 3** — 
- **Test 14 (line 4445) says "the **ten** read tools"; there are eleven** (B-44).
- **Test 16 (line 4454) — the two-way route/tool table test fails on the stale `GET /v1/session/events` exclusion** (B-41, B-42) and on `/v1/attachments/{id}/content` (B-43).
- **No multi-account MCP test at all.** Nothing asserts: `list_accounts` returns both; a write tool omitting `account_id` with two accounts returns `isError` with `details.accounts`; a read tool omitting it covers both; `get_session` with and without `account_id`; `get_health` returning two account rows. Given that §8.3's instructions block leads with accounts, this is the biggest §16 gap.
- **Test 18 (line 4463) — instructions parity** will catch the §8.3 broken sentence at line 2428-2434 only if `docs/mcp.md` reproduces the same break.
- **Test 23 (live gate, line 4478)** — single-account, correct.

**Slice 4** — no account content anywhere; tests 1-10 are packaging. Test 10's connector gate is single-account. Acceptable, but nothing asserts a connector sees both accounts, which is the whole point of D27 for the primary consumer.

## §17 — agent team

**B-85. Line 4550-4553 — good and explicit:** two-account live gates need the owner; implementers never add an account to the owner's deployment; two-account tests run against two fakes. Correctly cross-references §13.1. No defect.

**B-86. `.claude/agents/implementer.md` line 40 and `reviewer.md` — `session.enc` singular.** implementer.md:40 *"anything from `session.enc`"*. Should be `sessions/<acct>.enc`. reviewer.md:39 correctly says "ID derivation missing the account component" — good.

**B-87. `.claude/agents/reviewer.md` — no multi-account silent-failure prompt.** The list at lines 36-42 should include "a query or index missing `account_id`", "a `server_meta` key that should be an `accounts` column", and "`not_paired` used where `not_logged_in` is meant" — the three failure modes this audit found most of.

## §18 — decisions and open questions

**B-88. D26 is placed after D30** (line 4614, between D30 and the field observation). Out of numeric order; D19-D25 are also interleaved oddly (D19 at 4603 after D18 at 4602, fine, but D26 at the end). Cosmetic.

**B-89. §18.2 line 4662 — "**Six** questions remain"** but **seven are listed** (OQ-1 … OQ-7). Off by one, presumably introduced when OQ-6 (`accounts.max_concurrent`) was added by D27.

**B-90. §18.2 — no open question about per-account media/backfill budgets** (B-21, B-52) or about `sender=me` cross-account (B-35), both of which are genuine unresolved design points D27 created.

## Supporting files

**B-91. `CONTRIBUTING.md` lines 17 and 34 — `session.enc` singular** (twice). Should be `sessions/<acct>.enc`. Lines 3-4 and 13-14 correctly state multi-account/not-multi-user.

**B-92. `docs/README.md` line 32-33 — `+1<APPROVED_DIRECT_NUMBER>`.** The `+1` prefix is prepended to placeholders that the spec (§13.3 line 3695-3698) defines as whole numbers. Cosmetic but it is the kind of thing `no-real-numbers` reasoning depends on.

**B-93. `docs/README.md` line 15-22 — the planned-pages table.** `pairing.md` correctly names "the account model (adding, logging out, removing, re-pairing)". But `api.md` (line 16) does not mention the account-selection rule of §7.3; `cli.md` (line 17) does not mention `--account` / `AGENT_GM_ACCOUNT` / `agm accounts`; `mcp.md` (line 19) does not mention `list_accounts` or the account section of the instructions block; `operations.md` (line 18) does not mention the per-account settings scope. Each page's "what it owes" list predates D27.

**B-94. `README.md` lines 3-5** — correctly multi-account ("as many Google accounts as you have"). No defect. Line 18 "No code yet" — accurate.

**B-95. `devbox.json`, `Makefile`** — no account-shaped content; nothing to fix. Note `devbox.json` has no `AGENT_GM_ACCOUNT` in `env`, which is correct (it is a CLI-side user setting).

---

# TASK C — new-decision coherence (D27–D30, §7.3)

## C-1. Decision cross-reference errors (two sites, same bug)

- **§1.3 line 89 (N4):** `"(D28 — per-account scoping is deferred, not refused)"` → should be **D29**.
- **§9.7 line 2841:** `"**Scopes are global across accounts** (D28)"` → should be **D29**.

D28 (line 4611) is *"The account identifier is `AuthData.Mobile.SourceID`"*. D29 (line 4612) is *"OAuth scopes are global across accounts; per-account scoping is deferred"*. Both citations point a reader at the wrong decision, and one of them (N4) is in the non-goals, which is the most-read part of the spec.

## C-2. `not_logged_in` vs `not_paired` — three contradictions

The distinction is **correct and crisp** in four places: §4.7 line 1403/1418-1420, §7.2 line 1879, §7.8 lines 2185 and 2197-2202, §8.3 line 2411, §16 Slice 2 test 31 line 4337 (which explicitly asserts "**not** `not_paired`"). It is **wrong** in three:

- **§3.2 line 591** — `logged_out` account: *"reads keep working, **writes return `not_paired`**"*. Must be `unsupported_capability` / `not_logged_in`. (B-2)
- **§3.5 line 756** — `events.ErrInvalidCredentials` → `not_paired` (409). A per-account credential death. (B-8)
- **§3.5 line 757** — `events.ErrRequestedEntityNotFound` → `not_paired` (409). Same. (B-8)

The §3.5 rows are the worst of the three because §3.5 is the *taxonomy* — an implementer building the error mapper works from that table, and it will produce `not_paired` for exactly the condition §7.8 says must never be `not_paired`.

**Also missing:** §3.5 has **no row producing `not_logged_in` at all**, so the code has no library origin. And §11.2's exit table (line 3055) glosses exit 6 as "unsupported capability for this **conversation or message**", never "account" — so the CLI's own description of the exit code excludes the account case.

**Consistent where checked:** §7.2 ✓, §7.7 (writes) ✓, §7.8 ✓, §8.2/§8.3 ✓, §11.2 (via exit 6) ✓ modulo the gloss, §16 test 31 ✓, §15.4 line 4071 ✓.

## C-3. Undefined account states leak in from the pre-D27 text

§4.7 line 1395-1404 defines exactly six states: `pairing`, `connected`, `degraded`, `error`, `logged_out`, `account_changed`. §7.5 line 1983-1985 restates them and says *"There is no server-level 'unpaired' state."* But three other states are asserted as live behaviour:

- **`unpaired`** — §3.2 line 612 ("mark `unpaired`"), §3.4 line 673 ("invalidate as `unpaired`"), §15.2 line 4051 ("the server starts unpaired").
- **`bad_credentials`** — §3.4 line 688 ("invalidate the session as `bad_credentials`").
- **`degraded` overloaded** — §4.7 line 1463 uses it for capacity-parked accounts as well as transient listen errors (B-17).

An implementer reading §3.4 will write a state machine with eight states; a reviewer reading §4.7 will reject it. This is the second-biggest coherence break after C-2.

## C-4. `agm accounts remove` as the only purge — mostly consistent, three gaps

**Asserted correctly at:** §3.2 line 553, §4.7 lines 1406-1420 (logout deletes nothing) and 1441-1456 ("Removal is the only thing that deletes"), §7.5 line 1977 ("**The only route that deletes an account's data**"), §12.1 line 3457, §15.4 line 4076 ("that is the only command that deletes"), D30 line 4613, §16 Slice 2 test 33.

**Gaps:**
1. **The command does not exist in §11.3** (B-54). D30 names `agm accounts remove` as the *only* purge and §11.3 — the CLI contract — has no `agm accounts` subtree at all. Worse, §11.3 **does** list `agm unpair` (A-1), which under the old model was the destructive account command; a reader of §11.3 alone would conclude `agm unpair` is the purge.
2. **§11.3 line 3178's destructive-command list omits it.** The list is `unpair`, `conversations delete`, `messages delete`, `auth logout`, `admin ... revoke`, `pair --forget-browser` — the one command that deletes an entire account's history is not in the list that requires the `effect` sentence and the `y`/`--yes` gate, while `unpair` (which no longer exists) is.
3. **No test asserts the *only*.** §16 test 33 proves remove purges; nothing proves that logout, `RevokePairData`, cookie expiry, `account_changed`, `--forget-browser` and a failed re-pair each delete **zero** rows. "X is the only Y" needs the negative half. (Test 31 covers logout alone.)

## C-5. §7.3 (account selection) vs the rest — contradictions

The rule at lines 1916-1944 is clear: reads default to all accounts, writes must name one when >1, zero accounts → writes `not_paired`, IDs imply their account, every multi-account DTO carries `account_id`, cursors bind the `account` filter. Against it:

- **§8.3 line 2438** — *"`search_messages` … searches the whole account"* contradicts "a read may omit it and then covers every account". (B-46)
- **§7.6 line 2046 (contacts DTO)** and **§10.1 line 2896 (attachment DTO)** violate line 1941's "Every DTO … carries `account_id`". (B-32, B-33)
- **§10.2 line 2936 (upload DTO)** — no `account_id`, and uploads have no account dimension at all. Not covered either way by §7.3. (B-16)
- **§7.6 line 2054 — `sender` accepts `me`** is undefined across accounts. (B-35)
- **§11.1** provides no `--account` flag, so the CLI cannot express the rule at all. (B-53)
- **Cross-ref rot:** §4.5 line 1365 and §7.3 line 1943 both cite "§7.3" for pagination, which is now §7.4. (B-30)

## C-6. D27 vs §3 — the supervisor is never specified as plural

D27 promises "each with its own `libgm` client, session file, event stream, backfill and sweep, all concurrent". §2.2 line 176 and §2.4 line 278 and §4.7 line 1458-1463 deliver that. **§3 does not.** §3.3 says "one per logged-in account" (good) but §3.4's whole event catalogue, §3.4's `ListenFatalError` supervisor rule (line 679-688), §3.5's error taxonomy, and §3.7's health JSON are all written in the singular (B-6, B-13). §3 is titled "The Google layer contract" and is the section an implementer reads first for Slice 1 — it is the one section D27 did not reach.

## C-7. D30 vs §4.7 — one unmodelled transition

D30 makes re-pairing free and §4.7 line 1422-1439 specifies it. But the state table has no arrow for `logged_out → pairing → connected`, and §7.5 line 1979's pairing poll returns `account_id?` "once the address is known" — meaning there is a window in which an account is `logged_out` *and* has an in-flight pairing. `GET /v1/accounts/{id}` during that window returns `state: "logged_out"` while `GET /v1/pairing/{id}` returns `state: "waiting"`. Neither surface tells a caller the two are related. (B-18)

## C-8. D29 vs §9.4 — the honest statement is not in the screen's contract

§9.7 line 2846-2848 quotes the authorization screen's disclosure sentence as the thing that makes global scopes honest, but §9.4's specification of the screen (lines 2674-2677) does not include it. Nothing testable. (B-49)

---

**Highest-priority fixes, in order:** C-2 (§3.5 lines 756-757 and §3.2 line 591 — wrong error code for the commonest per-account failure), C-3 (`unpaired`/`bad_credentials` states that do not exist), B-53/B-54 (`--account` flag and the `agm accounts` / `agm logout` commands referenced from eight places and defined nowhere), A-1/A-2 (`agm unpair` residue), B-19 (`server_meta.backfill_complete_at` racing across accounts), B-80 (§15.2's "derived from the phone" sentence, which states the inverse of D28), B-41/B-44 (§16 Slice 3 tests 14 and 16 currently fail against the spec), and C-1 (D28/D29 mis-citation in N4 and §9.7).