# OAuth 2.1

Agent GM is its own authorization server. claude.ai and ChatGPT connectors
require one, and this page is what they see.

Serves spec §9, with §11.5 for `agm auth login` and §12.3 for the budgets.
Where this page and the spec disagree, the spec is right.

Three things are true of the whole design and are worth reading before the
detail:

- **There is no self-service.** A connector cannot obtain a token unless the
  owner does two separate things: issue an enrollment code, and approve the
  request that code produces. A leaked code still cannot mint anything without
  a person looking at a screen and saying yes.
- **Client resolution is DCR-only** (D20). There is no preregistered client
  table, no Client ID Metadata Document fetch, and therefore **no outbound
  HTTP from the OAuth layer at all** — which removes the SSRF surface rather
  than defending it. Every client this server will ever see registers itself.
- **Nothing stores a credential value.** Enrollment codes, authorization
  codes, access tokens, refresh tokens and the two browser secrets all reach
  the database as hashes. There is no column that could hold a plaintext one.

## The routes

```text
GET  /.well-known/oauth-protected-resource
GET  /.well-known/oauth-protected-resource/mcp
GET  /.well-known/oauth-authorization-server
POST /oauth/register
GET  /oauth/authorize
POST /oauth/authorize
GET  /oauth/poll.js
GET  /oauth/requests/{authorization_request_id}
GET  /oauth/requests/{authorization_request_id}/status
POST /oauth/requests/{authorization_request_id}/complete
POST /oauth/token
POST /oauth/revoke
```

The root path is `404`. Every OAuth error body is
`{ "error", "error_description" }` with `Cache-Control: no-store`, **including
the ones an HTTP framework would otherwise answer itself**: a body over 1 MiB
is `413` with that shape, a wrong method is `405` with that shape and an
`Allow` header, and an unknown path under `/oauth` is the REST `not_found`
envelope — because an unknown path is not an OAuth protocol failure and
answering it as one would tell a client that a route it invented is part of
the protocol.

Every OAuth page and every OAuth error carries §9.9's headers:

```http
Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; script-src 'self'
Cache-Control: no-store
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
```

`script-src 'self'` is there for exactly one file, `/oauth/poll.js`. The
polling script is served rather than inlined precisely so that this directive
can stay `'self'` and no page needs a nonce or `unsafe-inline`.

## Discovery

`GET /.well-known/oauth-protected-resource` and its `/mcp` twin (RFC 9728):

```json
{
  "resource": "https://gm.example.test/mcp",
  "authorization_servers": ["https://gm.example.test"],
  "scopes_supported": ["messages:read", "messages:write", "messages:delete"],
  "bearer_methods_supported": ["header"],
  "resource_documentation": "https://github.com/thisnick/agent-gm"
}
```

Both protected-resource paths are served by the MCP Go SDK's own RFC 9728
handler, so both carry the CORS headers RFC 9728 §3.1 asks for —
`Access-Control-Allow-Origin: *`, `Access-Control-Allow-Methods: GET, OPTIONS`,
`Access-Control-Allow-Headers: Content-Type` — and both answer an `OPTIONS`
preflight with `204` and no body. This document is public discovery data, and a
browser-based MCP client cannot read it without that. The values in it are
unchanged, `resource` is still `AGENT_GM_PUBLIC_URL` plus `/mcp` byte for byte,
and these two paths are the **only** ones on this server meant to be read
cross-origin.

`GET /.well-known/oauth-authorization-server` (RFC 8414):

```json
{
  "issuer": "https://gm.example.test",
  "authorization_endpoint": "https://gm.example.test/oauth/authorize",
  "token_endpoint": "https://gm.example.test/oauth/token",
  "registration_endpoint": "https://gm.example.test/oauth/register",
  "revocation_endpoint": "https://gm.example.test/oauth/revoke",
  "response_types_supported": ["code"],
  "grant_types_supported": ["authorization_code", "refresh_token"],
  "token_endpoint_auth_methods_supported": ["none"],
  "code_challenge_methods_supported": ["S256"],
  "scopes_supported": ["messages:read", "messages:write", "messages:delete"],
  "authorization_response_iss_parameter_supported": true
}
```

**`issuer` equals `AGENT_GM_PUBLIC_URL` byte for byte**, and `resource` is that
plus `/mcp` with no trailing-slash drift. Both are asserted as string equality
rather than parsed-URL equivalence, because a client that fetched the metadata
and compared the issuer against the one it asked for will reject a mismatch,
and two URLs that parse the same are not the same bytes.

The `401` challenge on `/mcp`:

```http
WWW-Authenticate: Bearer resource_metadata="https://gm.example.test/.well-known/oauth-protected-resource/mcp", scope="messages:read messages:write"
```

One line, printed unwrapped because it is sent unwrapped — HTTP line folding is
obsolete and a folded copy is a header no server sends.

There is no `realm` and no `error` parameter here. `/mcp` is served by the MCP
Go SDK's bearer middleware and this is the format that middleware emits; the
SDK owns it. The `/v1` challenge of §7.2 is a **different string** — it does
carry `realm="agent-gm"` and an `error` parameter — so a client that parses one
of the two must not assume the other. See [api.md](api.md) and
[mcp.md](mcp.md#transport).

A valid token carrying **no** messaging scope gets `403 insufficient_scope`
with the *same* challenge — deliberately distinct from the `401`, because a
caller with no credential is told to get one and a caller whose credential is
too narrow is told which scope it would have needed.

The challenge names two scopes and not `messages:delete`. `scope` in a
challenge is what a client should **ask for**, and those two are the default an
authorization request carries. A challenge that also asked for
`messages:delete` would send every connector to an approval screen offering to
let a model delete the owner's threads, for no better reason than that the
scope exists. A client that wants it asks for it.

## Dynamic client registration

`POST /oauth/register`, JSON, answers `201` with a `client_id` and **no
`client_secret`**. Public native clients only, which is the whole security
model: there is no secret to leak, so PKCE and the registered redirect are what
bind an authorization to the client that started it.

- `token_endpoint_auth_method` must be `none`.
- Grants must be a subset of `authorization_code` and `refresh_token`.
- The only response type is `code`.
- A client-chosen `client_id` is refused. RFC 7591 lets a server ignore one;
  ignoring it would leave the client believing it registered an ID it did not
  get, and the first authorization would fail naming a client nobody has heard
  of.

### Redirect URIs

| Accepted | Refused |
|---|---|
| `https://` with a fully qualified host | `http://` on any non-loopback host |
| `http://127.0.0.1[:port]/…` | `http://localhost.evil.example/…` |
| `http://[::1][:port]/…` | `https://` with an IP literal |
| `http://localhost[:port]/…` | any URI with a fragment or embedded credentials |
| a private-use scheme containing a dot | |

At most 10 redirect URIs, each at most 500 characters. Registration is limited
to **20 per source per hour**, counted over rows rather than a token bucket, so
it survives a restart — a registration is a durable object, and forgiving it on
restart would forgive something that is still there.

A registration **expires 24 hours after creation** unless an authorization
activates it. A maintenance pass removes expired unreferenced registrations
every 60 seconds and audits each removal as `client.revoked`.

A registered **loopback** redirect matches **any port** at authorization time
(RFC 8252 §7.3), for `127.0.0.1`, `[::1]` and `localhost` alike, because a
native client binds an ephemeral port it cannot know at registration time.
Everything else must still be equal: scheme, host, path and query. So a
registered `http://127.0.0.1/cb` matches `http://127.0.0.1:53211/cb` and
matches neither `http://127.0.0.1:53211/cb2` nor `http://127.0.0.2/cb`. **The
token endpoint still requires `redirect_uri` to equal the one bound to the code
exactly**, so the port a client authorized on is the port it must present at
exchange.

> **Deliberate deviation, recorded as contract text.** `http://localhost/…` is
> accepted, against RFC 8252 §8.3, which prefers the IP literals because
> `localhost` resolution depends on the host's name service. That hazard exists
> only on the client's own machine, and widely used MCP clients register the
> name; refusing them buys little. **The allowance is for the literal host name
> and nothing else**: the comparison is exact and case-insensitive, so
> `localhost.evil.example`, `notlocalhost`, `local.host` and
> `localhost@evil.example` are ordinary domains with no plain-http exemption. A
> suffix or substring match here would be far worse than the problem the
> allowance solves.

## Authorization

`GET /oauth/authorize` takes `response_type=code`, `client_id`,
`redirect_uri`, `state`, `code_challenge`, `code_challenge_method=S256`,
`resource`, and an optional `scope` defaulting to
`messages:read messages:write`.

`code_challenge_method` must be **present** and `S256`. RFC 7636 defaults an
omitted method to `plain`, which is not supported; defaulting it would silently
accept an unprotected exchange.

An unknown client or an unregistered redirect URI answers `4xx` and **never
redirects** (RFC 6749 §4.1.2.1): until the callback is verified there is
nowhere safe to send an error, and redirecting to an unverified URI is how an
open redirector is built. Every later failure redirects to the verified
callback carrying `error`, `state` and the RFC 9207 `iss`:

| Condition | `error` |
|---|---|
| `response_type` is not `code` | `unsupported_response_type` |
| missing `state`, missing or non-`S256` PKCE, malformed challenge | `invalid_request` |
| `resource` is not `https://gm.example.test/mcp` | `invalid_target` |
| unknown or empty scope, or `admin` requested | `invalid_scope` |

### The screen

The page sets `agm_oauth_context`, a signed cookie with `Path=/oauth`,
`HttpOnly`, `Secure`, `SameSite=Lax`. The form carries the signed context, an
anti-CSRF `form_token`, hidden echoes of every OAuth parameter, one `scope`
checkbox per requested scope, an `enrollment_code` input, and — **because
scopes are global across accounts (D29)** — this line, rendered verbatim above
the checkboxes:

> This will let the client read and send as any Google account on this server,
> including accounts added later.

followed by the current accounts' labels and addresses. That sentence is part
of the screen's contract and is asserted as a string, which is what makes
§9.7's claim to honesty testable rather than aspirational: the owner approves
knowing what is in scope.

The `form_token` is **derived** from the cookie's handle rather than stored, so
the waiting page can re-render it from the cookie the browser already holds
without the server keeping a second secret alive across two requests. It is
unguessable without the handle, which is unguessable without the cookie.

`POST /oauth/authorize` verifies, **in this order**:

1. `Origin`, when present;
2. the cookie;
3. the signed context;
4. that the cookie's handle hashes to the context;
5. the form token;
6. every hidden echo;
7. that the client and redirect are still valid;
8. that the selected scopes are a nonempty subset of the requested set;
9. **only then** the enrollment code.

The order matters. Everything before the last step is a check on the *browser*;
the enrollment code is a check on the *owner's secret*. A server that examined
the code first would let a caller with no session at all spend an owner's code
attempts and learn, from the difference in responses, which codes exist.

Outcomes:

- **Success** → `303` to `/oauth/requests/{id}`.
- **Invalid code** → `200` re-rendering the form with **one generic message,
  byte-identical for unknown, expired, revoked and consumed codes**. No pending
  request is created and nothing is consumed.
- **Scopes above the code's ceiling** → `200` re-rendering, showing the access
  the code does allow; the code stays redeemable. This is deliberately *not*
  the generic failure: the owner's code is fine and only the selection is
  wrong, which they can correct.
- **The eleventh failed code attempt within 15 minutes**, per signed context
  **or** per source → `429` with `Retry-After`. The per-source bucket is
  checked *before* the submission is examined, so loading a fresh authorization
  page does not reset it.

## Enrollment codes

```text
POST   /v1/admin/enrollment-codes            # answers 200, not 201
GET    /v1/admin/enrollment-codes
GET    /v1/admin/enrollment-codes/{id}
DELETE /v1/admin/enrollment-codes/{id}?reason=…
```

`POST` takes `{"label", "expires_in"?, "scopes"?, "allow_scopes"?}`.
**`data.code` is the only time the value is returned**, and only its SHA-256 is
stored — which is why the answer is `200` and not `201`: there is no resource
at a URL to point a `Location` header at.

`expires_in` is a duration string (`30m`) or a number of seconds, defaulting to
the `oauth.enrollment_default_ttl` setting (15m), bounded 1m–24h. `scopes`
**replaces** the default ceiling `messages:read messages:write`;
`allow_scopes` **extends** it; they are mutually exclusive, because a request
carrying both is a request whose author did not know which they meant.
**`admin` can never be enrolled.** Enrollment codes carry **no account
dimension**, consistent with D29: a code caps which scopes may be granted,
never which accounts they reach.

Revocation takes its reason as the query parameter `?reason=`, and repeating it
answers `200` with `revoked: false` — a second revocation is not a failure, and
answering `404` or `409` would make an idempotent script wrong.

### Checking that the value is not in the database

The stored hash is **plain SHA-256 of the code's canonical form**, hex, with no
domain separator — deliberately, so an owner can verify it themselves without
running any Agent GM code. The canonical form is the code upper-cased with
every character outside the alphabet `0123456789ABCDEFGHJKMNPQRSTVWXYZ`
removed, which in practice means dropping the hyphens:

```console
$ printf '%s' A1B2C3D4E5F6G7H8 | sha256sum
b1e0…  -
$ sqlite3 /data/agent-gm.sqlite3 "SELECT code_hash FROM enrollment_codes WHERE id='enroll_…';"
b1e0…
```

Everything else Agent GM hashes *is* domain-separated, so a digest computed for
one purpose can never verify a value presented for another.

## Owner approval

```text
GET  /v1/admin/authorization-requests[?status=pending|approved|denied|completed]
GET  /v1/admin/authorization-requests/{id}
POST /v1/admin/authorization-requests/{id}/approve   # {"scopes"?}
POST /v1/admin/authorization-requests/{id}/deny      # {"reason"?}
```

or, on a terminal:

```console
$ agm admin authorization-requests list --status pending
$ agm admin authorization-requests approve authreq_…
```

`scopes` at approval may only **narrow** the browser-selected set. Widening or
an empty list is `invalid_request`; a no-longer-pending request is
`idempotency_conflict`; an expired one is `invalid_request`. Narrowing-only
matters because the browser-selected set is what the owner saw on screen next
to the disclosure line — an approval that could widen it would grant access the
screen never described.

There is no approval **page**. An approval page is a human UI and contradicts
non-goal N2; the owner approves on a terminal. That is open question OQ-3's
recorded answer.

### The waiting page

`/oauth/requests/{id}` polls `/oauth/requests/{id}/status`, which answers
`{"request_id", "status", "expires_in_seconds", "poll_interval_seconds"}` with
`no-store`. The server decides the interval; the page clamps it to 1–60
seconds, because a page that believed a value outside that range would either
hammer the server or appear to hang.

**Without a valid context cookie every `/oauth/requests` route answers `404`.**
The request ID alone conveys no authority: it appears in a redirect, it is
logged by proxies, and it is the sort of value that ends up in a chat window.
Answering `403` would confirm that it exists.

`POST /oauth/requests/{id}/complete` requires the cookie, the waiting page's
form token, and a same-origin `Origin`. An approved, unexpired request answers
`303` to the exact registered callback with `code`, `state`, `iss` and the
granted `scope`, and clears the cookie. A **denied** request redirects with
`error=access_denied`, because a denial is a decision the client is entitled to
hear. Expired, completed or still-pending answers `409`, and **a completed
request can never mint a second code**. `GET /oauth/requests/{id}` answers
`200` for every state including expiry — it renders a page rather than
performing an operation, and a person who left a tab open overnight should read
what happened rather than a `409`.

## Tokens

`/oauth/token` and `/oauth/revoke` are `application/x-www-form-urlencoded` with
`Cache-Control: no-store`.

`grant_type=authorization_code` takes `code`, `redirect_uri`, `client_id`,
`code_verifier`, and an optional `resource` that must equal the canonical
resource. `resource` is **mandatory at `/oauth/authorize` and optional here**:
this server has exactly one audience, so an omitted value is read as the
canonical resource and a present one must match it (RFC 8707 §2.2 permits this
for a single-audience server, and the binding that matters was fixed at
authorization time).

Two rules have teeth:

- A **replayed code** is `invalid_grant` **and revokes the tokens the first
  exchange produced**. A code seen twice means the value leaked, and the honest
  response is to invalidate what it bought rather than to refuse only the
  second attempt.
- A **PKCE verifier that does not hash to the bound challenge** is
  `invalid_grant` **and consumes the code**, so a verifier cannot be guessed by
  retrying.

`grant_type=refresh_token` takes `refresh_token`, `client_id`, and an optional
`scope` that may only narrow; widening is `invalid_scope` and **does not spend
the presented token**. Refresh tokens rotate on every use, and reuse of a spent
token revokes the whole family and its authorization. A rotation, its audit
record, and the family revocation that reuse triggers all commit in one
transaction.

`POST /oauth/revoke` (RFC 7009) takes `token` and `client_id` and **always
answers `200`**. A token belonging to another client, or to nobody, is ignored
without confirming that it exists. Revoking any token of a grant revokes the
whole grant.

Only hashes are stored; no token value is ever written to the database, a log,
or an audit payload.

| Setting | Default | Bounds |
|---|---|---|
| `oauth.access_token_ttl` | 15m | 5m–1h |
| `oauth.refresh_token_idle_ttl` | 30d | 1d–90d |
| `oauth.refresh_token_absolute_ttl` | 90d | 7d–365d |
| `oauth.authorization_code_ttl` | 2m | 30s–5m |
| `oauth.authorization_request_ttl` | 15m | 1m–1h |
| `oauth.enrollment_default_ttl` | 15m | 1m–24h |

Access tokens are bound to `https://gm.example.test/mcp`, and the binding is
inside the hash. **If `AGENT_GM_PUBLIC_URL` ever changes, every token minted
under the previous origin is refused with `401 invalid_token` on both `/mcp`
and `/v1`, and every registered client is orphaned** — there is no separate
audience check to forget, because a token minted under another origin resolves
to nothing at all. See [operations.md](operations.md) and spec §15.6.

### The two credential paths never cross

The admin bootstrap (`POST /v1/auth/admin-session`, exchanging
`AGENT_GM_ADMIN_SECRET`) is a separate path. An OAuth refresh token at
`/v1/auth/refresh` is `invalid_token`; an admin refresh token at `/oauth/token`
is `invalid_grant`. **Neither attempt revokes anything** — a credential
presented at the wrong endpoint is a mistake, not an attack on the grant it
belongs to.

An OAuth authorization can never hold `admin`. It is refused three times
independently: the authorization screen refuses `scope=admin`, an enrollment
ceiling cannot contain it, and the token mint refuses it again. Three refusals,
because one of them is one edit away from being wrong.

## Scopes

| Scope | Grants |
|---|---|
| `messages:read` | every read route and read tool; `whoami`; `logout`; download-ticket redemption |
| `messages:write` | send, start, mark read, reactions, uploads; `get_operation`; upload-ticket redemption |
| `messages:delete` | `delete_message` and `delete_conversation`, and nothing else |
| `admin` | `/v1/admin/*` and all pairing routes. Issued only by the admin bootstrap; **never enrollable** |

**Scopes are global across accounts** (D29). A token holding `messages:read`
reads every account this server holds; one holding `messages:write` can send
from any of them. Per-account scoping is **deferred, not refused**: it needs a
scope grammar, a UI for choosing accounts at the authorization screen, and
enrollment ceilings that can name accounts that may not exist yet. Until then
the honest statement is the one the authorization screen renders verbatim above
its scope checkboxes. An owner who needs a genuinely separated account runs a
second Agent GM.

## Budgets on unauthenticated endpoints

`/oauth/revoke`, the `refresh_token` grant, and `POST /v1/auth/refresh` all
accept a bearer value from an unauthenticated caller: **the presented token is
the credential**, so an attacker can guess at all three in the same shape. All
three carry:

- **60 requests/minute per source, burst 20**, in memory. A restart forgives an
  anonymous caller's request debt, which costs nothing.
- **A durable limit of 30 unknown or invalid presented tokens per 15 minutes**,
  with a cooldown that doubles per further failure in the window and caps at 24
  hours, held in `oauth_attempts` — because that is the limit an attacker would
  restart-cycle to reset.

Both are checked **before** the presented token is examined, so a `429` never
distinguishes a real token from a guess, and it always carries `Retry-After`. A
successful presentation does **not** clear the failure counter.

The presented token is looked up read-only **before any write transaction
opens**, so a caller presenting a value that belongs to nobody cannot take the
single writer's lock and queue every other writer behind itself. When the token
does exist, the transaction re-reads the row before cascading, so revocation
stays a single atomic decision.

`POST /v1/auth/admin-session` is budgeted separately and more tightly (5 per 15
minutes per source, 20 globally, exponential cooldown), because a success there
yields more than a success anywhere else.

## Revoking a connector

```console
$ agm admin authorizations list
$ agm admin authorizations revoke auth_… --reason "no longer used"
```

Revoking an authorization revokes **every token of it**, so the connector's
next call is a `401` and it cannot refresh its way back.
`agm admin clients revoke` goes further: it removes the dynamic registration
too, after revoking every authorization it holds. Removing the row alone would
leave live tokens behind, because a grant outlives the client row.

## `agm auth login`

`agm auth login` without `--admin` performs **the same flow as any other MCP
client**: discovery, dynamic registration, a loopback callback on `127.0.0.1`,
PKCE `S256`, and verification of `state` and the RFC 9207 `iss` before the code
is exchanged. A callback whose `iss` does not match is refused, **and so is one
carrying no `iss` at all**, because the server's metadata advertises
`authorization_response_iss_parameter_supported`.

`--no-browser` prints the URL instead of opening one. `--scopes` requests a
specific set; **`admin` is refused there** and is issued only by
`agm auth login --admin`, which exchanges `AGENT_GM_ADMIN_SECRET` over
`--secret-stdin` or a TTY prompt.

`agm auth login --server <url>` names the profile it creates after the
server's **host** — a login to `https://gm.example.test` creates the profile
`gm.example.test` — and records it as the active profile, so nothing
afterwards needs `--server`. `--profile <name>` overrides the name. There is
no profile called `default` and no built-in hostname: a command with no
`--server`, no `AGENT_GM_URL` and no active profile fails saying what to type.

Each profile is bound to an exact server issuer and resource, and records the
`client_id` its tokens belong to. `agm auth login` is the only command that
may name a server this machine has no profile for; on every other command a
`--server` that matches no stored profile is refused as `invalid_request`,
naming `agm auth login --server`. One profile's token is never forwarded to
another origin. See [cli.md](cli.md) §Credentials and profiles.

A whole login, on one terminal:

```console
# with an admin session, in another shell
$ agm admin enrollment-codes create "claude.ai" --expires-in 30m
code: A1B2-C3D4-E5F6-G7H8      # printed once, never again

# the client
$ agm auth login --server https://gm.example.test --no-browser
Open this URL to authorize:
https://gm.example.test/oauth/authorize?...

# paste the code into the screen, then, with the admin session
$ agm admin authorization-requests list --status pending
$ agm admin authorization-requests approve authreq_…

# the profile is named for the host and is now the active one
$ agm profiles list
* gm.example.test                  https://gm.example.test
```

## Audit

Every step writes a row (§12.4), and no payload carries a token, a code or a
cookie: `enrollment.created`, `enrollment.consumed`, `enrollment.revoked`,
`enrollment.expired`, `authorization.created`, `authorization.approved`,
`authorization.denied`, `authorization.expired`, `authorization.revoked`,
`client.revoked`, and the credential layer's `auth.refresh_token_reuse_detected`.

```console
$ agm admin audit list --kind-prefix authorization.
$ agm admin audit list --kind enrollment.consumed
```

## Conformance

`devbox run conformance` builds the server, starts it on a free loopback port
with a throwaway data directory and admin secret, mints a token **through the
whole flow above**, starts a small loopback proxy that adds it, and runs the
pinned `@modelcontextprotocol/conformance` package against the proxy. An admin
bootstrap token is deliberately not used: it would work, and it would leave the
client-shaped path unmeasured.

The baseline is `scripts/mcp-conformance-baseline.yaml` and is checked in both
directions — a new failure fails the run, and so does a listed scenario that
starts passing — so the file cannot rot into a list of excuses. A revision that
runs **zero** scenarios is a failure, not a green line that tested nothing.
