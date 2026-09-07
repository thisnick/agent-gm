# `agm`, the command line

Serves spec §6.3, §11.1–§11.3, §11.5 and §12.1.

`agm` is one binary and it speaks the [REST API](api.md) and nothing else. It
never opens SQLite and never talks to Google directly, so anything the CLI can
do an agent can do too, and the reverse. Every command below names the routes
it drives; if you want the route's parameters, its DTO or its error semantics,
read [api.md](api.md).

Pairing has a page of its own — [pairing.md](pairing.md) — because most of what
there is to say about `agm pair` happens on the phone.

## Installing

```sh
npm i -g @agent-gm/cli
agm version
# agm 1.0.0 (121648dd0fa089443752558866e53e707640e2dc)
```

`@agent-gm/cli` is a **thin wrapper**, not a bundle. Installing it downloads
the `agm` archive for your platform from the matching GitHub release and
verifies it against a copy of `checksums.txt` that was pinned inside the npm
tarball when it was published — so a release page edited afterwards cannot
hand an already-published version a different binary. The package version
equals the Go release version: `@agent-gm/cli@1.4.2` installs `agm 1.4.2`.

Supported platforms are `linux/x64`, `linux/arm64`, `darwin/x64` and
`darwin/arm64`. Anything else **fails the install** with a message naming the
platform, rather than leaving a shim that cannot run. Windows is not in the
v1 matrix; download a binary from the release page or build from source.

Two environment variables belong to the wrapper alone:

| Variable | Effect |
|---|---|
| `AGENT_GM_CLI_SKIP_DOWNLOAD=1` | skip the postinstall download, for a CI image that supplies the binary itself |
| `AGENT_GM_CLI_BINARY=<path>` | run this binary instead of the vendored one |

Or take the archive directly, which is the same binary:

```sh
v=1.0.0; os=darwin; arch=arm64
base=https://github.com/thisnick/agent-gm/releases/download/v$v
curl -fsSLO $base/agm_${v}_${os}_${arch}.tar.gz
curl -fsSLO $base/checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing
tar -xzf agm_${v}_${os}_${arch}.tar.gz agm && chmod 0755 agm
```

`checksums.txt` is signed with cosign, keyless, over GitHub OIDC; the release
notes carry the exact `cosign verify-blob` invocation, the GHCR digest of the
matching image, and the `libgm` and `go-sdk` pins the binary was built
against.

**The exit codes survive the wrapper.** `agm --nonsense` exits `2` through npm
exactly as it does natively, and `1` stays unassigned so that a `1` is always
the wrapper's, the shell's or the runtime's — never Agent GM's.

## Global flags

Available on every command.

```text
--server <url>          Select a server this machine has a profile for. On any
                        command except `agm auth login` it must match a stored
                        profile's server; there is no built-in hostname to fall
                        back on.
--profile <name>        Select a saved profile for this one invocation.
--json                  Emit one stable JSON value on stdout.
--output <format>       table (default) | json | jsonl
--timeout <duration>    Bound the request, and any operation wait.
--quiet                 Suppress non-result output.
--verbose               Diagnostic detail on stderr.
--idempotency-key <k>   Resume one logical state-changing request. Optional,
                        and off by default: an ordinary command sends no key.
--yes                   Skip the confirmation prompt on a destructive command.
--credentials-file <p>  Override the credentials file.
```

**Durations are an integer plus a unit, `s`, `m`, `h` or `d`**: `30s`, `15m`,
`2h`, `7d`. There is no `1.5h`, no `90` and no `1h30m`. Anything else is exit
`2` naming the flag.

`--account <acct-id>` is not global but appears on nearly every command that
touches an account, and defaults to `AGENT_GM_ACCOUNT`. The rule for when it
may be omitted is the API's and is worth reading once: with one account
paired, always; with several, on a read but never on a write
([api.md](api.md#choosing-an-account)).

## `--json`

With `--json`, **exactly one JSON value goes to stdout and everything else goes
to stderr** — for every command, without exception. Diagnostics, progress,
warnings, the confirmation prompt and the `--wait-for` warning below are all
stderr. That is what makes `agm ... --json | jq` safe in a pipeline even when
the command has something to say.

The value on stdout is the server envelope of [api.md](api.md#envelopes) plus
CLI metadata: the effective profile and the effective server. `--output jsonl`
streams one object per line for a listing instead.

Without `--json`, human output prints **the operation ID first** for every
mutation, so a transcript always contains the thing you need to pass to
`agm operations wait`.

## Exit codes

`1` is deliberately unassigned, so a `1` is the wrapper's, the shell's or the
runtime's — never Agent GM's decision.

| Code | Meaning |
|---|---|
| `0` | Success |
| `2` | CLI usage or validation error |
| `3` | Authentication required, or expired credentials |
| `4` | Authorization or insufficient scope |
| `5` | Requested resource absent |
| `6` | Unsupported capability for this account, conversation or message |
| `7` | Retryable network, Google, phone or rate-limit failure |
| `8` | An operation reached a terminal failure |
| `9` | Local configuration or credential-store failure |
| `10` | Server contract or internal failure |

Every error code in [api.md](api.md#error-codes) maps onto exactly one of
these, exhaustively; the mapping lives in `internal/apierr` so `agm` and the
server cannot come to disagree about what exit `7` means.

| Error codes | Exit |
|---|---|
| `invalid_request`, `idempotency_conflict`, `payload_too_large`, `media_unsupported_type` | `2` |
| `invalid_token` | `3` |
| `insufficient_scope` | `4` |
| `not_found` | `5` |
| `unsupported_capability` | `6` |
| `rate_limited`, `disconnected`, `phone_not_responding`, `google_http_error`, `pairing_init_timeout` | `7` |
| an operation reaching `failed` or `unknown` while waiting | `8` |
| `not_paired`, every other `pairing_*`, `not_default_sms_app`, `google_error`, `google_undocumented_status`, `google_permission_denied`, `internal_error` | `10` |

Three of them are easy to get wrong, so they are stated plainly.

**`idempotency_conflict` is `2`, not a server failure.** It means the same
idempotency key was presented with a different body, which is a caller mistake.
Fix the invocation; never retry it unchanged.

**`phone_not_responding` is `7`, and deliberately not `8`.** The operation is
`pending`, not failed: the server accepted the request and the phone may still
act on it when it wakes. `agm` says so in the message, prints the operation ID,
and leaves it to `agm operations wait`. A script that treats this as a failure
and resends sends the message **twice**. `8` is reserved for an operation that
genuinely reached `failed` or `unknown` while `agm` was waiting on it.

**`unsupported_capability` is `6`, and it is usually an account-level
condition.** The commonest reason is `not_signed_in` — the account is
`signed_out`, `error`, `parked` or `account_changed`, so it still reads but
refuses writes. The fix is an action on the account (`agm pair
--refresh-cookies`, or `agm pair` again), not a retry of the command. The other
reasons are properties of the conversation or the message and are listed in
[api.md](api.md#unsupported_capability-reasons).

A command that times out waiting returns `7`, prints the operation ID, and
leaves it available to `agm operations wait <op-id>`.

## Commands

Grouped as spec §11.3 groups them. Each entry names the routes it drives; the
mapping is declared in `internal/cli/commands.go` and a test asserts that
**every `/v1` route parameter is reachable from a command and every `/v1` route
has one**, so this list cannot quietly fall behind the API.

The two exceptions are `GET` and `DELETE /v1/uploads/{upload_id}`, which have
no command: `agm messages send --file` performs the whole reserve, `PUT` and
send sequence in one process and never surfaces an `upl_` ID for a human to
inspect or cancel.

### Pairing

```text
agm pair [--account <acct-id>] [--device-index N] [--timeout 5m]
agm pair --refresh-cookies --account <acct-id>
agm pair --paste [--paste-file <path>]
```

`agm pair` is one command from start to paired and covers `POST`, `GET` and
`DELETE /v1/pairing/*` — the `GET` is the poll, and Ctrl-C issues the
`DELETE`. Cookies are captured over CDP from a short-lived Chrome profile, or
read from `--paste` / `--paste-file`; **they are never a flag value, because a
secret must not appear in `argv`**.

`agm pair --refresh-cookies` drives
`POST /v1/accounts/{account_id}/refresh-cookies` and re-authenticates an
existing pairing without re-pairing. `--account` is required with it.

The whole walkthrough, the phone settings that must be right, and every
pairing failure is in [pairing.md](pairing.md).

### Accounts

```text
agm accounts list [--all]
agm accounts show <acct-id>
agm accounts label <acct-id> <label>
agm accounts sign-out <acct-id> [--yes]
agm accounts remove <acct-id> [--yes]
agm session [--account <acct-id>] [--watch]
agm reconnect --account <acct-id>
agm health
```

- **`agm accounts list`** — `GET /v1/accounts`. The fast operator view. `--all`
  is a display switch here, not a paging one: the route is not paginated.
- **`agm accounts show`** — `GET /v1/accounts/{account_id}`. The positional
  `<acct-id>` defaults to `--account`. Adds the `google`, `backfill`, `sweep`
  and `counters` blocks the listing omits.
- **`agm accounts label`** — `PATCH /v1/accounts/{account_id}`. Both the ID and
  the label are positional. A label is a human name for a listing; nothing else
  about an account is mutable.
- **`agm accounts sign-out`** — destructive. Shreds the account's session file
  and refuses writes afterwards, and **deletes nothing**.
- **`agm accounts remove`** — destructive, and **the only command that deletes
  an account's history**.
- **`agm session`** — `GET /v1/accounts/{account_id}`, or the SSE routes with
  `--watch`. With `--watch` and no `--account` it streams every account's state
  changes, each tagged.
- **`agm reconnect --account <acct-id>`** — `POST /v1/accounts/{account_id}/reconnect`.
- **`agm health`** — `GET /v1/health`. `status` is about the server; an account
  in `signed_out` does not make it anything but `ok`.

```console
$ agm accounts list
ID              GOOGLE ACCOUNT        LABEL     STATE       PHONE
acct_01k4z0aa   owner@example.test    personal  connected   responding
acct_01k4z0bb   work@example.test     work      signed_out  —
```

### Conversations

```text
agm conversations list [--account|--query|--participant|--folder|--type
                        |--unread|--group|--include-deleted|--limit|--all]
agm conversations show <conv-id>
agm conversations start <e164>... [--account <acct-id>] [--name <name>]
agm conversations archive|unarchive|pin|unpin|mark-unread <conv-id>
agm conversations mark-read <conv-id> --message <msg-id>
agm conversations delete <conv-id> [--yes]
agm conversations typing <conv-id>
```

- **`agm conversations list`** — `GET /v1/conversations`. Every filter is a
  flag: `--account`, `--query`, `--participant`, `--folder`, `--type`,
  `--unread`, `--group`, `--include-deleted`, `--limit`. `--all` walks every
  page.
- **`agm conversations show <conv-id>`** — the only way to see
  `peer_typing_until`, which is always `null` in a listing.
- **`agm conversations start <e164>...`** — `POST /v1/conversations`.
  `--account` is **required when more than one account is paired**, because the
  target is a phone number and nothing else in the call can imply the account.
  `--name` is accepted only for two or more recipients.
- **`agm conversations archive|unarchive|pin|unpin|mark-unread <conv-id>`** —
  five subcommands over one `PATCH`. The state is named as a verb rather than
  passed as a mode: `archive` is `folder=archived`, `unarchive` is
  `folder=active`, `pin` and `unpin` set `pinned`, `mark-unread` sets `unread`.
  Asking for a state the conversation is already in answers `changed: false`
  and calls Google zero times.
- **`agm conversations mark-read <conv-id> --message <msg-id>`** — `mark-read`
  is `mark_read` is `POST .../read`: the same word on all three surfaces.
- **`agm conversations delete <conv-id>`** — destructive; see below.
- **`agm conversations typing <conv-id>`** — fire-and-forget. No operation, no
  idempotency key, nothing to wait for.

```console
$ agm conversations list --account acct_01k4z0aa --unread --limit 3
CONV ID           NAME    TYPE     UNREAD  LAST ACTIVITY
conv_01k4z2p8vq   Alex    rcs      yes     2026-09-06T09:41:02Z
```

### Messages

```text
agm messages list [<conv-id>] [--account|--direction|--sender|--after|--before
                   |--has-attachment|--delivery-state|--include-system|--limit|--all]
agm messages show <msg-id>
agm messages context <msg-id> [--before 5] [--after 5]
agm messages search <query> [--account] [--mode words|exact] [--conversation <conv-id>]
                            [--sender|--after|--before|--has-attachment|--limit]
agm messages send <conv-id> [--text <t>] [--file <path>] [--reply-to <msg-id>]
                            [--force-rcs] [--wait] [--wait-for sent|delivered|read|terminal]
agm messages delete <msg-id> [--yes]
agm messages add-reaction <msg-id> <emoji>
agm messages remove-reaction <msg-id> <emoji> | --reaction <react-id>
```

- **`agm messages list`** — with a positional `<conv-id>` it is
  `GET /v1/conversations/{conversation_id}/messages`; without one it is
  `GET /v1/messages` across every account, or one with `--account`.
- **`agm messages context <msg-id>`** — the messages around one message.
  `--before` and `--after` each default to 5 and cap at 100.
- **`agm messages search <query>`** — the query is positional. `--mode words`
  requires every term; `--mode exact` matches the phrase as written. During
  backfill the result carries a `history_incomplete` warning, so an empty
  answer is not proof of an absent message.
- **`agm messages send <conv-id>`** — needs `--text`, `--file`, or both.
  `--file` takes **one** path and does the whole media dance for you: reserve,
  `PUT`, send. An agent driving the API does those three steps itself, because
  it has no filesystem the server can reach.
- **`agm messages delete <msg-id>`** — destructive; see below.
- **`agm messages add-reaction <msg-id> <emoji>`** and
  **`agm messages remove-reaction <msg-id> <emoji>`** — both positional.
  `--reaction <react-id>` removes by ID instead. There is no `unreact`.

Every mutation accepts `--idempotency-key`, and **omitting it is the ordinary
case**: `agm` then sends no `Idempotency-Key` header at all and the server
mints the operation ID. Re-running the command sends again, which is what
re-running a send has always meant.

`agm` never mints a key for you. It used to, and that was a fiction: a fresh
key on every run is exactly a run with no key, with one more field on the
wire. The flag is for **automation that retries** — pass the *same* value on
the retry and the second call returns the first operation and sends nothing.
Reusing a key against a *different* `--account` is refused rather than obeyed,
because obeying it would send a second real message to a real person.

If a send times out and you sent no key, **do not just re-run it**:
`agm messages list --conversation <conv-id>` or `agm operations list` first.
The send may well have gone.

### Attachments and contacts

```text
agm attachments list <msg-id>
agm attachments show <att-id>
agm attachments download <att-id> [--out <path>]
agm contacts list [--account] [--query <q>] [--top] [--limit N]
```

`agm attachments download` uses the download ticket and puts it in the
`Authorization` header, never in the URL. `--out` names the file.

### Operations

```text
agm operations list [--account|--kind|--status|--terminal|--after|--before|--limit|--all]
agm operations show <op-id>
agm operations wait <op-id> [--for sent|delivered|read|terminal] [--timeout 60s]
```

`agm operations wait` polls `GET /v1/operations/{operation_id}` client-side and
takes its default bound from the server's `operations.wait_timeout` setting.
`cursor` never reaches a flag on any listing: `--all` walks the pages, and an
opaque signed cursor is not something a human types.

### Waiting: `--wait` and `--wait-for`

`--wait` on `agm messages send` defaults to `--wait-for sent`.

| `--wait-for` | Satisfied by |
|---|---|
| `sent` | the carrier took it |
| `delivered` | the recipient's device has it |
| `read` | the recipient opened it |
| `terminal` | an **operation** status of `succeeded`, `failed` or `unknown`, **or** a **message** `delivery_state` of `delivered`, `read`, `failed`, `canceled` or `deleted` |

**`delivered` and `read` are message states, not operation states.**
`--wait-for delivered` therefore keeps waiting after the operation is already
terminal, which is deliberate and is occasionally what you want.
**`--wait-for terminal` is the one that means "stop as soon as anything is
settled".**

**`agm` warns on stderr when `--wait-for delivered` or `--wait-for read` is
asked for on an `sms_mms` conversation.** SMS usually stops at `sent`: the
carrier reports nothing further, so an agent waiting for `delivered` can wait
until its timeout for a state that will never arrive. The warning goes to
stderr, so it does not disturb `--json`.

### Auth

```text
agm auth login [--admin] [--server <url>] [--profile <name>] [--scopes ...]
               [--no-browser] [--secret-stdin]
agm auth logout
agm auth whoami

agm profiles list
agm profiles use <name>
agm profiles remove <name>
```

`agm auth login` is the only command that may name a server this machine has
no profile for — it is where a profile comes from. It stores the profile under
the server's host (or under `--profile <name>` if you give one) and makes it
active, so nothing afterwards needs `--server`. See
[Credentials and profiles](#credentials-and-profiles).

`agm auth login --admin` exchanges `AGENT_GM_ADMIN_SECRET` for a session over
`--secret-stdin` or a TTY prompt — **never `argv`**. `--scopes` narrows the
session; `admin` is refused at `--scopes` on the OAuth path and can only come
from `--admin`. Slice 2 has only the admin path; the OAuth flow that
`--no-browser` belongs to arrives in Slice 3.

`agm auth logout` ends **this token's** session. It has nothing to do with
signing a Google account out, which is `agm accounts sign-out`.

### Admin

```text
agm admin settings list
agm admin settings get <key>
agm admin settings set <key>=<value>...
agm admin audit list [--account|--authorization|--kind|--kind-prefix|--after|--before|--limit|--all]
agm admin backfill [--account <acct-id>] [--conversation <conv-id>] [--yes]
agm admin backup
agm admin diagnostics [--account <acct-id>]
```

`agm admin settings set` sends the settings keys themselves as body fields;
they are validated against the settings registry, and an invalid one rejects
the whole request and changes nothing. The keys, their bounds and their scope
are in [operations.md](operations.md).

`agm admin backfill` with neither `--account` nor `--conversation` walks every
account — the most expensive thing Agent GM can be asked to do — so it requires
a confirmation.

`agm admin diagnostics` is the only place raw Google values are served.

### Admin — the OAuth flow

```text
agm admin enrollment-codes create <label> [--expires-in <duration>] [--scopes <list>] [--allow-scopes <list>]
agm admin enrollment-codes list
agm admin enrollment-codes show <enroll-id>
agm admin enrollment-codes revoke <enroll-id> [--reason <text>] [--yes]
agm admin authorization-requests list [--status pending|approved|denied|completed]
agm admin authorization-requests show <authreq-id>
agm admin authorization-requests approve <authreq-id> [--scopes <list>]
agm admin authorization-requests deny <authreq-id> [--reason <text>]
agm admin authorizations list [--include-revoked]
agm admin authorizations show <auth-id>
agm admin authorizations revoke <auth-id> [--reason <text>] [--yes]
agm admin clients list
agm admin clients show <client-id>
agm admin clients revoke <client-id> [--reason <text>] [--yes]
```

These are the owner's half of the OAuth flow, described end to end in
[oauth.md](oauth.md). **There is no self-service**: a connector cannot get a
token unless the owner does two separate things — issue a code with
`agm admin enrollment-codes create`, and approve the request it produces with
`agm admin authorization-requests approve`. There is no approval page; the
approval happens on a terminal, which is open question OQ-3's recorded answer.

`agm admin enrollment-codes create` prints the code **once**. Only its SHA-256
is stored, so no later command can show it again — and that is the point:
`agm admin enrollment-codes show` proves the value is not in the database.
`--scopes` replaces the default ceiling (`messages:read messages:write`),
`--allow-scopes` extends it, and the two are mutually exclusive. `admin` can
never be enrolled by either.

`agm admin authorization-requests approve --scopes` may only **narrow** what
the browser selected. Widening is refused, because the browser-selected set is
what the owner saw on screen next to the global-scope disclosure line; an
approval that could widen it would grant access the screen never described.

`agm admin authorizations revoke` revokes every token of an authorization at
once, so the client's next call is a `401` and it cannot refresh its way back.
`agm admin clients revoke` goes further and removes the registration too, after
revoking every authorization it holds.

`agm completion bash|zsh|fish` and `agm version` drive no route and are not
listed above. `agm version` prints the release version and the **source
commit** — `agm 1.0.0 (121648d…)`, or `{"version":…,"commit":…}` under
`--json`. Both are stamped from the tag at release time; a build from a
working tree says `dev` and reads the commit from the VCS stamp instead. The
server's own `agent-gm version` and `GET /v1/health` report the same two facts
plus the `libgm` pin, so a bug report can name exactly what was running.

## Destructive commands

These print the **exact `effect` sentence returned by the route** and require
an interactive `y`, or `--yes`:

| Command | What it does |
|---|---|
| `agm accounts remove` | **The only command that deletes an account's history** |
| `agm accounts sign-out` | Shreds a credential. Deletes nothing |
| `agm conversations delete` | Deletes Google's copy of the thread for you only |
| `agm messages delete` | Deletes Google's copy of the message for you only |
| `agm auth logout` | Revokes the calling authorization's tokens |
| `agm admin backfill` with neither `--account` nor `--conversation` | Re-backfills every account |

The prompt is not paraphrased. It is the string the route returned in its
`effect` field, byte for byte, and the same string is the MCP tool
description's closing sentence — so a human and a model cannot read a
different claim off two surfaces.

```console
$ agm messages delete msg_01k4z2p8vt
This deletes this message from your Google Messages account only; the recipient keeps it
Continue? [y/N]

$ agm conversations delete conv_01k4z2p8vq
This deletes this conversation from your Google Messages account only; the other people in it keep it
Continue? [y/N]

$ agm accounts remove acct_7f2a1c9d
This permanently deletes Agent GM's copy of this account's conversations, messages, attachments and operations; your Google Messages account and the messages in it are untouched
Continue? [y/N]
```

Those are all three sentences, in full, and they are the only three. Each is
compiled into the binary and checked against the specification's own text, so
the page, the prompt, the route's `effect` field and the MCP tool description
cannot drift apart.

Note what those sentences do **not** say. Deleting a message does not unsend
it. Only `agm accounts remove` deletes anything Agent GM stored, and even then
audit rows survive.

## Credentials and profiles

Tokens are stored per server in
`$XDG_STATE_HOME/agent-gm/credentials.json` — in practice
`~/.local/state/agent-gm/credentials.json` — at mode `0600` inside a `0700`
directory, written atomically under a `credentials.lock` advisory lock.
Override the path with `--credentials-file` or `AGENT_GM_CREDENTIALS_FILE`.

**A profile is a server you have logged in to.** `agm auth login --server <url>`
creates one, names it after the server's **host**, and records it as the
active profile:

```console
$ agm auth login --server https://gm.example.test
Logged in to https://gm.example.test
Scopes: messages:read messages:write

$ agm profiles list
* gm.example.test                  https://gm.example.test
```

After that, **every command needs no `--server`**: it uses the active profile.

```text
agm profiles list             # every stored profile; `*` marks the active one
agm profiles use <name>       # make one the active profile
agm profiles remove <name>    # forget one locally
```

Pass `--profile <name>` (or set `AGENT_GM_PROFILE`) to use a different profile
for **one invocation**, without changing which one is active. `agm profiles`
drives no route, so it answers even when the server is down.

There is **no profile called `default`** and **no compiled-in hostname**. A
credentials file left over from an older build that carries a profile named
`default` is migrated the first time this build reads it: the profile is
renamed to its server's host and becomes the active one, and every other field
is preserved. A file with no `active_profile` and exactly one profile treats
that one as active.

`agm profiles remove` never leaves `active_profile` naming a profile that is
gone: with one profile left, that one becomes active; otherwise no profile is
active and the next bare command says so rather than guessing.

`agm auth logout` revokes at the *server* and forgets the profile.
`agm profiles remove` only forgets it locally.

### Which server, with what

| Source | Used for |
|---|---|
| `--server`, else `AGENT_GM_URL`, else the **active profile's** server | Which server a command talks to |
| `AGENT_GM_ACCESS_TOKEN` | One access token, used as given. Not refreshable |
| `AGENT_GM_REFRESH_TOKEN_FILE` plus `AGENT_GM_CLIENT_ID` | Automation. Every invocation exchanges the token and rewrites the file with the rotated value, atomically, at mode `0600` |
| The stored profile | The ordinary case, written by `agm auth login` |

If none of the three resolves a server, the command fails with exit `9` and
says what to type. It never reaches a built-in host, because there is not one.

Each profile is bound to an exact server, issuer and resource, and the binding
is enforced **before** the request: on any command except `agm auth login`, a
`--server` naming a server no profile holds is refused as `invalid_request`
(exit `2`), naming `agm auth login --server`. One profile's token is never
forwarded to another origin. `agm auth login` is the exception, because it is
where a profile comes from.

### Refreshing

A stored profile **refreshes itself**. The profile records the access token's
`expires_at`, and `agm` exchanges the refresh token when the token is within
**60 seconds** of that instant, or on the first `invalid_token` from the
server, whichever comes first. **A token past its recorded expiry is never
presented.** The rotation is written back to the profile atomically, at mode
`0600`, and the command carries on; a mutation that was refused before it was
applied is retried with the **same** `Idempotency-Key`, so it stays one
logical request.

An admin session refreshes at `POST /v1/auth/refresh`; an OAuth profile
refreshes at `/oauth/token` with `grant_type=refresh_token` and the
`client_id` the profile records. **The two paths do not cross** — a token
presented at the other endpoint is unknown or `invalid_grant`. A profile
written by an older build that records no expiry is covered by the on-refusal
path.

Exit `3` therefore means **a refresh was attempted and refused** — a revoked
or expired refresh token, or a profile that holds none — and not merely an
expired access token. Log in again.

**Write-back safety**, which is the part worth knowing before an automated run:

- The destination is proved writable **before the token is spent** — the
  temporary file the write later renames into place is created first.
- An unwritable directory is **exit `9` while the token is still good**.
  Nothing was consumed, so fix the machine and run again.
- A token the server has already refused is **exit `3`, and is never retried**.
  Log in again.

Rotation means a refresh token is single-use: a run that exchanges one and then
cannot store the replacement has lost it. That ordering is why the check comes
first.

## A worked example

Fictional numbers throughout. Nothing here dials anybody.

```console
$ agm auth login --admin --server https://gm.example.test --secret-stdin < secret.txt
Logged in as authorization auth_01k4z2p8w8
Scopes: admin messages:read messages:write messages:delete

$ agm conversations list --limit 2
CONV ID           NAME    TYPE     UNREAD  LAST ACTIVITY
conv_01k4z2p8vq   Alex    rcs      yes     2026-09-06T09:41:02Z
conv_01k4z2p8vy   Sam     sms_mms  no      2026-09-05T18:20:44Z

$ agm messages send conv_01k4z2p8vq --text "on my way" --wait
op_01k4z2p8vv
status: succeeded  message: msg_01k4z2p8vt  delivery: sent

$ agm messages send conv_01k4z2p8vq --file ./IMG_0421.jpg --text "the photo" --wait
op_01k4z2p8w9
status: succeeded  message: msg_01k4z2p8wa  delivery: sent

$ agm messages add-reaction msg_01k4z2p8vt 👍
op_01k4z2p8wb
status: succeeded

$ agm messages list conv_01k4z2p8vq --limit 2 --json | jq '.data.items[].text'
"the photo"
"on my way"
```

Starting a thread with someone not yet in one, on a deployment with two
accounts paired — so `--account` is required:

```console
$ agm conversations start +12025550123 --account acct_01k4z0aa --wait
op_01k4z2p8wc
conversation: conv_01k4z2p8wd
status: succeeded
```

Omitting `--account` there would be exit `2`, and the error would list the
accounts to choose from, so the retry needs no second lookup.
