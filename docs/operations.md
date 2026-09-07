# Operations

Serves spec §3.6, §11.2, §12.1, §12.2, §12.3, §15.1–§15.6, and decisions
D32 and D33.

This is the page to read at 3am. It says what each knob does, what breaks
when it is wrong, and what to do about the handful of things that go wrong in
practice.

## Configuration

Configuration is **environment only, plus a runtime settings table**. There is
no config file, and there is no plan for one.

### Environment

| Variable | Default | Meaning |
|---|---|---|
| `AGENT_GM_PUBLIC_URL` | *(required)* | The public origin, e.g. `https://gm.agent-wx.app`. It is the OAuth issuer, the canonical resource, and the base of every URL Agent GM hands out. **Never derived from the `Host` header** — see "changing the public URL" below, because changing it is a migration |
| `AGENT_GM_LISTEN_ADDR` | `0.0.0.0:8080` | Bind address |
| `AGENT_GM_DATA_DIR` | `/data` | Holds `agent-gm.sqlite3`, `sessions/` (one file per account), `media-cache/` and `backups/` |
| `AGENT_GM_ADMIN_SECRET` | *(required)* | The owner's bootstrap credential. Must be at least 43 characters; the server refuses to start with a shorter one |
| `AGENT_GM_DATA_KEY` | *(required)* | 256 bits, as 64 hex characters or standard base64. **Not rotatable** |
| `AGENT_GM_TRUSTED_PROXY_CIDRS` | empty | Which peers may assert `X-Forwarded-For`. An invalid value **refuses to start** |
| `AGENT_GM_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `AGENT_GM_LOG_FORMAT` | `json` | `json` \| `text` |
| `AGENT_GM_UNSAFE_TRACE` | unset | `1` permits `libgm` trace logging, which base64-logs decrypted message payloads. See "logging" |
| `AGENT_GM_BACKEND` | `libgm` | `libgm` \| `fake` |
| `AGENT_GM_ALLOW_FAKE` | unset | `1` permits `AGENT_GM_BACKEND=fake`. Without it a `fake` backend refuses to start, so a production deployment cannot be talked into serving an empty in-memory phone |
| `AGENT_GM_LIVE_NUMBERS` | unset | Live-gate targets, `direct,group1,group2`. Read only by `-tags live` tests, never committed |
| `AGENT_GM_PAIRING_TIMEOUT` | `5m` | How long a started pairing may sit unconfirmed before Agent GM discards the half-built session and deletes any `pairing` row. Distinct from upstream's own 20-second init timeout and from Google's `pairing_timeout` |
| `AGENT_GM_ACCOUNT` | unset | CLI only. Default `acct_` ID for `--account` |
| `AGENT_GM_CHROME` | unset | CLI only. Path to a Chrome or Chromium binary for pairing; searched **before** the platform's usual locations and `PATH` |
| `AGENT_GM_URL`, `AGENT_GM_ACCESS_TOKEN`, `AGENT_GM_REFRESH_TOKEN_FILE`, `AGENT_GM_CLIENT_ID`, `AGENT_GM_CREDENTIALS_FILE` | — | CLI credential handling only |
| `AGENT_GM_CLI_SKIP_DOWNLOAD`, `AGENT_GM_CLI_BINARY` | — | npm wrapper only |

### Runtime settings

These are mutable through `PATCH /v1/admin/settings` (`agm admin settings
set`). Each reports its effective value, its source — `default`,
`environment` or `database` — its mutability and whether it needs a restart.

**Read the scope column before you change anything.** `server` means one
budget for the whole process. `per account` means the value is applied to
each account **independently**, so it multiplies by the number of accounts.
This is exactly how a two-account deployment ends up doing twice the work it
was configured for: `backfill.concurrency` of 2 with eight accounts connected
is sixteen concurrent backfills, not two.

| Key | Default | Bounds | Scope |
|---|---|---|---|
| `backfill.concurrency` | 2 | 1–8 | **per account** — worst case is this × `accounts.max_concurrent` |
| `backfill.conversation_page_size` | 100 | 10–500 | per account |
| `backfill.message_page_size` | 100 | 10–500 | per account |
| `backfill.max_messages_per_conversation` | 2000 | 100–100000 | per account |
| `backfill.horizon` | `365d` | `7d`–`3650d` | **per account** — the cost multiplies by account count |
| `backfill.include_archive` | `true` | — | per account |
| `ingest.sweep_interval` | `15m` | `1m`–`6h` | **per account** |
| `accounts.max_concurrent` | 8 | 1–32 | server |
| `operations.send_deadline` | `300s` | `60s`–`600s` | server |
| `operations.idempotency_ttl` | `30d` | `1d`–`365d` | server |
| `operations.pending_timeout` | `24h` | `1h`–`7d` | server |
| `operations.wait_timeout` | `60s` | `5s`–`10m` | server |
| `media.upload_max_bytes` | `104857600` | mutable downward only | server |
| `media.cache_max_bytes` | 2 GiB | — | server — the LRU is cross-account |
| `media.inline_mcp_image_max_bytes` | 1 MiB | — | server |
| `oauth.*`, `admin.*` | see spec §9.6 | | server |
| `backup.keep` | 7 | 1–100 | server |
| `logging.level` | `info` | reloadable without restart | server |

A retired key becomes an unknown key: setting one answers `invalid_request`
naming its replacement, rather than silently writing a key nothing reads.

Durations everywhere are integer plus unit, `s|m|h|d`.

## The two required secrets

Generate both with:

```console
$ devbox run gen-secret
```

which is `openssl rand -hex 32` — 64 hex characters, 256 bits. Generate them
separately; do not reuse one for the other.

| Secret | Rotatable |
|---|---|
| `AGENT_GM_ADMIN_SECRET` | **Yes.** Changing it revokes every previous admin bootstrap authorization on the next start. It must be at least 43 characters and lives in the environment only |
| `AGENT_GM_DATA_KEY` | **No.** Not rotatable, in place or otherwise |

**The data key is not rotatable, and it moves with the data directory
always.** It derives, by HKDF-SHA256 with distinct info strings, the key that
seals every session file, the key that encrypts every attachment decryption
key, the key that signs media tickets, and the key that signs pagination
cursors. One key covers every account; there is no per-account key.

Rotation would mean re-encrypting every session and every attachment key
atomically across a restart, and it is not implemented — that is a recorded
decision, not an oversight. The operational consequences are:

- A database restored **without** the key that sealed it cannot decrypt any
  session file or any attachment key.
- Starting with the wrong key fails with `session envelope cannot be
  decrypted`. Restore the original key. There is no in-place rotation and no
  recovery command.
- If the key is genuinely lost: delete `sessions/`, re-pair each account, and
  re-backfill. **Message text survives**, because it is not encrypted at
  rest. Cached media and the sessions do not.

Neither secret ever appears in `argv`. The admin secret is presented over
`--secret-stdin` or a TTY prompt, never as a flag value.

## Backup and restore

`agm admin backup` (`POST /v1/admin/backup`) writes
`<data_dir>/backups/agent-gm-<timestamp>-<id>.sqlite3` using the **SQLite
backup API**, not a file copy. Pages are copied under the database's own
locking and the copy restarts if a writer changes a page it has already
taken, so the result is a usable database while the server keeps serving. The
snapshot has no `-wal` sidecar and opens on its own. The caller does not
choose the path.

**Retention counts calls, not days.** After each successful backup the newest
`backup.keep` snapshots are kept and older ones deleted, each removal
audited. An hourly cron with `backup.keep=7` keeps **seven hours**, not seven
days — set the schedule and the count together. Pruning never touches a file
not named `agent-gm-<timestamp>-<id>.sqlite3`, and a file it cannot delete is
logged and left, rather than failing a backup that had already succeeded.

### A complete backup is three things, and they move together

1. **The snapshot** (or the whole `data/` directory).
2. **The whole `sessions/` directory** — one file per account.
3. **`AGENT_GM_DATA_KEY`.**

Without (3), (2) is unreadable and so is cached media. Restore means putting
all three back.

### A backup of `sessions/` is a credential backup

Say it in full, because half-saying it is how it gets stored in the wrong
place: **a backup of `sessions/` is a backup of the owner's Google account
credentials — all of them.** Not "a Messages session". The gaia flow keeps
live Google account cookies for the life of each pairing, so anyone holding
the data directory **and** the data key can act as **every** account the
server holds. One key covers all of them, so the blast radius grows with each
account added.

Handle it the way you handle a credential, and **never store it in the same
place as `AGENT_GM_DATA_KEY`**. There is no pairing flow that stores anything
less, so this is the standing handling rule, not a per-flow caveat.

### Restoring without `sessions/`

If (1) and (3) survive but `sessions/` does not, **every account comes back
`signed_out` with its history intact.** That is a safe state, not a
corruption:

- Every conversation, message, attachment, reaction and contact is still
  there, readable and searchable.
- Only writes are refused, per account, with `unsupported_capability` and
  `details.reason = "not_signed_in"`.
- The recovery is `agm pair` for each account — nothing else.

Re-pairing keeps every existing `conv_` and `msg_` ID, **including onto a
different phone**, because the account identifier is derived from the Google
account address rather than from the device. That is the whole reason the
identifier was chosen that way.

## Health and diagnosis

There are two health endpoints and they answer different questions.

| Endpoint | Scope | Question it answers |
|---|---|---|
| `GET /healthz` | none | *Is the process serving?* Returns `200 {"status":"ok"}` whenever it is. Never touches SQLite, so it is safe as a container liveness probe |
| `GET /v1/health` | `messages:read` | *What is the state of everything?* `version`, `commit`, `source_url`, `config_version_compiled`, `upstream_commit`, an `accounts_summary` of totals by state, an `accounts[]` array with per-account detail, `pending_reprocess`, and the `client_source` this request resolved to |

Each entry in `accounts[]` carries that account's `state`, `state_reason`,
`phone_responding`, a `google` block (`config_version_live`,
`config_version_stale`, `is_default_sms_app`), `backfill`, `sweep` and
`counters`. The `google` block is `null` for an account that is not
`connected`; its values are cached from the last config fetch and refreshed
on connect and every `ingest.sweep_interval`, so `/v1/health` never blocks on
a phone.

`pending_reprocess` names the one-off task a migration deferred, while it is
still running, and is `null` the rest of the time — which is almost always. It
runs behind the listener, so on the one start after such an upgrade a query
filtered on a value it is still rebuilding (`sender=me` is the one that
matters) can return fewer rows than it will a moment later. If a page looks
short right after an upgrade, read this field before reading anything else.

**`status` describes the server, not the accounts.** This is the single most
important thing to know about monitoring Agent GM:

- An account in `signed_out` or `error` is a fact about **that account**,
  reported in its row and in `accounts_summary`. It does **not** make Agent
  GM unhealthy.
- **A deployment with zero accounts is `ok`.** A server with no accounts is a
  healthy server with no accounts; it simply cannot send.
- Anything watching for "is the service up" reads `status`. Anything watching
  for "can I send" reads the **account**.

An alert wired to `status` for account-level problems will never fire. An
alert wired to `accounts_summary` for process death will fire late. Wire
both, to the right thing.

The fast operator views:

```console
$ agm accounts list                       # every account and its state
$ agm health                              # the /v1/health view
$ agm session --account <acct-id> --watch # stream one account's state changes
$ agm session --watch                     # stream every account's
$ agm admin diagnostics                   # the bundle to attach to a bug report
```

Every account also carries `state_reason` — one of `capacity`,
`listen_error`, `credentials`, `revoked_by_phone`, `cookies_expired`,
`account_switched`, `crash_recovered`, or `null` — so `degraded` and `error`
are diagnosable without reading logs.

## Runbook

| Symptom | Cause | Fix |
|---|---|---|
| `session envelope cannot be decrypted` at startup | `AGENT_GM_DATA_KEY` differs from the key that sealed `sessions/*.enc` | Restore the original key. **There is no in-place rotation.** The server still starts and still serves: each account whose session will not open is marked `signed_out` / `credentials`, with its history intact, and the rest resume normally |
| Every write is `not_paired` | There are **no accounts at all** | `agm pair`. A server with zero accounts is healthy; it just cannot send |
| A write is refused `unsupported_capability` / `not_signed_in` | **That one account** is not usable; the rest may be fine | Read the account's `state` in `agm accounts list` and follow the matching row below |
| An account goes to `error` with `RevokePairData` in the audit log | That phone revoked the pairing | `agm pair --account <acct-id>`; history resumes |
| Starting a conversation fails and `agm health` shows `config_version_stale: true` | The pinned `libgm` `ConfigVersion` is older than Google's | **Bump the pin** as its own slice with its own live gate. Retrying does not help |
| Starting a conversation fails with `google_undocumented_status` and the ConfigVersions **match** | Google returned a status the pinned proto has no name for | Record `details.status` and the request, and report it upstream. **Do not invent a meaning** |
| One account is `signed_out`, others fine | Its cookies expired, or the owner signed it out | `agm pair --refresh-cookies --account <acct-id>`, or `agm pair` again. Its history was never touched |
| An account's Google address changed | The owner renamed the Google account | A re-pair creates a **second** account. Confirm, then `agm accounts remove` the stale one — that is the only command that deletes |
| Paired against the wrong Android phone | The account has several and the library picked the most recently seen | `agm pair --device-index 1` |
| An account sits in `parked` | `accounts.max_concurrent` is reached, so it holds no client and no goroutine. **Not an error and not transient**; `state_reason` is `capacity` | Raise `accounts.max_concurrent`, or sign out an account that is no longer needed. `agm accounts list` shows which accounts hold the slots |
| An account is `account_changed` and refuses writes | The phone switched to a different Google account underneath the pairing | Decide which account the phone should serve. To keep the original, switch the phone back and `agm pair --refresh-cookies --account <acct-id>`. To adopt the new one, `agm pair` it as a **new** account; the old account's history stays readable and is deleted only by `agm accounts remove` |
| `agm pair` reports no Chrome found | No Chrome or Chromium on this machine | Use `--server` from a machine that has one, or `--paste`. Set `AGENT_GM_CHROME` if Chrome is in an unusual place. Exit code is `9`, not `2` |
| Every send is `not_default_sms_app` | Google Messages is not the phone's default SMS app | Change it on the phone. `agm health` reports `is_default_sms_app` per account |
| A group send fans out as separate SMS threads | The phone's *Group messaging* is set to "Send an SMS reply to all recipients" | Set it to MMS on the phone. Agent GM can only report what Google returns |
| Sends time out with `phone_not_responding` | The phone is asleep or offline | Wait. The operations are `pending`, not failed, and the echo will settle them. **Do not resend** — Google already accepted the request and the phone may still act on it, so a resend is a duplicate text to a real person |
| `dropped_events` climbing in `counters` | Ingest is slower than the event stream | This is a bug. Capture `agm admin diagnostics` and file it |
| Every caller collapses to one source in the audit log | `AGENT_GM_TRUSTED_PROXY_CIDRS` does not cover the proxy or tunnel container, so every request is attributed to the proxy's own address | Fix the CIDR. Until you do, **every per-source rate limit is one shared bucket** and every audit row names the proxy instead of the caller. Compare `client_source` in `/v1/health` against the address you came from |
| The process refuses to start naming a CIDR | A mistyped proxy list | Fix it. This is deliberate: an operator who mistypes the list should find out immediately, not discover months later that the trust they configured was never in force |
| A WAL checkpoint reports `busy` | A long-lived reader | Not an error. The next pass finishes it |

**Hourly upkeep**, which Agent GM runs itself: optimise the FTS index, **then**
checkpoint the WAL with `TRUNCATE`, in that order — `optimize` writes, so
checkpointing first would leave its pages in the log for the next pass to
carry. The pass measures the log **file size**, not pragma page counts,
because a successful `TRUNCATE` resets the log and therefore reports zero
pages checkpointed, which is indistinguishable from having done nothing.

## `config_version_stale` is informational, not a fault

This one deserves its own section because it looks alarming and usually is
not (D32).

Google ships a new `ConfigVersion` on its own schedule. Agent GM's is
compiled in, pinned to a `libgm` commit. So the two drift apart the moment
Google moves, and **`config_version_stale: true` is the normal resting state
between pin bumps.** It does not change `/v1/health`'s `status`, it is not an
error, and on its own it calls for no action at all.

There are exactly three cases:

1. **Everything works, `config_version_stale` is `true`.** Do nothing. Note
   it and carry on. The Slice 1 live gate paired, listed, sent and received
   an echo with a live version ahead of the pin.

2. **A conversation-creating call fails *and* the versions differ.** The
   error is still Google's own status — `google_error` or
   `google_undocumented_status` — and the two versions arrive in `details` as
   **context**, with a sentence saying a pin bump is worth trying. There is
   no `config_version_stale` error code: it was retired because relabelling
   every failure that happened to coincide with a version difference told an
   operator to bump the pin for failures that had nothing to do with the pin,
   and threw away the status that did fail. A pin bump is a reasonable next
   move here, as its own slice with its own live gate, but **retrying the
   same request is not** — it will carry the same version next time.

3. **A conversation-creating call fails and the versions match.** Then the
   version is not even context. It is `google_undocumented_status`: Google
   returned a status the pinned proto has no name for. Record
   `details.status` and the request and report it upstream. **Do not invent a
   meaning for the integer** — it is a
   diagnostic value with no Agent GM meaning, published so you have something
   to search for.

The distinction is why the boolean exists at all. Treating a version
difference as an error would fail a healthy deployment; hiding it would
remove the only diagnosis there is for a conversation-creating failure.

## Upgrading

Migrations are the whole mechanism. Pull the new image tag **by digest**,
recreate the container, and the process migrates before it binds.

- **Downgrade is not supported.** A database at a higher `user_version`
  refuses to open. Keep a snapshot before an upgrade if you want a way back.
- A migration that adds a column every existing row needs sets a
  pending-reprocess marker and derives the values once after startup, rather
  than shipping a hand-written data migration.

### The `libgm` pin policy

Agent GM pins `go.mau.fi/mautrix-gmessages` to commit **`be48a58`**
(`ConfigVersion` 2026-09-02). That pin is a fact recorded in three places
that must agree — `go.mod`, `internal/gm/pin.go`, and spec §3.6 — and CI
fails if they diverge. `GOFLAGS=-mod=readonly` is set so an accidental
`go get` cannot silently move it.

**Bumping the pin is never part of a routine upgrade.** It is a deliberate
slice that must:

1. update all three places;
2. diff `pkg/libgm` and `pkg/connector` between the old and new commits, and
   record in `docs/upstream-pin.md` every change to a symbol Agent GM uses,
   every change to `ConfigVersion`, and every added, removed or renamed enum
   value in the delivery-state or event vocabularies;
3. re-run the fixture-validation job;
4. **pass a live gate** — pair, list, send one text to the approved direct
   number, receive a reply — run by the coordinator, not by an implementer.

A bump that changes `ConfigVersion` is expected to be urgent, because a stale
`ConfigVersion` breaks conversation creation with an undocumented status and
no other symptom.

## Logging

### What is redacted

Structured logs carry request IDs, operation IDs, coarse event types,
latency, error codes and state transitions. Redaction is mandatory and covers:

- `Authorization` headers and bearer tokens;
- the admin secret and the data key;
- the Google tachyon token, refresh key, request-crypto keys, and **every one
  of the seven Google account cookies by name** — including inside error
  strings bubbled up from `libgm`;
- enrollment-code values and OAuth authorization codes;
- message bodies, subjects and attachment bytes;
- **full phone numbers** — logs carry a stable salted hash and the last four
  digits, never the number itself;
- **Google account addresses** — logs carry the `acct_` ID, never the
  address. The address is served on `/v1/accounts` and `/v1/health`, because
  the owner needs to tell their accounts apart, and nowhere else;
- OAuth form bodies.

The acceptance test is a scan: the whole flow is run with sentinel values and
none of them may appear in captured logs, in `agent-gm.sqlite3`, in its
`-wal` or `-shm`, or in any audit payload.

### Two loggers, and neither ever writes to stdout

Agent GM's own logger and the one handed to `libgm` are separate, and **both
write to stderr, always.** Stdout carries a command's result — a table, or
the single JSON value `--json` promises — so a log line in stdout is a parse
error for anything downstream. If you are piping `agm --json` into `jq`, this
is the guarantee you are relying on.

`libgm`'s logger is tagged **`component=libgm`** and is **floored at
`warn`**, and a one-shot CLI command silences it altogether. Upstream's
narration of the long poll is a commentary on a protocol Agent GM does not
control, is not a diagnosis you can act on, and printed inline with a
command's output at the Slice 1 live gate. **`AGENT_GM_LOG_LEVEL=debug` does
not lower that floor.**

### The one way past the floor

At `trace`, `libgm` base64-logs decrypted payloads — that is message content
in your logs. So `trace` is **refused** unless `AGENT_GM_UNSAFE_TRACE=1` is
set. That variable is the only thing that goes past the `warn` floor, and
when it is used:

- every line is stamped `unsafe_trace=true`;
- one `security.unsafe_trace_enabled` audit row is written at startup.

Use it to diagnose a protocol problem, then unset it and rotate the logs it
produced.

## Rate limits and trusted proxies

### The limits

| Surface | Limit |
|---|---|
| Reads (`messages:read`) | 300 req/min per authorization, burst 100 |
| Mutations (`messages:write`, `messages:delete`) | 120 req/min per authorization, burst 30 |
| `/v1/admin/*` | 120 req/min per authorization, burst 30 |
| Admin-secret failures | 5 / 15 min per source, 20 / 15 min globally, exponential cooldown |
| Enrollment-code failures | 10 / 15 min per signed OAuth context and per source |
| Dynamic client registration | 20 / hour per source |
| Unauthenticated `/oauth/token` refresh and `/oauth/revoke` | 60 req/min per source, burst 20; plus 30 invalid tokens / 15 min, durable |
| Concurrent uploads | 4 per authorization, 16 globally |
| Concurrent downloads | 8 per authorization, 32 globally |
| `/mcp` in flight | 8 per authorization, 32 globally |

Exceeding one is `rate_limited` (429) with `Retry-After`. Ordinary traffic
uses token buckets; the credential-failure limiters keep durable cooldown
metadata, so a restart does not clear a cooldown.

**Holding several scopes does not multiply the allowance, and neither does
holding several accounts.** The buckets are per authorization and per source,
never per account. One authorization sending to three accounts shares one
120/min mutation budget, so **adding an account divides the per-account send
rate rather than adding to it.** That is deliberate — the limits protect the
server and the phones from a runaway agent, and an agent is no less runaway
for spreading itself across accounts — but know it before you add the fourth
account.

### Client source

Every per-source limit, and the `source` field of every audit record, uses
the resolved client source:

| `AGENT_GM_TRUSTED_PROXY_CIDRS` | Source |
|---|---|
| unset or empty (default) | The TCP peer. Forwarded headers are ignored entirely |
| a comma-separated CIDR list | If the TCP peer is inside the list, the **rightmost** `X-Forwarded-For` entry that is not itself inside the list; otherwise the TCP peer |

The rules that matter:

- **The walk is right to left.** Taking the leftmost entry is the usual bug,
  and it would turn every per-source limit in the system into decoration,
  because the leftmost entry is whatever the client claimed.
- An unparseable entry **stops** the walk and falls back to the TCP peer. It
  is not skipped.
- Entries may be CIDRs (`127.0.0.1/32`, `10.0.0.0/8`, `::1/128`) or bare
  addresses read as host routes. An IPv4-mapped IPv6 peer
  (`::ffff:127.0.0.1`) matches an IPv4 CIDR.
- **An invalid value refuses to start.** You should find out immediately, not
  discover months later that the trust you configured was never in force.
- The resolved source is never the empty string.
- `X-Forwarded-Proto` and `X-Forwarded-Host` are recorded but **never build a
  URL**. The issuer, the canonical resource, both discovery documents, every
  redirect and every media URL come from `AGENT_GM_PUBLIC_URL` alone.

A typical deployment behind a tunnel that terminates TLS and connects over
plain HTTP from inside a container network sets
`AGENT_GM_TRUSTED_PROXY_CIDRS` to that network. Startup logs
`client_source_mode=socket_peer` or `=trusted_proxy` once, and
`GET /v1/health` reports the `client_source` **this** request resolved to —
so the misconfiguration where every caller collapses to one source is
visible rather than silent.

**With a trusted proxy configured, the listening port becomes a trust
boundary.** Any process that can reach it can assert an arbitrary
`X-Forwarded-For` and choose its own source — which means choosing its own
rate-limit bucket and its own audit attribution. Do not configure a trusted
proxy and then expose the listening port more widely than the proxy.

## Exit codes

`agm` exit codes, for scripting. `1` is deliberately unassigned, so a `1` from
a wrapper is the wrapper's own, never Agent GM's.

| Code | Meaning | Typical script response |
|---|---|---|
| `0` | Success | — |
| `2` | CLI usage or validation error, including `idempotency_conflict` (the same key reused with a different body) | Fix the invocation. Never retry unchanged |
| `3` | Authentication required, or expired credentials | `agm auth login` again. Do not retry the original |
| `4` | Authorization or insufficient scope | Get a token with the scope. Do not retry |
| `5` | Requested resource absent | Do not retry |
| `6` | Unsupported capability for this account, conversation or message — including `not_signed_in`, an **account**-level condition | Read the reason; usually needs an action on the account or the phone |
| `7` | Retryable network, Google, phone or rate-limit failure — including `phone_not_responding` and `pairing_init_timeout` | Back off and retry. **For `phone_not_responding` the send is `pending`, not failed: print the operation ID and wait, do not resend** |
| `8` | An operation reached a terminal failure while waiting | The send failed. Decide at the application level |
| `9` | Local configuration or credential-store failure — including "no Chrome found" and an unwritable credentials directory | Fix the machine. A `9` from a credential write means **the token is still good**; nothing was spent |
| `10` | Server contract or internal failure — including `not_paired`, the `pairing_*` codes other than `pairing_init_timeout`, `not_default_sms_app`, `google_error`, `google_undocumented_status`, `google_permission_denied` and `internal_error` | Do not blind-retry. Read the code |

A command that times out waiting returns `7`, prints the operation ID, and
leaves it available to `agm operations wait <op-id>`.

## Changing the public URL

**This is a migration, not a config edit.** Changing `AGENT_GM_PUBLIC_URL`:

1. invalidates every access and refresh token, because they are
   audience-bound to the old origin;
2. orphans every registered OAuth client, because a client's redirect and the
   issuer it verified are tied to the old origin;
3. means every connector — claude.ai, ChatGPT — must be **removed and re-added
   by hand**, each requiring a fresh enrollment code and a fresh approval.

The procedure, in order:

1. Issue new enrollment codes.
2. Change the variable.
3. Restart.
4. Re-add each connector.
5. Revoke the stale authorizations and clients.

There is no way to make old tokens keep working, and no attempt should be
made. Plan the URL before the first connector is enrolled.
