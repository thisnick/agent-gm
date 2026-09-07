# MCP

Serves spec §8. The endpoint is `AGENT_GM_PUBLIC_URL` plus `/mcp` —
`https://gm.example.test/mcp` in the examples below — over streamable HTTP,
protocol revision `2026-07-28`, with `2025-11-25` accepted for compatibility.
The protocol layer is the official MCP Go SDK, at a pinned version:
[The SDK underneath](#the-sdk-underneath) says which behaviours on this page
are therefore the SDK's.

Every tool here is a facade over the REST route that serves the same data
([api.md](api.md)). The filters, the validation, the error codes and the DTOs
are the same on both surfaces **by construction**: a tool call runs the very
same handler a `/v1` request runs, through the same strict parameter checks.
So anything this page says about a filter is also true of the route, and a fix
to one is a fix to both.

Authorization is OAuth 2.1 ([oauth.md](oauth.md)).

The section below, "First five minutes", is the `instructions` block the
server returns from `initialize`, byte for byte. A test asserts the two are
identical, so a client that surfaces instructions to its model has already
told it this page.

## First five minutes

**This server is one person's Google Messages — possibly more than one
account of it.** Each account is paired directly with an Android phone, the
way the Google Messages web client pairs. Everything you can see here, they
can see in the Messages app on that phone, and everything you send leaves as
a real SMS, MMS or RCS message to a real person. There is no sandbox and no
undo.

**Accounts.** Call `list_accounts` first if you do not already know which
account you are working in. Each has an ID starting `acct_`, the Google
address it belongs to, a label the owner chose, and a `state`. **If there is
exactly one account you can leave `account_id` out of every call and it will
be used.** If there is more than one, reads without `account_id` cover all of
them, but **a write must name one** — omit it and you get `invalid_request`
listing the accounts to choose from, which you can retry against
immediately. A `conv_` or `msg_` ID already belongs to one account, so you
never need to pass both.

An account whose `state` is not `connected` is still fully readable — its
history is here — but writes to it are refused with
`unsupported_capability` and `reason: "not_signed_in"`. That means the owner
signed it out or its credentials expired; only they can fix it, and no
amount of retrying will.

**The thing you address is a conversation.** A conversation is a Google
Messages thread: one other person, or a group. It has an ID that starts
`conv_`. Messages in it have IDs that start `msg_`. Every ID here is an
Agent GM ID with a typed prefix — `conv_` conversations, `msg_` messages,
`att_` attachments, `react_` reactions, `part_` people in a thread,
`contact_` contacts, `upl_` uploads, `op_` operations. Google's own IDs are
not accepted in their place.

Words mean what they mean in Google Messages. A *conversation* is a thread.
A *contact* is somebody in the phone's contact list. *RCS* is the modern
protocol; *SMS/MMS* is the fallback. *Delete* means delete from this
account only — the other person keeps their copy, always.

1. **Find the conversation.** `list_conversations` with `participant` set
   to a phone number (`+15105550123`, the bare digits, or a national form)
   returns the threads that number is in, newest activity first. Add
   `account_id` to look in one account, or leave it out to look in all of
   them — results carry `account_id` either way. With only a name, use
   `query`, a substring match over the thread name and every participant's
   name and number. `sender: "me"` means the owner in whichever account a
   message belongs to, so it works across accounts as well as within one.
   `list_contacts` maps names to
   numbers.
2. **Read it.** `list_messages` with that `conversation_id`. Newest first,
   so the first item is the latest message. Pass the result's `next_cursor`
   back as `cursor` for the next page; page size defaults to 50 and caps at
   100. `search_messages` needs `q`; it searches **every** account unless
   you pass `account_id`.
3. **Reply.** `send_message` with the `conversation_id` and `text`.
   Add `reply_to_message_id` to thread a
   reply — **but replies are an RCS feature; on an `sms_mms` conversation
   that argument is refused with `unsupported_capability` and
   `reason: "reply_not_supported"`.** Check the conversation's `type` first,
   or read `capabilities.reply`.
4. **Know whether it arrived.** The result carries a `message_id` and an
   `operation`. Then watch the message's `delivery.state`, which walks
   `sending → sent → delivered → read`. **On SMS it usually stops
   at `sent`, and on group threads it usually stops at `sent`. Delivery and
   read receipts are an RCS feature and a carrier feature; waiting for
   `delivered` on an SMS thread can wait forever.** `sent` means the
   carrier took it, and that is as much as SMS will ever tell you.
5. **Send a photo or a file.** `create_upload` with the filename, mime type
   and byte length; run the `curl` command it returns, with `FILE` replaced
   by the path; then `send_message` with `upload_ids: ["upl_…"]` and
   optionally `text` as a caption. An upload is **not** tied to an account —
   the conversation you send it into decides that — but it can be sent only
   once. You cannot attach a file through this protocol any other way, and
   base64 through the model is not acceptable.
6. **Start a new thread.** `start_conversation` with `recipients` as E.164
   phone numbers. One recipient is a direct chat; two or more is a group,
   and `name` is only accepted for a group. **If a thread with exactly
   those recipients already exists you get that thread back and nothing is
   sent** — starting is safe, sending is not.
7. **React, or take something back.** `add_reaction` with `emoji` set to a
   bare emoji — Google offers eleven (👍 😍 😂 😮 😥 😠 👎 🤔 😢 😡 ❤️) and
   anything else is sent as a custom reaction that may not render on the
   recipient's phone. One reaction per person per message: adding a second
   replaces the first. `remove_reaction` takes the same `emoji`, or the
   `reaction_id` you read. `delete_message` and
   `delete_conversation` delete from **this account only** — the recipient
   keeps their copy. There is no delete-for-everyone and no mode to choose.

**There is no idempotency key to invent.** Every write returns an
`operation` with an id, and status is checked by that id with
`get_operation`. **If a call's result is lost — a timeout, a dropped
connection, a tool error you cannot read — do not send it again. Look
first:** `list_messages` on the conversation, or `list_operations`, will
tell you whether it went. Sending again because you did not see an answer
is how a person gets the same text twice. If a send times out with
`phone_not_responding`, the operation is `pending`, not failed: the server
accepted it and the phone may still send it when it wakes. **Poll
`get_operation`; do not resend.**

The phone has to be awake and online for anything to happen.
`list_accounts` and `get_session` tell you whether it is: `state` and
`phone_responding`, per account. `get_health` tells
you whether the index is complete (`backfill`) and whether the two things
that break sending are right (`is_default_sms_app`,
`config_version_stale`). If `state` is not `connected`, reads still work
from the local index but writes will fail, and only the owner can fix it.

A call that is refused comes back as an ordinary result with
`isError: true` and
`structuredContent.error = {code, message, retryable, details}`. Read it
and correct the call rather than repeating it. `not_found` means no such
object. `invalid_request` names the parameter you got wrong in
`details.parameter` or `details.field` — this server refuses a misspelled
filter rather than silently ignoring it. `unsupported_capability` means the
action cannot apply here and `details.reason` says why.
`phone_not_responding` and `rate_limited` are retryable; almost nothing
else is.

Scopes: `messages:read` gives you `list_accounts`, `list_conversations`,
`get_conversation`, `list_messages`, `get_message`, `message_context`,
`search_messages`, `get_attachment`, `list_contacts`, `get_session` and
`get_health`. A scope covers **every** account this server holds; there is
no per-account permission.
`messages:write` adds `send_message`, `start_conversation`, `mark_read`,
`add_reaction`, `remove_reaction`, `update_conversation`, `create_upload`
and `get_operation`. `messages:delete` adds `delete_message` and
`delete_conversation`. You only see the tools your token allows.

## Transport

`POST /mcp` only, served by the SDK's `StreamableHTTPHandler` in its
**stateless** mode. There is no server-initiated stream to open and no session
to delete: every request carries its own bearer token and its own protocol
metadata, and durable state lives in SQLite. `GET` and `DELETE` on `/mcp` are
`405` with `Allow: POST` **once the caller is authenticated**, and `401`
before — authentication comes first, so an anonymous request never learns
which methods exist.

Stateless is not a preference. The SDK refuses protocol `2026-07-28` on a
stateful transport, because that revision is sessionless by design and drops
resumability, so sessions and this revision cannot both be had.

The checks, in this order. The first two run **before the transport parses
anything**; the last two are the SDK's own and run after authentication,
because the SDK owns the parse.

| Check | Refusal |
|---|---|
| `Origin`, when present, must equal `AGENT_GM_PUBLIC_URL`. A non-browser client sending none is supported | `403` |
| The body is at most 1 MiB | `413` |
| Exactly one `Authorization` header, and only the `Bearer` scheme. Two headers, another scheme, or a value carrying two tokens are refused rather than resolved to whichever happens to be first | `401` |
| The token carries at least one messaging scope | `401` with no token; `403 insufficient_scope` for a valid token that carries none |
| At most 8 requests in flight per authorization and 32 across the process | `429` with `Retry-After` |
| `Content-Type` is `application/json` | `415` |
| `Accept` admits **both** `application/json` and `text/event-stream` | `400` |

`Accept` is *both* rather than *either* because the server chooses which of the
two to answer with, so a client admitting only one is refusing an answer the
server is entitled to give. A refusal at any of these carries the §7.1 error
envelope, on this surface as on every other.

The `401` and the `403` are deliberately distinct. A client that retried the
authorization flow on a `403` would loop for ever, and one that gave up on a
`401` would never authorize at all. Both carry the same challenge:

```http
WWW-Authenticate: Bearer resource_metadata="https://gm.example.test/.well-known/oauth-protected-resource/mcp", scope="messages:read messages:write"
```

That is one line. It is printed unwrapped because it is sent unwrapped: HTTP
line folding is obsolete, and a reader who copied a folded version would have a
header no server sends.

There is no `realm` parameter and no `error` parameter. That is the exact
string the SDK's bearer middleware emits, and the SDK owns the format here. The
`/v1` challenge of §7.2 is a different string and still carries both — see
[api.md](api.md) — so a client that parses one must not assume the other.

`serverInfo` carries `name: "agent-gm"` and `version`: the release version with
the built commit as **semver build metadata**, `0.1.0+abc1234`. The source URL
is `serverInfo.websiteUrl`. That pair is the AGPL §13 obligation of spec §1.4,
not a nicety: a deployment reachable over a network must offer its
corresponding source.

There is no `serverInfo.commit` and no `serverInfo.source_url`. The SDK's
`serverInfo` type has a field for neither, and a build fact placed in `_meta`
does not reach a reference client, which keeps exactly one `_meta` key of the
`server/discover` result and discards the rest. A licence obligation no client
can read is not met, so it goes where a client reads. The `_meta` keys
`app.agent-gm/commit` and `app.agent-gm/source_url` are written on both
handshakes as well, for a caller reading the wire directly.

## The SDK underneath

The protocol layer is the official MCP Go SDK,
`github.com/modelcontextprotocol/go-sdk`, pinned in `go.mod` at **v1.7.0**.
Nothing here reimplements the API: every tool call still runs the REST handler.

**What the SDK decides** — so when you read these on this page you are reading
its behaviour, not a choice made here: JSON-RPC framing, error objects and
their codes; protocol-version negotiation, including revisions older than the
two §8.1 names; the `tools/list` and `tools/call` wire shapes;
`resources/list`, `resources/templates/list` and `resources/read`; the bearer
middleware and the format of the `WWW-Authenticate` challenge above; the RFC
9728 protected-resource document's handler, which is why that document carries
CORS headers and answers an `OPTIONS` preflight ([oauth.md](oauth.md)); and
every method this server does not implement but the SDK does — a client asking
for one gets the SDK's answer rather than a `-32601` chosen here.

**What is not the SDK's:** the twenty-one tools and every schema, description
and enum in them; the translation onto the REST routes, so that both surfaces
answer from one handler; the `isError` envelope; scope gating; the `Origin`
check; the single-`Authorization`-header rule; the "at least one messaging
scope" rule; the concurrency budget; the §7.1 envelope on every refusal; and
the whole authorization server of [oauth.md](oauth.md).

**The pin is bumped the way the `libgm` pin of spec §3.6 is bumped: as its own
deliberate slice, never as a drive-by commit.** A bump re-runs the MCP
acceptance tests, re-baselines `devbox run conformance` in both directions, and
drives a real server with the SDK's own client and with the official TypeScript
client. It does not need §3.6's live gate, because nothing here touches Google.
For an operator the consequence is narrow and worth knowing: a status code, a
header or an error code on this page can change under you when that pin moves,
and nothing else can. If a connector stops working after an upgrade, the pinned
version is the first thing to read.

## Scopes and what you see

| Scope | Tools |
|---|---|
| `messages:read` | the eleven reads |
| `messages:write` | eight more: the seven writes plus `get_operation` |
| `messages:delete` | `delete_message` and `delete_conversation`, and nothing else |
| `admin` | **no tools at all.** It is never enrollable and never reaches this surface |

`tools/list` returns **only** the tools your authorization may use, so a model
is never invited to attempt something that will be refused. `tools/call`
checks again: visibility is not authorization, and a client may call a name it
learned elsewhere. A refusal at call time is an ordinary result with
`isError: true` and `details.required_scope`, because that is something the
model can act on by choosing a different tool.

Scopes are global across accounts. A token holding `messages:read` reads every
account this server holds; one holding `messages:write` can send from any of
them. There is no per-account permission, and the authorization screen says so
before you approve it.

## Accounts

**`list_accounts` is the call to make first** when you do not already know
which account you are working in. It returns every Google account this server
holds, each with an `acct_` ID, the Google address it belongs to, the label
the owner chose, and a `state`.

The rule is the same on every surface (spec §7.3):

- **Exactly one account** → `account_id` may be omitted everywhere and
  defaults to it. A single-account deployment never has to think about
  accounts at all.
- **More than one** → a **write** must name one. Omitting it is
  `invalid_request` with `details.field = "account_id"` and
  `details.accounts` listing the candidates as `{id, google_account, state}`,
  so you can retry immediately without another round trip. A **read** may
  still omit it and then covers every account; every row it returns carries
  its own `account_id`.
- **Zero accounts** → a write is `not_paired`; reads return empty pages.

A `conv_` or `msg_` ID already belongs to one account, so you never pass both.

An account whose `state` is not `connected` is still fully readable — its
history is here — but writes to it are refused with `unsupported_capability`
and `reason: "not_signed_in"`. Only the owner can fix that, and no amount of
retrying will.

`get_session` reads one account in detail, and `get_health` reports every
account at once together with the two settings that break sending,
`is_default_sms_app` and `config_version_stale`.

## The tool catalogue

Twenty-one tools: eleven reads, eight writes, two deletes.

### Reads — `messages:read`

| Tool | REST route | Arguments |
|---|---|---|
| `list_conversations` | `GET /v1/conversations` | `account_id`, `query`, `participant`, `folder`, `type`, `unread_only`, `group_only`, `include_deleted`, `cursor`, `limit` |
| `get_conversation` | `GET /v1/conversations/{conversation_id}` | `conversation_id` **(required)** |
| `list_messages` | `GET /v1/messages` | `account_id`, `conversation_id`, `cursor`, `limit`, `direction`, `sender`, `after`, `before`, `has_attachment`, `delivery_state`, `include_system` |
| `get_message` | `GET /v1/messages/{message_id}` | `message_id` **(required)** |
| `message_context` | `GET /v1/messages/{message_id}/context` | `message_id` **(required)**, `before`, `after` |
| `search_messages` | `GET /v1/search/messages` | `q` **(required)**, `account_id`, `mode`, `conversation_id`, `sender`, `after`, `before`, `has_attachment`, `cursor`, `limit` |
| `get_attachment` | `GET /v1/attachments/{attachment_id}` | `attachment_id` **(required)** |
| `list_contacts` | `GET /v1/contacts` | `account_id`, `query`, `top`, `cursor`, `limit` |
| `get_session` | `GET /v1/accounts/{account_id}` | `account_id` |
| `get_health` | `GET /v1/health` | — |
| `list_accounts` | `GET /v1/accounts` | — |

### Writes — `messages:write`

| Tool | REST route | Arguments |
|---|---|---|
| `send_message` | `POST /v1/conversations/{conversation_id}/messages` | `conversation_id` **(required)**, `text`, `upload_ids`, `reply_to_message_id`, `force_rcs` |
| `start_conversation` | `POST /v1/conversations` | `account_id`, `recipients` **(required)**, `name` |
| `mark_read` | `POST /v1/conversations/{conversation_id}/read` | `conversation_id` **(required)**, `message_id` |
| `add_reaction` | `POST /v1/messages/{message_id}/reactions` | `message_id` **(required)**, `emoji` **(required)** |
| `remove_reaction` | `DELETE /v1/messages/{message_id}/reactions/{emoji}` or `DELETE /v1/reactions/{reaction_id}` | `reaction_id`, `message_id`, `emoji` |
| `update_conversation` | `PATCH /v1/conversations/{conversation_id}` | `conversation_id` **(required)**, `folder`, `pinned`, `unread` |
| `create_upload` | `POST /v1/uploads` | `filename`, `mime_type` **(required)**, `size_bytes` **(required)**, `sha256` |
| `get_operation` | `GET /v1/operations/{operation_id}` | `operation_id` **(required)** |

`get_operation` is a read, but it is gated on `messages:write` because it
exposes only operations the caller created, and `messages:write` is the scope
that creates them. Visibility and read-ness are different questions, which is
why its annotations still say `readOnlyHint: true`.

`remove_reaction` takes either `reaction_id`, or `message_id` together with
`emoji`. Supplying neither is `invalid_request`: there is nothing to remove
and nothing to infer.

### Deletes — `messages:delete`

| Tool | REST route | Arguments |
|---|---|---|
| `delete_message` | `DELETE /v1/messages/{message_id}` | `message_id` **(required)** |
| `delete_conversation` | `DELETE /v1/conversations/{conversation_id}` | `conversation_id` **(required)** |

Both deletes are **delete-for-me**. The other person keeps their copy, always.
There is no delete-for-everyone and nothing to choose.

## Input schemas

- Every input schema is **closed** — `additionalProperties: false` — and it is
  enforced by the decoder rejecting unknown fields, not merely declared. An
  invented argument name comes back named, in a result you can read.
- **Every argument carries a description.** The claim is measurable: fetch
  `tools/list`, count the arguments, count the descriptions, and they are
  equal. A test does exactly that against a running server, so one stale
  description fails the build.
- Closed vocabularies — `folder`, `type`, `direction`, `delivery_state` and
  search's `mode` — are real JSON Schema `enum`s with a `oneOf` of `const`s,
  because `oneOf` is the only place JSON Schema lets a per-value description
  live. Where the vocabulary is an optional **filter**, `null` is in the enum
  too: an enum that omitted it would make not filtering a schema violation.
- Every conversation argument is `conversation_id` and takes a `conv_` ID;
  every message argument is `message_id` and takes a `msg_` ID. Google's own
  IDs are not accepted in their place.
- Every tool declares an `outputSchema` describing the envelope it really
  returns, with `data` typed by that tool's own DTO.

No write tool takes an idempotency key of any spelling: the server mints the
operation id and returns it. Every write tool's description ends with the same
sentence, byte for byte:

> The result carries an operation id; check status by that id. If a call's
> result is lost, check the conversation or list operations before sending
> again.

## Annotations

Accurate rather than conventional. `false` is a claim, so every hint is
serialised rather than omitted.

| Tool | `readOnlyHint` | `destructiveHint` | `idempotentHint` | `openWorldHint` |
|---|---|---|---|---|
| `list_conversations` | true | false | true | false |
| `get_conversation` | true | false | true | false |
| `list_messages` | true | false | true | false |
| `get_message` | true | false | true | false |
| `message_context` | true | false | true | false |
| `search_messages` | true | false | true | false |
| `get_attachment` | true | false | true | false |
| `list_contacts` | true | false | true | false |
| `get_session` | true | false | true | false |
| `get_health` | true | false | true | false |
| `list_accounts` | true | false | true | false |
| `send_message` | false | false | true | true |
| `start_conversation` | false | false | true | true |
| `mark_read` | false | false | true | true |
| `add_reaction` | false | false | true | true |
| `remove_reaction` | false | true | true | true |
| `update_conversation` | false | false | true | false |
| `create_upload` | false | false | true | false |
| `get_operation` | true | false | true | false |
| `delete_message` | false | true | true | false |
| `delete_conversation` | false | true | true | false |

The two deletes are `openWorldHint: false` because Google's delete is
delete-for-me: it changes the owner's own copy and nothing leaves the
building. `create_upload` and `update_conversation` are `false` for the same
reason — a reservation is purely local, and archiving or pinning is a change
to the owner's own thread list. `remove_reaction` *is* open-world and
destructive: it takes back something the owner sent, and the recipient sees it
go.

## Results, and what `isError` means

A successful call carries `structuredContent` — `{ "data", "next_cursor",
"warnings" }`, which is the REST envelope minus the request ID — plus a
one-line text summary. The summary says what was returned **and whether more
exists**, so a model that reads only the text is not misled about
completeness.

**A domain failure is a result, not a JSON-RPC error.** It comes back with
`isError: true` and

```json
{ "error": { "code": "not_found",
             "message": "No such conversation.",
             "retryable": false,
             "details": {} } }
```

in `structuredContent`. That covers every code in the §7.2 table except the
transport-level ones. The reason is practical: many MCP clients surface a
JSON-RPC error as a transport failure and never hand it to the model, so a
`not_found` reported that way is a fact the model never learns and cannot
correct itself from.

Only these are JSON-RPC errors, and the code on each is the SDK's:

| JSON-RPC error | Code |
|---|---|
| an unknown method | `-32601` |
| an unknown tool name | `-32602` |
| any `resources/read` failure | `-32602` — the SDK's resource-not-found code — for a URI this server does not serve, a missing attachment, or a scope refusal; otherwise the SDK's own code for the failure |
| an authorization failure at the transport | answered as an HTTP status, before any JSON-RPC frame exists |

A body that is **not a JSON-RPC message at all** is neither: it is `400` with
the §7.1 envelope, because it carries no `id` and there is nothing to answer.

**Every JSON-RPC error is delivered with HTTP `200`.** That is the one place
the SDK is deliberately not the authority. From protocol `2026-07-28` the SDK
gives some JSON-RPC errors a 4xx status of their own, and the SDK's own client
treats any non-2xx as a connection failure and tears the session down — so a
model naming a tool that does not exist, which is an everyday thing for a model
to do, would disconnect the connector rather than be told. The error object is
passed through unchanged, code and all; only the status is demoted, which is
what every protocol revision before `2026-07-28` did anyway.

> **Correlating one of these with the server log takes a little work, and here
> is why.** The response carries an `X-Request-Id` and the server writes a
> matching line, but a reference MCP client builds its error from the JSON-RPC
> error object and does not surface response headers — so what reaches the
> operator is bare prose, `MCP error -32602`, with no ID in it. Putting the ID
> where a client would show it means writing into the SDK's `error.data`,
> which would break the pass-through rule above and make this server's errors
> differ from every other server built on the same SDK. The trade is
> deliberate: the log line is the correlation point, and
> `agm admin audit list` plus the timestamp is how you find it.

**The `isError` rule is about `tools/call`, and `resources/read` is the
boundary of its scope.** A `CallToolResult` has an `isError` field, and
reporting a domain failure there keeps the fact in front of the model. A
`ReadResourceResult` has no such field and *must* carry `contents`, so a
"result" reporting a failure is not a valid result at all: the official
TypeScript SDK rejects it on schema before the client's own code ever runs.
The model learns nothing either way — bytes never reach it — so the only thing
at stake is whether the client gets a readable refusal or a parse error.

Read the error and correct the call rather than repeating it.
`invalid_request` names the parameter you got wrong in `details.parameter` or
`details.field` — this server refuses a misspelled filter rather than silently
ignoring it. `unsupported_capability` means the action cannot apply here and
`details.reason` says why. `phone_not_responding` and `rate_limited` are
retryable; almost nothing else is.

## Resources

Attachment bytes are served as a **resource**, not as a tool, so a client can
fetch them without putting them through the model's context.

| Method | Answer |
|---|---|
| `resources/templates/list` | one template, `agm://attachments/{attachment_id}`, offered only to a caller holding `messages:read` |
| `resources/list` | **empty, on purpose.** Attachments are addressed by template, not enumerated |
| `resources/read` on an unknown URI or a missing attachment | `-32602`, a JSON-RPC error carried on HTTP `200`, for the reason above. A caller without `messages:read` gets the same answer: to it the resource does not exist |
| `resources/read` | the bytes of one attachment, under `messages:read`, with the same media limit and cache path as `GET /v1/attachments/{id}/content`. Text media comes back as `text`; everything else as a base64 `blob` |

`get_attachment` decides its content form by **size and type, not
preference**. A supported image under `media.inline_mcp_image_max_bytes`
(1 MiB by default) comes back as image content in the result; anything larger
or non-inlinable comes back as an `agm://attachments/{id}` resource link.
**The download ticket comes back either way**, in
`structuredContent.data`. And the summary text is always the first content
block, so a client that reads only the first block reads a sentence rather
than a megabyte of base64.

## What is not a tool

MCP serves the messaging surface, not the whole API. A `/v1` route has a tool
if and only if it is something an agent does with messages. Every route
carrying a `messages:*` scope is served in exactly one of three ways — as a
tool, as a resource, or as a named exclusion below — and a test walks the
route inventory in both directions to prove it.

| Excluded route | Reason |
|---|---|
| `POST /v1/conversations/{conversation_id}/typing` | no lasting effect and no result a model can act on |
| `GET /v1/messages/{message_id}/attachments` | its data is already inside `get_message`; a tool would only add a round trip |
| `GET /v1/uploads/{upload_id}` | an agent that has just called `create_upload` already holds everything it would return |
| `DELETE /v1/uploads/{upload_id}` | an agent that has just called `create_upload` already holds everything it would return |
| `GET /v1/accounts/{account_id}/events` | a stream; MCP tools are request/response. `get_session` and `list_accounts` answer the same question at a point in time |
| `GET /v1/accounts/events` | the all-accounts form of the same stream |
| `GET /v1/operations` | `get_operation` covers the ID-addressed case, the only one a model reaches: it holds the `operation_id` the write returned. Listing operations is an owner's audit question, served by `agm operations list` |
| `GET /v1/conversations/{conversation_id}/messages` | the same rows as `list_messages` with `conversation_id` set, which is the argument shape a model already has; two tools for one listing would be two places a filter could drift |

`GET /v1/attachments/{id}/content` is the resource above rather than an
exclusion.

Served in **none** of the three, by design:

- **`GET /v1/auth/whoami` and `POST /v1/auth/logout`** — credential
  self-management. A model does not choose its own token, cannot act on the
  answer, and must not be able to log its client out mid-conversation. The
  client owns its credential; the model does not.
- **`POST /v1/auth/admin-session` and `POST /v1/auth/refresh`** — credential
  issuance, reachable only by presenting a secret or a refresh token, neither
  of which a model holds.
- **every `/v1/pairing/*` route** — pairing is a physical act at the owner's
  browser and phone.
- **`PATCH` and `DELETE /v1/accounts/{id}`, `/sign-out`, `/reconnect`,
  `/refresh-cookies`** — signing an account out and deleting its history are
  owner acts with confirmation prompts. `list_accounts` and `get_session` give
  a model everything it can act on.
- **every `/v1/admin/*` route** — the two diagnostics an agent genuinely needs,
  backfill progress and the two settings that break sending, are served by
  `get_health` instead, without exposing the raw Google view.
