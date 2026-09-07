# REST API

Serves spec §6.3, §6.5, §7, §9.6, §9.7, §10 and §12.3.

Base URL: `AGENT_GM_PUBLIC_URL` plus `/v1` — `https://gm.example.test/v1` in
the examples below. JSON in, JSON out, UTF-8 throughout. Every `/v1` route
requires a bearer token except `POST /v1/auth/admin-session`,
`POST /v1/auth/refresh` and `PUT /v1/uploads/{upload_id}/content`, which carry
their own credential; `/healthz` requires nothing.

Anything the CLI can do an agent can do, because [`agm`](cli.md) is a client of
this API and nothing else. If you are looking for a command rather than a
route, read [cli.md](cli.md); if you are looking for a knob, read
[operations.md](operations.md).

## Envelopes

Every `/v1` response is one of two shapes and never anything else.

Success:

```json
{ "data": { "id": "conv_01k4z2p8vq", "name": "Alex" },
  "next_cursor": null,
  "warnings": [],
  "request_id": "req_01k4z2p8w0" }
```

`data` is the object or `{ "items": [...] }` for a listing. `next_cursor` is
`null` on the last page. `warnings` is a list of machine-readable strings —
`history_incomplete`, `filename_truncated`, `sha256_unavailable:bytes_unavailable`
— and is never how a refusal is reported.

Error:

```json
{ "error": { "code": "not_found",
             "message": "No such conversation.",
             "retryable": false,
             "details": {} },
  "request_id": "req_01k4z2p8w1" }
```

`code` is from [the error table](#error-codes) and is the field to branch on;
`message` is for a human and may be reworded. `retryable` repeats the table so
a client need not carry it. `request_id` is on both shapes and is what to
quote in a bug report.

`401` and `403` also carry `WWW-Authenticate` with `realm="agent-gm"`, an
`error` parameter, a `resource_metadata` pointing at the protected-resource
document, and, for a scope refusal, the `scope` the route requires.

**Text normalisation is never silent.** A `reason` or a `filename` carrying
control characters, or longer than its bound — 500 runes for a reason, 255 for
a filename — is accepted, cleaned, and reported in `warnings` as
`reason_normalized`, `reason_truncated`, `filename_normalized` or
`filename_truncated`.

## Strict parameter rejection

**Every `/v1` route rejects an unknown query parameter or an unknown JSON body
field with `invalid_request`, naming it** in `details.parameter` or
`details.field`. Nothing is allowlisted. A cache-busting `?_=1757150000` is an
unknown parameter like any other, and so is `?client_request_id=` — the
idempotency key has two transports and neither is a query parameter.

```console
$ curl -s -H "Authorization: Bearer $TOKEN" \
    'https://gm.example.test/v1/messages?directon=incoming'
```

```json
{ "error": { "code": "invalid_request",
             "message": "Unknown query parameter \"directon\".",
             "retryable": false,
             "details": { "parameter": "directon" } },
  "request_id": "req_01k4z2p8w2" }
```

The reason for the strictness is that one line of JSON above: `?directon=incoming`
is a typo for `direction`, and a server that ignored it would answer `200` with
**every** message in the deployment. That looks exactly like a correct answer,
it is the kind of mistake an agent makes silently, and the caller finds out
when it has already acted on the wrong set. A refused request has no effect —
no operation row, no send, no state change.

The rule applies to `/v1` only. `/mcp`, `/oauth/*` and `/.well-known/*` keep
their RFC behaviour and ignore what they do not recognise, because those are
the surfaces third-party clients drive.

## Authentication in Slice 2

Slice 2 has exactly one credential path: the **admin bootstrap**. OAuth,
enrolment codes and per-client authorizations arrive in Slice 3 and are
documented in `oauth.md` when they do.

```console
$ curl -s -X POST https://gm.example.test/v1/auth/admin-session \
    -H 'Content-Type: application/json' \
    -d '{"secret":"<AGENT_GM_ADMIN_SECRET>"}'
```

```json
{ "data": { "access_token": "agm_at_EXAMPLE_NOT_A_REAL_TOKEN",
            "refresh_token": "agm_rt_EXAMPLE_NOT_A_REAL_TOKEN",
            "token_type": "Bearer",
            "expires_in": 900,
            "scopes": ["admin", "messages:read", "messages:write", "messages:delete"] },
  "next_cursor": null, "warnings": [], "request_id": "req_01k4z2p8w3" }
```

**An admin session carries `admin` plus all three messaging scopes** (§9.7).
The owner presenting `AGENT_GM_ADMIN_SECRET` is by definition the person the
service belongs to, and a credential that could administer the server but not
read a message would be useless.

The secret goes **in the body**, never in a query string and never in a URL.
Failures are rate-limited hard (5 per 15 minutes per source, 20 globally, with
an exponential cooldown), so a script that retries a wrong secret in a loop
locks itself out rather than guessing.

`{"scopes": [...]}` narrows the session to a subset — `{"scopes":
["messages:read"]}` mints a read-only admin session, which is how a caller
tests its own scope handling. Asking for anything outside the four scopes is
`invalid_scope`.

`POST /v1/auth/refresh` rotates the refresh token. Reuse of a spent token
revokes the session. Its optional `scopes` may only narrow **relative to the
scopes the session was minted with**, which are recorded on the session row.

**A refresh may never widen** (§9.6). That includes widening *back* to the
full set after a narrowing: a session minted with all four scopes and refreshed
down to `messages:read` cannot refresh back up. Widening is `invalid_scope` and
**does not spend the presented token**. Getting the full set again means
presenting `AGENT_GM_ADMIN_SECRET` at `POST /v1/auth/admin-session` again.
Without that rule a narrowed session would be one refresh away from full
privilege, and narrowing would be decoration.

The two credential paths never cross: an OAuth refresh token at
`/v1/auth/refresh` is `invalid_token`, and an admin refresh token at
`/oauth/token` is `invalid_grant`. Neither attempt revokes anything.

## Choosing an account

Agent GM holds any number of paired Google accounts, and `account_id` — one
name on every surface — says which one a call is about. This is the rule
callers get wrong, so it is stated in full.

| Accounts paired | On a **read** | On a **write** |
|---|---|---|
| exactly one | may be omitted; defaults to it | may be omitted; defaults to it |
| more than one | may be omitted; covers **every** account | **must** be named |
| none | empty pages | `not_paired` |

A single-account deployment never has to think about accounts at all.

Reads and writes differ deliberately. "What came in today, across everything I
own" is a useful default for a listing and a dangerous one for a send: a send
that guessed an account would deliver a real message from an address the
caller did not choose. So a write with more than one account and no
`account_id` is refused — and the refusal carries the candidates, so the
caller can retry without a second round trip:

```json
{ "error": { "code": "invalid_request",
             "message": "account_id is required when more than one account exists.",
             "retryable": false,
             "details": { "field": "account_id",
                          "accounts": [
                            { "id": "acct_01k4z0aa", "google_account": "owner@example.test", "state": "connected" },
                            { "id": "acct_01k4z0bb", "google_account": "work@example.test", "state": "signed_out" } ] } },
  "request_id": "req_01k4z2p8w4" }
```

**A `conv_` or `msg_` ID already implies its account**, so `account_id` is not
required beside one, and is accepted for confirmation. The one write that
*must* name an account is `POST /v1/conversations`, whose target is a phone
number rather than an ID — nothing else in the request can imply the account.

An `acct_` ID that does not exist is `not_found`. A `conv_` or `msg_` ID
belonging to a *different* account than the `account_id` given is
`invalid_request` naming both — never `not_found`, which would suggest the
thread was gone.

Every DTO that can appear in a multi-account result carries `account_id`:
conversations, messages, contacts, attachments, operations and search results.
Uploads are the exception, and [Media](#media) says why.

## Idempotency

**Every mutation requires an idempotency key** (§6.3). Its two transports:

- the `Idempotency-Key` request header, or
- the `client_request_id` body field.

There is **no `?client_request_id=` query parameter on any route**; one
presented as a query parameter is `invalid_request` naming it, like any other
unknown parameter. The two `DELETE` routes that would otherwise have no body
accept a JSON body carrying it. Supplying both transports with *different*
values is `invalid_request`: that is a contradiction, not a preference. An
empty key, a key over 200 bytes, or a key containing control characters is
`invalid_request` naming `client_request_id`, and writes nothing.

Uniqueness is scoped to **(authorization, account, operation kind, key)**. Two
clients may use the same key value; one client may use one key for a send and
another for a mark-read.

**A replay returns the same operation and its `message_id`, and sends
nothing.** Sameness is decided by a SHA-256 over the canonically serialised
body, so reordered JSON keys are a replay and any changed value is not; a
changed body under the same key is `idempotency_conflict`.

**A fresh key is a different call, not a repeat.** This is the mistake that
sends a second text message to a real person. If a send times out, retry it
**with the same key**; do not mint a new one to "try again".

Keys are retained for `operations.idempotency_ttl`, 30 days by default. A
replay of a swept key is a new operation.

**The mirror hazard.** Because the account is part of the tuple, reusing a key
against a *different* `account_id` is not a replay — it would be a new
operation, and it would send a **second real message to a real person**. This
is the direction a caller reaches for when it "retries" a failed send by
switching accounts. So the API refuses it rather than obeying it: **a key
already used by this authorization for this kind against a different account is
`invalid_request`**, with `details.field = "client_request_id"` and the account
the key was first used with. A genuinely new send to another account uses a new
key.

## Pagination

Listings are newest-first. `limit` defaults to 50 and caps at 100.
`GET /v1/messages/{message_id}/context` is the exception: its `before` and
`after` each default to 5 and cap at 100.

`next_cursor` is **opaque and HMAC-signed**. It encodes `(sent_at_ms, id)`, so
paging is stable across equal timestamps, and it is **bound to the endpoint and
to the filter set as written**.

Bound *as written*, not as normalised. A cursor issued for
`/v1/conversations` with no `folder` is **not** valid when replayed with
`?folder=active`, even though the two select the same rows. Reusing a cursor
with different filters is `invalid_request`. A client that walks a listing
sends the same query string on every page anyway, so the rule costs nothing and
stops a page boundary from silently changing what is in scope — including which
accounts, since `account_id` is a filter like any other.

Do not parse a cursor. It is signed with the data key and its contents are not
a contract.

## Routes

Every `/v1` route, its scope, and every parameter it accepts. Anything not in
the parameters column is `invalid_request` naming it. Path segments in braces
are part of the address and are never accepted as query parameters.

The three tables are generated from `internal/api/routes.go`, which is the
inventory the server is built from; `internal/api/docs_test.go` fails if this
page and that file disagree in either direction.

### Health, auth, accounts and pairing

<!-- route-inventory:begin -->

| Method | Path | Scope | Parameters | Notes |
|---|---|---|---|---|
| `GET` | `/healthz` | `none` | — | Liveness. Never touches SQLite, so it answers while a migration runs |
| `GET` | `/v1/health` | `messages:read` | — | The server and every account. `status` describes the server, not the accounts |
| `POST` | `/v1/auth/admin-session` | `none` | body `secret`, `scopes` | Presents `AGENT_GM_ADMIN_SECRET` in the body. Mints `admin` plus the three messaging scopes; `scopes` may only narrow |
| `POST` | `/v1/auth/refresh` | `none` | body `refresh_token`, `scopes` | Rotates. `scopes` may only narrow relative to what the session was minted with; widening is `invalid_scope` and does not spend the token |
| `GET` | `/v1/auth/whoami` | `messages:read` | — | `authorization_id`, `kind`, `scopes`, `client_id`, `expires_at` |
| `POST` | `/v1/auth/logout` | `messages:read` | — | Ends a *token's* session. Nothing to do with signing a Google account out, which is why it is not called sign-out |
| `GET` | `/v1/accounts` | `messages:read` | — | Every account, whatever its state. `google_account` is served so the owner can tell accounts apart; it is never an ID and never in a URL |
| `GET` | `/v1/accounts/{account_id}` | `messages:read` | — | One account plus its `google`, `backfill`, `sweep` and `counters` blocks. The list route omits those four deliberately |
| `PATCH` | `/v1/accounts/{account_id}` | `admin` | body `label` | A human name for a listing. Nothing else is mutable |
| `GET` | `/v1/accounts/{account_id}/events` | `messages:read` | — | SSE, one event per state change plus a 30s heartbeat. The only streaming route; it carries no message data, so it needs no replay ring and no cursor |
| `GET` | `/v1/accounts/events` | `messages:read` | — | Every account's state changes, each tagged |
| `POST` | `/v1/accounts/{account_id}/reconnect` | `admin` | — | Forces a reconnect on that account |
| `POST` | `/v1/accounts/{account_id}/sign-out` | `admin` | body `confirm` | Shreds the session file, keeps every row. Requires `confirm: true` |
| `DELETE` | `/v1/accounts/{account_id}` | `admin` | body `confirm` | **The only route that deletes an account's data.** Requires `confirm: true`; returns the deleted row counts and the `effect` sentence |
| `POST` | `/v1/accounts/{account_id}/refresh-cookies` | `admin` | body `cookies` | Re-authenticates an existing pairing. A different Google address is `pairing_wrong_account` and changes nothing |
| `POST` | `/v1/pairing/start` | `admin` | body `cookies`, `device_index`, `account_id` | Adds an account, or resumes an existing one. There is one pairing flow, so there is no `method` field; a body carrying one is `invalid_request` naming it |
| `GET` | `/v1/pairing/{pairing_id}` | `admin` | — | Poll: `state` is `waiting`, `paired`, `failed` or `expired`, plus `account_id`, `emoji`, `error` |
| `DELETE` | `/v1/pairing/{pairing_id}` | `admin` | — | Abandons an in-flight pairing and leaves nothing behind |

<!-- route-inventory:end -->

Account `state` is `pairing`, `connected`, `degraded`, `error`, `signed_out`,
`parked` or `account_changed`. There is no server-level "unpaired" state: an
Agent GM with no accounts is a healthy Agent GM with no accounts.

Note the consequence of global scopes: `GET /v1/accounts` and
`GET /v1/health` are `messages:read` and both return every account's
`google_account`, so a `messages:read` token enumerates all of the owner's
Google addresses. That is what the Slice 3 authorization screen discloses.

### Reads

<!-- route-inventory:begin -->

| Method | Path | Scope | Parameters | Notes |
|---|---|---|---|---|
| `GET` | `/v1/conversations` | `messages:read` | query `account_id`, `query`, `participant`, `folder`, `type`, `unread_only`, `group_only`, `include_deleted`, `cursor`, `limit` | `folder` is `active`, `archived` or `spam_blocked`; `type` is `sms_mms` or `rcs` |
| `GET` | `/v1/conversations/{conversation_id}` | `messages:read` | — | The only route that populates `peer_typing_until`, which is in-memory live state and is always `null` in a list |
| `GET` | `/v1/conversations/{conversation_id}/messages` | `messages:read` | query `direction`, `sender`, `after`, `before`, `has_attachment`, `delivery_state`, `include_system`, `cursor`, `limit` | `direction` is `incoming` or `outgoing`; `after` and `before` are RFC 3339 |
| `GET` | `/v1/messages` | `messages:read` | query `account_id`, `conversation_id`, `direction`, `sender`, `after`, `before`, `has_attachment`, `delivery_state`, `include_system`, `cursor`, `limit` | The same filters across every account, or one with `account_id` |
| `GET` | `/v1/messages/{message_id}` | `messages:read` | — | One message, with its attachments and reactions |
| `GET` | `/v1/messages/{message_id}/context` | `messages:read` | query `before`, `after` | The messages around one message. Each side defaults to 5 and caps at 100 |
| `GET` | `/v1/messages/{message_id}/attachments` | `messages:read` | — | Metadata only. Folded into `get_message` on MCP, so it has no tool |
| `GET` | `/v1/search/messages` | `messages:read` | query `q`, `account_id`, `mode`, `conversation_id`, `sender`, `after`, `before`, `has_attachment`, `cursor`, `limit` | `q` is required. `mode` is `words` (default) or `exact` — a search vocabulary, not a switch between destructive behaviours |
| `GET` | `/v1/contacts` | `messages:read` | query `account_id`, `query`, `top`, `cursor`, `limit` | `top` restricts to frequent contacts |
| `GET` | `/v1/attachments/{attachment_id}` | `messages:read` | — | Metadata plus a download ticket |
| `GET` | `/v1/attachments/{attachment_id}/content` | `messages:read` | — | The bytes. An access token **or** a download ticket; the ticket is accepted only in the `Authorization` header, never as `?t=` |
| `GET` | `/v1/operations` | `messages:write` | query `account_id`, `kind`, `status`, `terminal`, `after`, `before`, `cursor`, `limit` | The caller's own operations. `messages:write` because that is the scope that creates them |
| `GET` | `/v1/operations/{operation_id}` | `messages:write` | — | Another authorization's ID answers `not_found` with a body byte-identical to a genuinely absent one |
| `GET` | `/v1/uploads/{upload_id}` | `messages:write` | — | The caller's own reservation |

<!-- route-inventory:end -->

`participant` accepts an E.164 number (`+12025550123`), the bare digits, a
national form, or a `part_` or `contact_` ID. `sender` accepts the same plus
the literal `me`.

**Matching a phone number across accounts.** A number is not
account-specific. With `account_id` it matches participants in that account
only; without it, in every account, and the results carry `account_id` so the
caller can tell them apart. A `part_` ID already belongs to one account, so
passing it with a *different* `account_id` is `invalid_request` naming both.
**`sender=me` means "whichever account's own participant"**, resolved per
account: on a cross-account query it returns everything the owner sent from
anywhere. It is never ambiguous and never an error.

**Search** returns `results[{message, rank, snippet, conversation}]` plus a
`coverage` block:

```json
"coverage": { "complete": false,
              "oldest_indexed_at": "2025-09-06T00:00:00.000Z",
              "conversations_pending": 3 }
```

While backfill is outstanding, `coverage.complete` is `false` and the response
carries a `history_incomplete` **warning** — not an error — so an empty result
during backfill is not read as an absent message.

### Writes, deletes and admin

<!-- route-inventory:begin -->

| Method | Path | Scope | Parameters | Notes |
|---|---|---|---|---|
| `POST` | `/v1/conversations` | `messages:write` | body `account_id`, `recipients`, `name`, `client_request_id` | The one write whose target is a phone number rather than an ID, so nothing else can imply the account. `name` is accepted only for two or more recipients. Zero recipients, or two that normalise to one number, is `invalid_request` **before** an operation row exists |
| `POST` | `/v1/conversations/{conversation_id}/messages` | `messages:write` | body `text`, `upload_ids`, `reply_to_message_id`, `force_rcs`, `client_request_id` | At least `text` or one upload. `upload_ids` is an array but currently accepts exactly one element; two is `invalid_request` naming the limit |
| `POST` | `/v1/conversations/{conversation_id}/typing` | `messages:write` | — | `204`. Fire-and-forget: no operation and no idempotency key, because it has no lasting effect |
| `POST` | `/v1/conversations/{conversation_id}/read` | `messages:write` | body `message_id`, `client_request_id` | Marks the conversation read through that message |
| `PATCH` | `/v1/conversations/{conversation_id}` | `messages:write` | body `folder`, `pinned`, `unread`, `client_request_id` | Archive, unarchive, pin, unpin and mark-unread. Returns `operation: null` and `changed: false` when the conversation is already in the requested state, and calls Google zero times |
| `POST` | `/v1/messages/{message_id}/reactions` | `messages:write` | body `emoji`, `client_request_id` | Adds, or switches when the owner already has a different reaction. `operation: null` when the owner already has exactly that one. `emoji` is canonicalised first |
| `DELETE` | `/v1/messages/{message_id}/reactions/{emoji}` | `messages:write` | body `client_request_id` | Removes. `operation: null` when there is nothing to remove. The path segment is canonicalised before matching, so `❤` and `❤️` address the same reaction |
| `DELETE` | `/v1/reactions/{reaction_id}` | `messages:write` | body `client_request_id` | The same removal by `react_` ID. Somebody else's reaction is `unsupported_capability` with reason `not_my_reaction` |
| `POST` | `/v1/uploads` | `messages:write` | body `filename`, `mime_type`, `size_bytes`, `sha256`, `client_request_id` | `201` with an upload ticket. `sha256` is optional and is verified if given |
| `DELETE` | `/v1/uploads/{upload_id}` | `messages:write` | — | `204`. Drops the reservation and its staged bytes |
| `PUT` | `/v1/uploads/{upload_id}/content` | `none` | — | Raw bytes, authenticated by the **upload token** rather than the access token. Redemption re-checks the issuing authorization's `messages:write` scope and revocation state |
| `DELETE` | `/v1/messages/{message_id}` | `messages:delete` | body `client_request_id` | Carries the `effect` sentence. There is no other delete and no delete option: a request carrying an `action` or `scope` switch is `invalid_request` naming it |
| `DELETE` | `/v1/conversations/{conversation_id}` | `messages:delete` | body `client_request_id` | Carries the `effect` sentence |
| `GET` | `/v1/admin/settings` | `admin` | — | Effective value, source (`default`, `environment` or `database`), mutability, restart requirement |
| `GET` | `/v1/admin/settings/{key}` | `admin` | — | One key, the same shape |
| `PATCH` | `/v1/admin/settings` | `admin` | — | Validates the **whole** body: any invalid key rejects the request and changes nothing. Its body fields are the settings keys themselves, validated against the settings registry rather than a fixed list |
| `POST` | `/v1/admin/backfill` | `admin` | body `account_id`, `conversation_id`, `confirm` | Re-opens backfill for one conversation, one account, or every account. With no body it is a full re-backfill of every account — the most expensive operation Agent GM offers — so it requires `confirm: true` |
| `POST` | `/v1/admin/backup` | `admin` | — | Writes a snapshot under the data directory. The caller does not choose the path |
| `GET` | `/v1/admin/audit` | `admin` | query `kind`, `kind_prefix`, `account_id`, `authorization_id`, `after`, `before`, `cursor`, `limit` | The audit trail. Rows for a removed account survive it |
| `GET` | `/v1/admin/diagnostics` | `admin` | query `account_id` | The **only** place raw Google values appear: `delivery_state_raw`, `operations.google_status_raw`, `CurrentSessionID`, the chosen gaia device, and the compiled and live `ConfigVersion` |
| `POST` | `/v1/admin/enrollment-codes` | `admin` | body `label`, `expires_in`, `scopes`, `allow_scopes` | Issues an enrollment code. Answers **200, not 201**: only the SHA-256 is stored, so there is no resource at a URL to point a `Location` at. `data.code` is the **only** time the value appears. `scopes` replaces the default ceiling `messages:read messages:write`, `allow_scopes` extends it, and the two are mutually exclusive. `expires_in` is a duration string or a number of seconds, 1m–24h, defaulting to `oauth.enrollment_default_ttl`. **`admin` can never be enrolled** |
| `GET` | `/v1/admin/enrollment-codes` | `admin` | — | Every code, without its value |
| `GET` | `/v1/admin/enrollment-codes/{enrollment_code_id}` | `admin` | — | One code, without its value |
| `DELETE` | `/v1/admin/enrollment-codes/{enrollment_code_id}` | `admin` | query `reason` | Revokes it. Repeating the call answers `200` with `revoked: false`: a second revocation is not a failure |
| `GET` | `/v1/admin/authorization-requests` | `admin` | query `status` | `pending`, `approved`, `denied` or `completed`. A pending request past its deadline reports `expired`, which is derived rather than stored |
| `GET` | `/v1/admin/authorization-requests/{authorization_request_id}` | `admin` | — | One request. It carries no secret: not the context handle, not the form token, not a code |
| `POST` | `/v1/admin/authorization-requests/{authorization_request_id}/approve` | `admin` | body `scopes` | `scopes` may only **narrow** the browser-selected set; widening or an empty list is `invalid_request`. A no-longer-pending request is `idempotency_conflict`; an expired one is `invalid_request` |
| `POST` | `/v1/admin/authorization-requests/{authorization_request_id}/deny` | `admin` | body `reason` | A denial is a decision the client is entitled to hear: the waiting page's completion redirects with `error=access_denied` rather than leaving it hanging |
| `GET` | `/v1/admin/authorizations` | `admin` | query `include_revoked` | Every credential this server has issued, admin bootstrap and OAuth alike. No token and no hash appears |
| `GET` | `/v1/admin/authorizations/{authorization_id}` | `admin` | — | One authorization |
| `DELETE` | `/v1/admin/authorizations/{authorization_id}` | `admin` | query `reason` | Revokes it and **every token of it**, so the client's next call is `401` and it cannot refresh its way back |
| `GET` | `/v1/admin/clients` | `admin` | — | Every dynamic registration. There is no `client_secret` anywhere: public native clients only |
| `GET` | `/v1/admin/clients/{client_id}` | `admin` | — | One registration |
| `DELETE` | `/v1/admin/clients/{client_id}` | `admin` | query `reason` | Removes the registration **and** revokes every authorization it holds. Removing the row alone would leave live tokens behind |

<!-- route-inventory:end -->

The two `messages:delete` routes are the whole of deletion. Both responses
carry an **`effect`** field holding an exact sentence:

> *"deletes this message from your Google Messages account only; the recipient
> keeps it"*

> *"deletes this conversation from your Google Messages account only; the
> other people in it keep it"*

The same string is the MCP tool description's closing sentence and the `agm`
confirmation prompt, so a model or a human cannot read a broader claim off one
surface than another.

The four `admin` families above — enrollment codes, authorization requests,
authorizations and clients — are the owner's half of the OAuth flow, and they
are described end to end in [oauth.md](oauth.md). None of them is reachable by
any token an agent can hold: `admin` is issued only by the admin bootstrap, is
never enrollable, and is `invalid_scope` at `/oauth/authorize`.

`PATCH /v1/admin/settings` is the one route whose body fields are not a fixed
list, so the parameters column above says `—` rather than enumerating them:
they are the settings keys of [operations.md](operations.md), and an unknown
key is still `invalid_request` naming it.

## DTOs

Every field of every object this API serves. Timestamps are RFC 3339 UTC with
millisecond precision.

### Account

`GET /v1/accounts` serves the first block only. `GET /v1/accounts/{account_id}`
adds the four blocks after it — the same per-account object `GET /v1/health`
embeds. The asymmetry is deliberate: a many-account listing stays small.

```json
{ "id": "acct_01k4z0aa",
  "google_account": "owner@example.test",
  "label": "personal",
  "state": "connected",
  "state_reason": null,
  "pairing_id": null,
  "phone_id": "phone_01k4z0ac",
  "phone_responding": true,
  "paired_at": "2026-09-01T11:02:44.000Z",
  "last_event_at": "2026-09-06T09:40:59.000Z",

  "google": { "config_version_live": "2026.9.2",
              "config_version_stale": false,
              "is_default_sms_app": true },
  "backfill": { "state": "complete",
                "conversations_done": 41,
                "conversations_total": 41,
                "completed_at": "2026-09-06T09:12:00.000Z" },
  "sweep": { "last_sweep_at": "2026-09-06T09:38:00.000Z",
             "sweeps_total": 12 },
  "counters": { "dropped_events": 0, "unknown_events": 0,
                "pending_operations": 0 } }
```

`google` is `null` for an account that is not `connected`:
`config_version_live` and `is_default_sms_app` are cached from that account's
last config fetch and refreshed on connect and every `ingest.sweep_interval`,
so `/v1/health` never blocks on a phone.

### Health

```json
{ "status": "ok",
  "version": "1.0.0", "commit": "abc1234",
  "source_url": "https://github.com/thisnick/agent-gm/tree/abc1234",
  "config_version_compiled": "2026.9.2",
  "upstream_commit": "be48a58",
  "accounts_summary": { "total": 2, "connected": 1, "degraded": 0,
                        "error": 0, "signed_out": 1,
                        "backfill_complete": 2 },
  "accounts": [ { "account_id": "acct_01k4z0aa", "…": "the account object above" } ],
  "pending_reprocess": null,
  "client_source": "100.64.0.7" }
```

**`status` describes the server, not the accounts.** It is `ok` whenever the
process is serving. An account in `signed_out` or `error` is a fact about that
account, reported in its row and in `accounts_summary`, and does not make Agent
GM unhealthy; a deployment with zero accounts is `ok`. Anything watching for
"is the service up" reads `status`. Anything watching for "can I send" reads
the account.

`client_source` is the source **this** request resolved to, which is how a
trusted-proxy misconfiguration becomes visible rather than silent
([operations.md](operations.md)).

### Conversation

```json
{ "id": "conv_01k4z2p8vq", "account_id": "acct_01k4z0aa",
  "name": "Alex", "is_group": false, "type": "rcs",
  "folder": "active", "unread": true, "pinned": false, "read_only": false,
  "participants": [ { "id": "part_01k4z2p8vr", "contact_id": "contact_01k4z2p8vs",
                      "display_name": "Alex", "phone": "+12025550123",
                      "is_me": false } ],
  "peer_typing_until": null,
  "is_deleted": false, "deleted_at": null,
  "last_activity_at": "2026-09-06T09:41:02.115Z",
  "latest_message_id": "msg_01k4z2p8vt",
  "capabilities": { "send_text": true, "send_media": true, "reply": true,
                    "react": true, "mark_read": true, "typing": true,
                    "force_rcs": true, "archive": true, "pin": true,
                    "delete_message": true, "delete_conversation": true },
  "created_at": "2026-09-01T11:04:00.000Z",
  "updated_at": "2026-09-06T09:41:02.115Z" }
```

`is_deleted` is true once Google's delete-for-me has been applied to the
thread, which is what `include_deleted` filters on. A deleted conversation
keeps its ID and its indexed history; only Google's copy is gone.

`peer_typing_until` is an instant or `null`. It is held in memory only, never
persisted, and is **always `null` in a list** — typing is per-conversation live
state, so only `GET /v1/conversations/{conversation_id}` populates it.

**`participants` are the people in a thread; `recipients` are the phone numbers
you address when creating one.** They are two different things, not two names
for one.

### Message

```json
{ "id": "msg_01k4z2p8vt", "account_id": "acct_01k4z0aa",
  "conversation_id": "conv_01k4z2p8vq",
  "kind": "message",
  "direction": "outgoing", "sender": { "id": "part_01k4z2p8vu", "is_me": true },
  "text": "on my way", "subject": null,
  "delivery": { "state": "delivered", "error": null,
                "updated_at": "2026-09-06T09:41:07.900Z" },
  "reply_to_message_id": null, "operation_id": "op_01k4z2p8vv",
  "attachments": [ { "id": "att_01k4z2p8vw", "mime_type": "image/jpeg",
                     "filename": "IMG_0421.jpg", "size": 184320,
                     "width": 1024, "height": 768,
                     "download_state": "available" } ],
  "reactions": [ { "id": "react_01k4z2p8vx", "emoji": "👍", "type": "like",
                   "participant_id": "part_01k4z2p8vr", "is_mine": false } ],
  "is_deleted": false,
  "sent_at": "2026-09-06T09:41:02.115Z" }
```

`delivery.state` is Agent GM's own closed vocabulary: `sending`, `sent`,
`delivered`, `read`, `failed`, `canceled`, `deleted`, `received`,
`downloading`, `download_failed`, `unknown`. It is not Google's enum, and
Google's enum is not served. A reaction whose emoji type has no unicode serves
`{"emoji": null, "type": "emotify"}` rather than being dropped.

`kind` is `message` or `system`. System events are excluded from listings
unless `include_system=true`.

### Operation

Returned inline by every mutation, and by `GET /v1/operations/{operation_id}`.
This is the only definition; every surface serves exactly these fields.

```json
{ "id": "op_01k4z2p8vv",
  "account_id": "acct_01k4z0aa",
  "kind": "send_text",
  "status": "succeeded",
  "terminal": true,
  "terminal_at": "2026-09-06T09:41:03.201Z",
  "corrected_at": null,
  "conversation_id": "conv_01k4z2p8vq",
  "message_id": "msg_01k4z2p8vt",
  "error": null,
  "created_at": "2026-09-06T09:41:02.980Z",
  "updated_at": "2026-09-06T09:41:03.201Z" }
```

- `kind` is one of `send_text`, `send_media`, `start_conversation`,
  `mark_read`, `add_reaction`, `remove_reaction`, `delete_message`,
  `delete_conversation`.
- `status` is `running`, `succeeded`, `pending`, `failed` or `unknown`. There
  is no `queued` and no `accepted`, because there is no queue: sends are
  synchronous.
- `terminal` is derived from `status` and is never stored independently.
  `terminal_at` is when the operation *first* became terminal and is never
  cleared. `corrected_at` is set when a late fact moves it out of `unknown`.
- `error` is `null` or the same `{code, message, retryable, details}` object
  the envelope carries.
- **A `pending` operation carries `error.code = "phone_not_responding"` with
  `retryable: true` and `terminal: false`.** That is how a caller tells "not
  yet" from "no". It is not a failure and the send must not be repeated.
- `message_id` is `null` until the remote echo lands — including on a
  `succeeded` send whose echo has not yet arrived.

### Contact

```json
{ "id": "contact_01k4z2p8vs", "account_id": "acct_01k4z0aa",
  "display_name": "Alex", "phone": "+12025550123",
  "is_top": true,
  "avatar_hash": "b1946ac92492d2347c6235b4d2611184",
  "updated_at": "2026-09-05T18:22:10.000Z" }
```

`account_id` is always present: the same person in two accounts is two contact
rows, and a cross-account list would otherwise be unattributable.
`avatar_hash` is a digest of the avatar bytes or `null`. There is no avatar
*content* route — Agent GM stores the hash so a caller can detect a change, not
the picture.

### Attachment

`GET /v1/attachments/{attachment_id}` returns metadata **and a download
ticket**:

```json
{ "attachment_id": "att_01k4z2p8vw",
  "account_id": "acct_01k4z0aa",
  "filename": "IMG_0421.jpg", "mime_type": "image/jpeg",
  "size": 184320,
  "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  "sha256_available": true,
  "width": 1024, "height": 768,
  "download_state": "available",
  "inline": false,
  "resource_uri": "agm://attachments/att_01k4z2p8vw",
  "download_url": "https://gm.example.test/v1/attachments/att_01k4z2p8vw/content",
  "token": "agm_dt_EXAMPLE_NOT_A_REAL_TOKEN",
  "token_audience": "download:att_01k4z2p8vw",
  "expires_at": "2026-09-06T10:11:07.000Z",
  "max_redemptions": 5,
  "curl": "curl --fail -H 'Authorization: Bearer agm_dt_…' -o 'IMG_0421.jpg' 'https://gm.example.test/v1/attachments/att_01k4z2p8vw/content'" }
```

`sha256` is the digest of the **decrypted** bytes, computed on demand and
cached, because Google carries a digest of the ciphertext — which is not what
an agent comparing its downloaded copy would compute. When it cannot be
computed, `sha256` is `null`, `sha256_available` is `false`,
`sha256_unavailable_reason` is one of `larger_than_cache_budget`,
`download_budget_exhausted` or `bytes_unavailable`, and the reason also appears
in `warnings`.

### Computed fields, and fields that are never served

Two public fields are **computed rather than stored**, because the raw values
are Google protocol internals that mean nothing to a caller:

- **the conversation's `type`** — `sms_mms`, `rcs` or `unknown`. Google's own
  SMS conversation carries MMS too, which is why the public value names both.
  (§4.6 calls this derived value `conversation_type`; the conversation DTO and
  the `type` filter spell it `type`.)
- **`capabilities.force_rcs`** — true exactly when the conversation is `rcs`
  *and* its send mode is automatic. A caller that wants to know whether
  `force_rcs` will be accepted reads the capability, never a mode.

Three raw values are **deliberately never served on this surface**:
`send_mode_raw`, `delivery_state_raw` and `google_status_raw`. They exist in
the database and they appear on `GET /v1/admin/diagnostics`, which is `admin`,
and nowhere else. The only raw Google integers on a non-admin surface are
`details.google_type` and `details.status` on an error, which are diagnostic
values with no Agent GM meaning and are never IDs.

## Error codes

| Code | HTTP | Retryable | Meaning |
|---|---|---|---|
| `invalid_request` | 400 | no | Malformed, unknown parameter or field, wrong ID prefix, contradictory idempotency key |
| `invalid_token` | 401 | no | Absent, expired, unknown or wrong-audience bearer |
| `insufficient_scope` | 403 | no | Valid token, wrong scope |
| `not_found` | 404 | no | No such object. Byte-identical whether it never existed or the caller may not see it |
| `idempotency_conflict` | 409 | no | Same key, different body |
| `not_paired` | 409 | no | **There is no Google Messages session at all.** Not used for a paired-but-unusable conversation — that is `unsupported_capability` |
| `pairing_no_cookies` | 409 | no | Pairing attempted without cookies |
| `pairing_no_devices` | 409 | no | The account has no primary device |
| `pairing_wrong_emoji` | 409 | no | The owner tapped the wrong emoji |
| `pairing_cancelled` | 409 | no | Dismissed, or "this is not me" |
| `pairing_timeout` | 409 | no | No response within the window |
| `pairing_init_timeout` | 409 | **yes** | The 20-second initial round trip elapsed. Carries `details.multiple_devices` when the account had more than one candidate; there is no device count |
| `pairing_wrong_account` | 409 | no | A cookie refresh whose Google account differs from the paired one |
| `pairing_no_account` | 409 | no | The pairing completed but Google returned no account address, so there is nothing to derive an `acct_` ID from. Nothing is created |
| `unsupported_capability` | 409 | no | The action cannot apply here; `details.reason` from the closed list below |
| `payload_too_large` | 413 | no | Body over 1 MiB, or media over `media.upload_max_bytes` |
| `media_unsupported_type` | 415 | no | A MIME type Google Messages has no media type for |
| `rate_limited` | 429 | **yes** | With `Retry-After` |
| `internal_error` | 500 | **yes** | A bug |
| `not_default_sms_app` | 502 | no | The phone is not the default SMS app |
| `google_undocumented_status` | 502 | no | A Google enum value the pinned protocol has no name for. `details.status` is the bare integer; no meaning is claimed |
| `google_error` | 502 | **maybe** | A Google-side error; `details.google_type` (integer) and `details.google_message`. Only the status behind it knows whether a retry can help |
| `google_http_error` | 502 | **yes** | Transport level; `details.status` |
| `google_permission_denied` | 502 | no | Google refused the caller |
| `disconnected` | 503 | **yes** | The long poll is down |
| `phone_not_responding` | 504 | **yes** | **The operation is `pending`, not failed — do not resend.** |

A body over 1 MiB is `413` with `payload_too_large`; a body that fails to read
for any other reason is `400`, because "too large" would be a guess.

Each code maps onto exactly one `agm` exit code; the mapping is in
[cli.md](cli.md) and is generated from the same table.

### `unsupported_capability` reasons

A **closed** vocabulary in `details.reason`. The answer also carries the object
ID, the action, and the capability's current value.

| Reason | Meaning |
|---|---|
| `not_signed_in` | The account this touches is `signed_out`, `error`, `parked` or `account_changed` — every state that reads but does not write. Its history stays readable; only writes are refused. Read `state` and `state_reason` to tell "waiting for a slot" from the rest |
| `conversation_read_only` | The conversation is read-only |
| `conversation_deleted` | Delete-for-me has been applied locally |
| `not_my_message` | Deleting a message the owner did not send |
| `not_my_reaction` | Removing somebody else's reaction |
| `reply_not_supported` | `reply_to_message_id` on an SMS/MMS conversation; replies are RCS-only |
| `rcs_not_available` | `force_rcs` where `capabilities.force_rcs` is false |
| `media_pending` | The attachment's bytes are not downloaded yet |

These are emitted **before** any operation row exists, so a refused action
never leaves a record that looks like an attempt. The order is part of the
contract: resolve the object, check the capability, validate the request,
*then* create the operation.

**`not_paired` is not in this table.** Having no accounts at all is a
service-level condition and is the top-level `not_paired` code. Having an
account that is merely unusable right now is `unsupported_capability` with
`not_signed_in`, because the conversation exists, is readable, and will be
writable again after a re-pair. The two are never interchangeable.

## Media

An MCP client cannot attach a file and base64 through a model is not
acceptable, so bytes move by `curl`, with a ticket.

**`upload_url` and `download_url` are built from `AGENT_GM_PUBLIC_URL` and
never from the request's `Host` header.** An agent in a sandbox on another
machine therefore gets the origin the tunnel exposes, not the one it happened
to dial.

### Upload — three requests

**1. Reserve.**

```console
$ curl -s -X POST https://gm.example.test/v1/uploads \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"filename":"IMG_0421.jpg","mime_type":"image/jpeg","size_bytes":184320,
         "client_request_id":"01k4z2p8w5"}'
```

```json
{ "upload_id": "upl_01k4z2p8w6",
  "upload_url": "https://gm.example.test/v1/uploads/upl_01k4z2p8w6/content",
  "method": "PUT",
  "token": "agm_ut_EXAMPLE_NOT_A_REAL_TOKEN",
  "token_audience": "upload:upl_01k4z2p8w6",
  "expires_at": "2026-09-06T12:11:07.000Z",
  "limits": { "max_bytes": 104857600, "size_bytes": 184320,
              "mime_type": "image/jpeg", "sha256": null,
              "expires_in_seconds": 7200 },
  "curl": "curl --fail -X PUT -H 'Authorization: Bearer agm_ut_…' -H 'Content-Type: image/jpeg' --data-binary @FILE 'https://gm.example.test/v1/uploads/upl_01k4z2p8w6/content'" }
```

`mime_type` is validated at reservation time; an unsupported type is
`media_unsupported_type` and reserves nothing. **One attachment per message.**

**2. `PUT` the bytes**, with the upload token — not the access token:

```console
$ curl --fail -X PUT \
    -H 'Authorization: Bearer agm_ut_EXAMPLE_NOT_A_REAL_TOKEN' \
    -H 'Content-Type: image/jpeg' \
    --data-binary @IMG_0421.jpg \
    'https://gm.example.test/v1/uploads/upl_01k4z2p8w6/content'
```

The stream is refused the moment it exceeds the reserved length. Completion
verifies the byte count, the declared digest and the detected content type.
**A reservation that fails verification is spent: reserve again rather than
retry the `PUT`.**

**3. Send it**, naming the upload:

```console
$ curl -s -X POST https://gm.example.test/v1/conversations/conv_01k4z2p8vq/messages \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"upload_ids":["upl_01k4z2p8w6"],"text":"the photo",
         "client_request_id":"01k4z2p8w7"}'
```

Only the authorization that owns an upload may send it, and an upload can be
sent **once**. Naming it twice is refused; naming another authorization's is
`not_found`.

**Uploads are deliberately account-agnostic.** An `upl_` reservation carries no
`account_id`: it is bytes staged by an authorization, and **the account is
fixed at send time** by the conversation named in the send. The same upload may
therefore go into any account the caller may write to — but only once, so it
cannot be fanned out — which is why the "every multi-account DTO carries
`account_id`" rule does not apply to it.

### Download

`GET /v1/attachments/{attachment_id}` hands back the metadata and a download
ticket shown in [Attachment](#attachment) above; `GET
/v1/attachments/{attachment_id}/content` serves the bytes for either a
`messages:read` access token or that ticket.

Bytes are served with `Content-Disposition: attachment`,
`X-Content-Type-Options: nosniff` and a sandboxing CSP. **SVG, HTML and every
other unrecognised type are served as `application/octet-stream`, never with
their own type.**

An attachment whose full-size bytes are still being fetched is
`unsupported_capability` with `reason: "media_pending"`.

### Ticket rules

| Thing | Value |
|---|---|
| Upload reservation and token life | 2 hours |
| Upload token redemptions | 1 |
| Download token life | 15 minutes |
| Download token redemptions | 5 |
| Token prefixes | `agm_ut_` upload, `agm_dt_` download |

- Tokens are typed to **one audience** — `upload:<upload_id>` or
  `download:<attachment_id>` — and the audience is part of the redemption
  rather than a check after it. **A token presented at the wrong URL is refused
  and not spent**, so learning a token value does not let anyone destroy it.
- A token is accepted **only in the `Authorization` header**. There is no `?t=`
  form, so it cannot be captured from a proxy log or a browser history.
- **Every redemption re-checks the issuing authorization's scope and
  revocation state** — `messages:write` for an upload, `messages:read` for a
  download. A ticket outlives neither, so revoking an authorization
  immediately kills every ticket it minted.
- Every refusal is the same message whatever the reason, so a status code
  teaches an attacker nothing about which guesses were once valid.
- `client_request_id` makes a reservation idempotent, but **each attempt
  returns a fresh token**: the first token's value left the process and cannot
  be recovered. A token minted on a repeat never outlives the reservation it
  fills, which is what `limits.expires_in_seconds` counts down to.

## Rate limits

| Surface | Limit |
|---|---|
| Reads (`messages:read`) | 300 req/min per authorization, burst 100 |
| Mutations (`messages:write`, `messages:delete`) | 120 req/min per authorization, burst 30 |
| `/v1/admin/*` | 120 req/min per authorization, burst 30 |
| Admin-secret failures | 5 / 15 min per source, 20 / 15 min globally, exponential cooldown |
| Concurrent uploads | 4 per authorization, 16 globally |
| Concurrent downloads | 8 per authorization, 32 globally |

Exceeding one is `rate_limited` with `Retry-After`.

**Holding several scopes does not multiply the allowance, and neither does
holding several accounts.** The buckets are per authorization and per source,
never per account. One authorization sending to three accounts shares one
120/min mutation budget, so adding an account divides the per-account send rate
rather than adding to it. That is deliberate — the limits protect the server
and the phones from a runaway agent, and an agent is no less runaway for
spreading itself across accounts.

The remaining limits, and how a request's source is resolved behind a proxy,
are in [operations.md](operations.md).
