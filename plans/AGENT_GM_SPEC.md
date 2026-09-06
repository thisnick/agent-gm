# Agent GM — Specification

Status: design, pre-implementation. Version 1.0 (2026-09-06).

This file is the contract. Where the code and this file disagree, the code is
wrong. An implementer should be able to build any slice in §16 without asking a
question; a reviewer should be able to accept or reject a slice by checking it
against this file alone.

`AGENT_GM_PUBLIC_URL` is the public HTTPS origin Agent GM is reachable at.
**It is `https://gm.agent-wx.app`** — a Cloudflare Tunnel whose origin is
`agent-gm:8080` over plain HTTP. Every absolute URL in this spec is
`AGENT_GM_PUBLIC_URL` plus a path.

> **This value is load-bearing and must not change.** It is the OAuth issuer
> and the canonical resource indicator (§9). It is baked into every dynamic
> client registration a connector performs. Changing it invalidates every
> issued token and every registered client, and claude.ai / ChatGPT connectors
> would have to be removed and re-added by hand. Treat a change of hostname as
> a migration, not a config edit (§15.6).

---

## 1. Goals and non-goals

### 1.1 What this is

Agent GM is a single Go binary that gives **one owner's agents** programmatic
access to **their Google Messages accounts** — one or several — each paired
directly with a phone.

It talks to Google Messages by importing the client library from the
mautrix-gmessages bridge, `go.mau.fi/mautrix-gmessages/pkg/libgm`, pinned to a
specific upstream commit (§3.6). It keeps its own state in a single SQLite
database. It exposes three surfaces over the same core:

| Surface | Transport | Audience |
|---|---|---|
| MCP | streamable HTTP at `/mcp`, OAuth 2.1 bearer | claude.ai and ChatGPT connectors, local agents |
| REST | JSON over HTTPS under `/v1` | scripts, the CLI, anything that is not an MCP client |
| CLI | `agm`, a local binary that speaks the REST API | the owner at a terminal, and cron |

### 1.2 Goals

- **G1 — One owner, several accounts.** Pairing is a first-class, documented
  and recoverable operation (§3.2, §11.4), and it is repeatable: `agm pair`
  **adds** an account. Each account has its own `libgm` client, session file,
  event stream, backfill and reconciliation sweep, and they run concurrently
  (§4.7). Everything an agent reads or writes names an account, explicitly or
  by default when there is only one.
- **G2 — Agents are the primary consumer.** Every name, description, error
  message and tool schema is written for a *cold* agent that has never seen this
  server, has no memory of prior calls, and cannot ask a human (§8.3).
- **G3 — Reads are cheap and complete.** Conversations and messages are ingested
  into SQLite and served from there. A read never depends on the phone being
  awake.
- **G4 — Writes are honest.** A send is a synchronous call into the library with
  a real result. Agent GM never claims a message was sent when it was only
  queued (§6).
- **G5 — Public-connector-ready.** OAuth 2.1 with dynamic client registration,
  PKCE, resource indicators and issuer identification, because claude.ai and
  ChatGPT connectors require them (§9).
- **G6 — Operable by one person.** Backup is copying two files. Restore is
  copying them back. Upgrade is pulling one image tag (§15).
- **G7 — Reviewable.** Every claim about Google's behaviour in this spec cites
  a file and symbol in the pinned upstream source, and every fixture in the test
  suite is validated against that same pinned source (§13.4).

### 1.3 Non-goals

These are permanent. A change request that reopens one of these is closed, not
debated.

- **N1 — No Matrix.** No Synapse, no bridge process, no appservice, no rooms,
  no portals, no portal generations, no invitations, no E2EE, no sync tokens, no
  `/_matrix` anything. Agent MX's entire Matrix layer is deleted, not ported.
- **N2 — No human UI.** No web app, no chat client, no HTML beyond the two
  OAuth pages that the spec requires (§9.6). The owner reads their messages in
  the Google Messages app on their phone.
- **N3 — No second provider.** There is no provider abstraction, no
  `provider=` parameter, no pluggable transport. The only network is Google
  Messages. The `gm` package interface (§2.2) exists for *testing*, not for
  future providers, and the spec forbids adding a second production
  implementation.
- **N4 — No multi-tenancy.** One **owner**, however many Google accounts. There
  is no user table and no tenant column; authorization is still the single
  question "is this token the owner's?" plus scopes. Multiple accounts are the
  owner's own accounts, not other people's, and a token that can read one can
  read all of them (D29 — per-account scoping is deferred, not refused).
- **N5 — No outbox, no reconciliation loop.** Agent MX needed an outbox because
  it had to reconcile two systems of record (Matrix and Google). Agent GM has
  one. Sends are synchronous (§6.1).
- **N6 — No delete modes other than Google's.** `messages.delete` maps to
  `libgm.Client.DeleteMessage`, which is Google Messages' own delete. There is
  no "delete for everyone", no local-only tombstone, no redaction vocabulary.
  (Google's own in-thread system events are stored as `kind='system'`, §4.4 —
  "tombstone" is Matrix vocabulary and does not appear on any surface.)
- **N7 — No group creation choreography.** Agent MX had a multi-step
  group-start protocol because Matrix rooms had to exist before Google
  conversations did. Agent GM creates a conversation with one call
  (`GetOrCreateConversation`) or not at all (§7.4).
- **N8 — No inbound webhooks or push fan-out to third parties.** Events are
  ingested into SQLite and read back. There is no subscriber registry.
- **N9 — No message editing.** Google Messages has no edit primitive that
  `libgm` exposes for outgoing messages; the bridge's "edit" is a re-render of a
  changed status. Agent GM surfaces status changes (§5.5) and nothing else.

### 1.4 Licence consequence — stated once

Agent GM imports `go.mau.fi/mautrix-gmessages/pkg/libgm`, which is licensed
**AGPL-3.0-or-later**. `LICENSE.exceptions` in the upstream repository grants
embedding exceptions only to Beeper and Element; neither covers Agent GM.
Therefore:

- Agent GM is **AGPL-3.0-or-later**, and the repository ships the AGPL text as
  `LICENSE`.
- Because Agent GM is reachable over a network at `AGENT_GM_PUBLIC_URL`, AGPL
  §13 applies: **users interacting with it over the network must be offered the
  corresponding source.** Agent GM satisfies this by serving a `source_url`
  field in `GET /v1/health` and in the MCP `serverInfo`, pointing at
  `https://github.com/thisnick/agent-gm` at the exact built commit (§14.3).
  This is why the repository is public.
- The npm wrapper (`@agent-gm/cli`) downloads AGPL binaries; the npm package
  itself is AGPL-3.0 and its `package.json` says so.

This point is not repeated elsewhere in the spec.

---

## 2. Architecture

### 2.1 One binary

`cmd/agent-gm` builds a single static binary. It has no sidecars, no
message broker, no external database. Its entire runtime dependency set is: a
writable directory (SQLite database, session file, media cache) and outbound
HTTPS to Google.

```
                      ┌─────────────────────────────────────────┐
   claude.ai  ──MCP──►│  :8080                                  │
   ChatGPT    ──MCP──►│    /mcp        ◄── mcp ──┐              │
   agm CLI    ──REST─►│    /v1/*       ◄── api ──┤              │
   scripts    ──REST─►│    /oauth/*    ◄── oauth─┤              │
                      │    /.well-known/*        │              │
                      │                          ▼              │
                      │                     ┌────────┐          │
                      │                     │  core  │          │
                      │                     └───┬────┘          │
                      │                  ┌──────┴──────┐        │
                      │                  ▼             ▼        │
                      │              ┌───────┐    ┌────────┐    │
                      │              │ store │    │   gm   │    │
                      │              │sqlite │    │adapter │    │
                      │              └───────┘    └───┬────┘    │
                      └──────────────────────────────┼─────────┘
                                                     │ libgm
                                                     ▼
                                            Google Messages ◄──► phone
```

Everything above the `gm` package is deterministic and testable without a
network. The `fake` implementation of the `gm` interface (§2.2, §13.1) is what
makes that true.

### 2.2 Packages

All packages live under `internal/` except the command. Nothing in this
repository is intended for import by other Go programs; there is no stable Go
API.

| Package | Responsibility | Must not |
|---|---|---|
| `cmd/agent-gm` | flag parsing, config load, wiring, graceful shutdown | contain logic |
| `internal/gm` | the Google Messages adapter interface, its `libgm` implementation, and the fake. **One instance per account** | know about HTTP, SQLite, MCP or scopes |
| `internal/accounts` | the account registry and supervisor: which accounts exist, their state, one `gm.Backend` and one ingest goroutine each, start/stop on pair, sign-out and remove | speak JSON, or know a route from a tool |
| `internal/store` | SQLite: schema, migrations, all queries, the single writer | make network calls |
| `internal/core` | the operations the surfaces share: send, list, backfill, ingest, idempotency, delivery-status transitions | speak JSON or MCP |
| `internal/api` | REST handlers, DTOs, error envelope, strict parameter rejection, pagination | contain business logic |
| `internal/mcp` | MCP server, tool registry, schemas, instructions block, isError mapping | duplicate core logic |
| `internal/oauth` | OAuth 2.1 AS + protected-resource metadata, DCR, PKCE, enrolment, tokens | be reachable without TLS in production |
| `internal/media` | upload/download tickets, the media cache on disk, content sniffing, size limits | hold the store's write lock |
| `internal/cli` | `agm` subcommands, output formatting, exit codes, the Chrome cookie capture | talk to `gm` or `store` directly — it goes through REST |
| `internal/audit` | append-only audit log writer and its redaction rules | be optional |
| `internal/config` | environment and file config, defaults, validation | read secrets from anywhere but env/file |

### 2.3 The `gm` interface

`internal/gm` exists so that everything above it can be tested with no phone
and no network. It is a **narrow** interface: it exposes only what Agent GM
actually uses (§3.1), in Agent GM's own vocabulary, and it hides `gmproto`
types behind plain structs. Two implementations, and only ever two:

```go
package gm

// Backend is the whole of Agent GM's dependency on Google Messages.
type Backend interface {
    // lifecycle
    Connect(ctx context.Context) error
    Disconnect()
    IsConnected() bool
    IsLoggedIn() bool
    SessionID() string                 // libgm CurrentSessionID; drives the resync check

    // health -- everything GET /v1/health reports about Google
    FetchConfig(ctx context.Context) (ConfigInfo, error)  // live ConfigVersion, device email
    CompiledConfigVersion() ConfigVersion                 // util.ConfigMessage
    IsDefaultSMSApp(ctx context.Context) (bool, error)    // IsBugleDefault

    // pairing
    StartGooglePairing(ctx context.Context, cookies map[string]string, deviceIndex int, emoji func(string)) (PairedDevice, error)
    RefreshGoogleCookies(ctx context.Context, cookies map[string]string) error

    // reads
    ListConversations(ctx context.Context, folder Folder, count int) ([]Conversation, error)
    GetConversation(ctx context.Context, convID string) (*Conversation, error)
    GetConversationType(ctx context.Context, convID string) (ConversationType, error)
    ListMessages(ctx context.Context, convID string, count int, cursor *Cursor) ([]Message, *Cursor, error)
    ListContacts(ctx context.Context) ([]Contact, error)
    ListTopContacts(ctx context.Context) ([]Contact, error)
    ContactAvatars(ctx context.Context, ids []string) (map[string][]byte, error)
    DownloadAvatar(ctx context.Context, url string) ([]byte, error)

    // writes
    ResolveConversation(ctx context.Context, numbers []string, groupName string) (ResolveResult, error)
    SendText(ctx context.Context, req SendTextRequest) (SendResult, error)
    SendMedia(ctx context.Context, req SendMediaRequest) (SendResult, error)
    React(ctx context.Context, msgID string, emoji string, action ReactionAction) error
    DeleteMessage(ctx context.Context, msgID string) error
    MarkRead(ctx context.Context, convID, msgID string) error
    SetTyping(ctx context.Context, convID string) error
    UpdateConversation(ctx context.Context, convID string, ch ConversationChange) error
    DeleteConversation(ctx context.Context, convID, phone string) error

    // media
    Upload(ctx context.Context, data []byte, filename, mime string) (MediaRef, error)
    Download(ctx context.Context, mediaID string, key []byte) ([]byte, error)
    RequestFullSizeImage(ctx context.Context, msgID, actionMsgID string) error

    // events
    Events() <-chan Event
}
```

`Events()` returns a buffered channel that the core drains in one goroutine.
The `libgm` implementation adapts `libgm`'s synchronous `SetEventHandler`
callback onto that channel; because that callback **must not block** (`libgm`
calls it synchronously from the long-poll loop — see `client.go`
`SetEventHandler` doc comment), the adapter does a non-blocking send onto a
channel of capacity 1024 and increments a `dropped_events` counter on overflow,
which is surfaced in `GET /v1/health`.

`ResolveResult` carries the conversation **and** the raw
`GetOrCreateConversationResponse` status, so `core` can distinguish `SUCCESS`,
`CREATE_RCS` and an unnamed value (§3.7) without reaching past this interface.
`ConversationChange` carries the optional `folder`, `pinned` and `unread`
fields of `PATCH /v1/conversations/{id}`.

**Nothing above `internal/gm` imports `libgm` or `gmproto`.** Every field the
API promises is reachable through this interface, and the fake implements all
of it — that is what makes §13.2 runnable with no phone and no network.

The fake implementation (`internal/gm/fake`) is a deterministic in-memory
Google Messages: it accepts sends, generates message IDs, emits the same
event sequence a real phone would (§5.5), and can be scripted to fail with any
error in §3.5.

### 2.4 Concurrency model

- **One goroutine writes to SQLite, for the whole process.** All writes from
  every account go through `store.Writer`, a single goroutine consuming a
  channel of write closures. Accounts are concurrent at the network and at the
  ingest boundary, serialised at the database.
  SQLite is opened with `journal_mode=WAL`, `busy_timeout=5000`,
  `foreign_keys=on`, `synchronous=NORMAL`. Reads use a separate read-only pool.
- **One goroutine per account drains that account's `gm.Events()`** and applies
  each event as a store write, stamping `account_id` on every row. Accounts are
  independent: one account's phone being asleep, its cookies expiring or its
  backfill running does not block another's.
- **HTTP handlers are the only other concurrency**, and they never touch `gm`
  except through `core`, which serialises nothing but does hold per-conversation
  send locks so two agents cannot interleave sends into the same conversation.

---

## 3. The Google layer contract

Everything in this section is derived from mautrix-gmessages at the pinned
commit (§3.6). Citations are `path:symbol` in that tree.

### 3.1 Library surface Agent GM uses

Agent GM calls exactly these `libgm` symbols. Anything not listed here is out
of contract, and adding a call is a spec change.

#### Construction and lifecycle — `pkg/libgm/client.go`

| Symbol | Signature | Returns / meaning |
|---|---|---|
| `libgm.NewAuthData()` | `() *AuthData` | fresh `AuthData` with a new AES-CTR request-crypto helper and a new ECDSA refresh key. Used only when pairing from scratch. |
| `libgm.NewClient` | `(authData *AuthData, pk *PushKeys, logger zerolog.Logger) *Client` | the client. Agent GM always passes `pk == nil` (no web push; see §3.4). |
| `(*Client).SetEventHandler` | `(EventHandler)` where `EventHandler = func(evt any)` | registers the single event sink. **Called synchronously; must not block or make outgoing requests.** |
| `(*Client).FetchConfig` | `(ctx) error` | fetches Google's live `Config`, stores it on `c.Config`, and parses `DeviceInfo.DeviceID` into `AuthData.SessionID` (`client.go:389`). Logs (at trace) when the compiled-in `util.ConfigMessage` differs from live. Agent GM calls this **once per account, before that account's `Connect`**, and surfaces the compiled/live pair per account in `GET /v1/health` (§3.7). |
| `(*Client).Connect` | `() error` | refreshes the tachyon token synchronously so bad credentials surface immediately, then starts long polling. **The only connect path Agent GM uses**, once per account. |
| `(*Client).ConnectBackground` | `() error` | **not called**; a one-shot poll for push-woken processes. |
| `(*Client).Disconnect` | `()` | closes long polling and fails every in-flight response waiter with `ErrConnectionClosed`. |
| `(*Client).Reconnect` | `() error` | close + re-check login + restart polling. Agent GM calls this only from its own supervisor after `ListenFatalError` that is not a credential error. |
| `(*Client).IsConnected` | `() bool` | long-poll connection is non-nil. Upstream notes it is imprecise during reconnects; Agent GM treats it as advisory only. |
| `(*Client).IsLoggedIn` | `() bool` | `AuthData != nil && AuthData.Browser != nil && AuthData.HasCookies()`. This is the authoritative "are we paired" check. |
| `(*Client).CurrentSessionID` | `() string` | the session handler's current UUID; changes on every `SetActiveSession`. Recorded in that account's `account.state_changed` audit row on connect, with its `account_id` (§12.4). |
| `(*Client).SetProxy` | `(string) error` | **not called**; Agent GM has no proxy support. |
| `(*Client).SetPingInterval` | `(time.Duration)` | **not called**; clamped to `[1m, 4h)` upstream, and Agent GM leaves the default 1m. |
| `(*Client).SetDataReceiveCheckInterval` | `(time.Duration)` | **not called**; intervals under 5m are ignored upstream, and Agent GM leaves the default `DefaultBugleDefaultCheckInterval` = 2h55m. |

`libgm.AuthData` is the entire session. Its JSON tags are the on-disk format
Agent GM persists (§3.3). Fields: `RequestCrypto` (`*crypto.AESCTRHelper`),
`RefreshKey` (`*crypto.JWK`), `Browser` and `Mobile` (`*gmproto.Device`),
`TachyonAuthToken []byte`, `TachyonExpiry time.Time`, `TachyonTTL int64`,
`WebEncryptionKey []byte`, `SessionID`/`DestRegID`/`PairingID` (`uuid.UUID`),
`Cookies map[string]string`. `AuthData.IsGoogleAccount()` is `DestRegID != uuid.Nil`
(`client.go:92-94`). `AuthData.AuthNetwork()` (`client.go:96-101`) returns
`util.GoogleNetwork` ("GDitto") for the gaia flow and the **empty string**
otherwise; `util.QRNetwork` ("Bugle") appears only inside the two pairing HTTP
payloads (`pair.go:93,115`), never as a runtime network value.

#### Pairing — `pkg/libgm/pair.go`, `pkg/libgm/pair_google.go`

| Symbol | Signature | Returns / meaning |
|---|---|---|
| `(*Client).DoGaiaPairing` | `(ctx, emojiCallback func(string)) error` | Google-account flow, start to finish: `StartGaiaPairing`, hand the emoji to the callback, `FinishGaiaPairing`, emit `events.PairSuccessful`, reconnect in a goroutine. **This is the call Agent GM uses for the Google-account flow.** |
| `(*Client).StartGaiaPairing` | `(ctx) (string, *PairingSession, error)` | requires cookies (`ErrNoCookies` otherwise); signs in, enumerates the account's devices, **selects one by last-seen and `GaiaHackyDeviceSwitcher` — it does not error on several (§3.2)** — and returns the emoji to display. `pair_google.go:321-414`. |
| `(*Client).FinishGaiaPairing` | `(ctx, *PairingSession) (string, error)` | completes UKEY2, derives the request-crypto keys, sets `AuthData.PairingID`, returns `"<mobile sourceID>/<destRegDevice int>"` as the phone ID. |
| `(*Client).PairCallback` | — | **not called.** Listed only because leaving it nil is load-bearing: see the prose below |
| `(*Client).GaiaHackyDeviceSwitcher` | `int` field (`client.go:143`) | selects among several primary-looking devices as `primaryDevices[switcher % len]` after a newest-first sort (`pair_google.go:364`). Agent GM exposes it as `agm pair --device-index N` and as `device_index` on `POST /v1/pairing/start`. |

**Symbols in this table that Agent GM deliberately does *not* call** are marked
"not called" and are listed only because *not* calling them is part of the
contract: `PairCallback`, `SetProxy`, `ConnectBackground`, `SetPingInterval`
and `SetDataReceiveCheckInterval`. `PairCallback` in particular fires only from
`completePairing`, reached only via `handlePairingEvent` on a
`BugleRoute_PairEvent` — the withdrawn QR flow (D19). The gaia path emits
`PairSuccessful` directly from `DoGaiaPairing` (`pair_google.go:310`), so it is
unreachable here and Agent GM never sets it.

**`Unpair`, `UnpairGaia` and `UnpairBugle` are out of contract.** Agent GM has
no unpair operation: signing an account out (§4.7) shreds the local session and
leaves Google's device list alone, and removing an account deletes Agent GM's
copy of the data. Neither tells Google to revoke the pairing — that is done in
the Google Messages app on the phone, which is the only place the owner can see
the full device list anyway. `RevokePairData` arriving *from* the phone is
handled (§3.4); Agent GM never sends the corresponding request.

Upstream behaviours Agent GM depends on and must not re-implement:

- `DoGaiaPairing` reconnects in its own goroutine on success
  (`pair_google.go:311-318`). Agent GM does not reconnect on its own after
  `PairSuccessful`.

Google-account pairing errors, all from `pair_google.go`, all mapped in §3.5:
`ErrNoCookies`, `ErrNoDevicesFound`, `ErrIncorrectEmoji`, `ErrPairingCancelled`,
`ErrPairingTimeout`, `ErrPairingInitTimeout`, `ErrHadMultipleDevices`.
`GaiaInitTimeout` is 20s.

#### Reads — `pkg/libgm/methods.go`

| Symbol | Signature | Returns |
|---|---|---|
| `(*Client).ListConversations` | `(ctx, count int, folder gmproto.ListConversationsRequest_Folder) (*gmproto.ListConversationsResponse, error)` | `.Conversations []*conversations.Conversation`, `.Cursor *Cursor`. **The first call on each `Client` sends `MessageType_BUGLE_ANNOTATION`, every later call `BUGLE_MESSAGE`** — the library tracks it with `conversationsFetchedOnce`, which is **per `Client`, therefore per account** (`client.go:141`). Agent GM must call `ListConversations` at least once **per account's client** before relying on that account's live conversation events; one call on one account does nothing for another. |
| `(*Client).GetConversation` | `(ctx, conversationID string) (*gmproto.Conversation, error)` | one conversation, already unwrapped from the response. |
| `(*Client).GetConversationType` | `(ctx, conversationID string) (*gmproto.GetConversationTypeResponse, error)` | used only to disambiguate SMS vs RCS when a conversation record is incomplete. |
| `(*Client).FetchMessages` | `(ctx, conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error)` | `.Messages []*Message`, `.TotalMessages int64`, `.Cursor *Cursor`. This is the backfill primitive (§5.2). |
| `(*Client).ListContacts` | `(ctx) (*gmproto.ListContactsResponse, error)` | the phone's contact list. Request constants are fixed upstream (`i1=1, i2=350, i3=50`). |
| `(*Client).ListTopContacts` | `(ctx) (*gmproto.ListTopContactsResponse, error)` | 8 most-contacted. Agent GM uses it for `contacts.list?top=true`. |
| `(*Client).IsBugleDefault` | `(ctx) (*gmproto.IsBugleDefaultResponse, error)` | `.Success` — whether Google Messages is the phone's default SMS app. Surfaced in `GET /v1/health`; a `false` here explains most send failures. |
| `(*Client).GetParticipantThumbnail` / `(*Client).GetContactThumbnail` | `(ctx, ids ...string) (*gmproto.GetThumbnailResponse, error)` | avatars. Behind `Backend.ContactAvatars`; populates `contacts.avatar_hash`. |
| `(*Client).DownloadAvatar` | `(ctx, url string) ([]byte, error)` | fetches `Conversation.GroupAvatarURL` (`media.go:308-325`). Behind `Backend.DownloadAvatar`. |

`gmproto.ListConversationsRequest_Folder`: `UNKNOWN=0`, `INBOX=1`,
`ARCHIVE=2`, `SPAM_BLOCKED=5`. `gmproto.Cursor` is
`{lastItemID string, lastItemTimestamp int64}`.

#### Writes — `pkg/libgm/methods.go`

| Symbol | Signature | Returns |
|---|---|---|
| `(*Client).SendMessage` | `(ctx, *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)` | `.Status` is `UNKNOWN=0, SUCCESS=1, FAILURE_2=2, FAILURE_3=3, FAILURE_4=4`. Upstream renders `FAILURE_4` as the user-facing string `"Google Messages is not your default SMS app"` (`connector/errors.go:41-42`). `.GoogleAccountSwitch` is set when the phone's active Google account changed underneath us. |
| `(*Client).SendReaction` | `(ctx, *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error)` | `.Success bool` only. Action enum: `UNSPECIFIED=0, ADD=1, REMOVE=2, SWITCH=3`. |
| `(*Client).DeleteMessage` | `(ctx, messageID string) (*gmproto.DeleteMessageResponse, error)` | `.Success bool`. |
| `(*Client).MarkRead` | `(ctx, conversationID, messageID string) error` | no response body. |
| `(*Client).SetTyping` | `(ctx, convID string, simPayload *gmproto.SIMPayload) error` | fire-and-forget. Agent GM exposes this only via REST `POST /v1/conversations/{id}/typing`; there is no MCP tool for it (§8.2). |
| `(*Client).GetOrCreateConversation` | `(ctx, *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error)` | see §3.7 for the status enum and the undocumented values. |
| `(*Client).UpdateConversation` | `(ctx, *gmproto.UpdateConversationRequest) (*gmproto.UpdateConversationResponse, error)` | archive/unarchive/mark-unread/pin, via `ConversationActionStatus`. |
| `(*Client).DeleteConversation` | `(ctx, conversationID, phone string) error` | a thin wrapper over `UpdateConversation` with `ConversationActionStatus_DELETE` and a `DeleteConversationData{ConversationID, Phone}`. **This is Google's delete-for-me and the only delete Agent GM has** (N6). |

`gmproto.SendMessageRequest` is built as:

```go
&gmproto.SendMessageRequest{
    ConversationID: convID,
    MessagePayload: &gmproto.MessagePayload{
        TmpID:          txnID,   // a bare UUID, see below
        TmpID2:         txnID,   // must equal TmpID
        ConversationID: convID,
        ParticipantID:  outgoingID, // Conversation.DefaultOutgoingID
        MessageInfo: []*gmproto.MessageInfo{{
            Data: &gmproto.MessageInfo_MessageContent{
                MessageContent: &gmproto.MessageContent{Content: text},
            },
        }},
    },
    SIMPayload: simPayload, // Conversation.SIMCard.SIMData.SIMPayload, may be nil
    TmpID:      txnID,      // must equal MessagePayload.TmpID
    ForceRCS:   false,      // see below
    Reply:      replyPayload, // &gmproto.ReplyPayload{MessageID: ...} or nil
}
```

- `TmpID`, `MessagePayload.TmpID` and `MessagePayload.TmpID2` are **all three
  set to the same value**, mirroring
  `connector/handlematrix.go:ConvertMatrixMessage`.
- **That value is a bare UUID**, minted per send attempt with
  `uuid.NewString()`. Upstream generates it with `util.GenerateTmpID()`
  (`util/func.go:9-12`), whose comment is *"Matches what the native app
  does"*, and the phone echoes the value back on the remote message
  (`connector/handlegmessages.go:157,841`, `connector/backfill.go:66,158`).
  It is **not** Agent GM's `op_`-prefixed operation ID: sending a non-UUID
  transaction ID diverges from every other client of this protocol for no
  benefit. Agent GM stores the UUID in an indexed `operations.tmp_id` column
  and correlates the echo by `tmp_id → operation_id` (§6.3).
- `ParticipantID` must be the conversation's `DefaultOutgoingID`. Sending with
  the wrong participant ID is how messages end up attributed to the wrong SIM.
- `ForceRCS` is set only when the conversation is `ConversationType_RCS`, its
  send mode is `SEND_MODE_AUTO`, and the caller explicitly asked. Default false.
- Media: the same request with
  `MessageInfo[0].Data = &gmproto.MessageInfo_MediaContent{MediaContent: ref}`,
  where `ref` comes from `UploadMedia`. A caption is appended as a **second**
  `MessageInfo` entry carrying `MessageContent`, exactly as upstream does.

#### Media — `pkg/libgm/media.go`

| Symbol | Signature | Returns |
|---|---|---|
| `(*Client).UploadMedia` | `(data []byte, fileName, mime string) (*gmproto.MediaContent, error)` | the whole upload: generates a random 32-byte key, AES-GCM-encrypts the bytes, `StartUploadMedia` (resumable session against `util.UploadMediaURL`), `FinalizeUploadMedia`, and returns `MediaContent{Format, MediaID, MediaName, Size, DecryptionKey, MimeType}`. **`Size` is the plaintext size.** Note this method takes no `context.Context` at the pinned commit. |
| `(*Client).DownloadMedia` | `(mediaID string, key []byte) ([]byte, error)` | GETs `util.UploadMediaURL` with a base64 `DownloadAttachmentRequest` header, then AES-GCM-decrypts with `key`. Also no `context.Context`. |
| `(*Client).DownloadAvatar` | `(ctx, url string) ([]byte, error)` | plain HTTPS GET with relay headers, for `Conversation.GroupAvatarURL`. |
| `(*Client).GetFullSizeImage` | `(ctx, messageID, actionMessageID string) (*gmproto.GetFullSizeImageResponse, error)` | asks the phone to promote a thumbnail to full size. Agent GM calls it when a media part has a `ThumbnailMediaID` but an empty `MediaID`. |
| `libgm.MimeToMediaType` | `map[string]MediaType` | the mime → `gmproto.MediaFormats` table. Agent GM uses it to reject unsupported types **before** upload with `media_unsupported_type` (§7.6). Upstream falls back to the type prefix (`image/`, `video/`, …) when the exact mime is absent; Agent GM does the same. |

Because `UploadMedia` and `DownloadMedia` take no context and can block for the
duration of a large transfer, `internal/gm` wraps each in a goroutine plus a
`select` on `ctx.Done()`, and documents that cancelling only abandons the
result — the underlying HTTP request runs to completion.

#### Events — `pkg/libgm/event_handler.go`, `pkg/libgm/events/`

See §3.4 for the full event catalogue.
### 3.2 Pairing, and what the owner does on the phone

Agent GM implements **one** pairing flow: the Google-account (gaia) flow.
`agm pair` drives it end to end (§11.4), and `agm pair --paste` is the
fallback for a machine with no Chrome. **There is no second flow**, and D19
records why.

The cost of the only remaining flow is stated here and repeated in §11.4,
§12.1 and §15.2: it keeps live Google account cookies in that account's
session file for the
life of the pairing.

#### Google-account pairing (`GDitto` network, requires cookies)

**The required cookie set is exact** (`pkg/connector/login.go:200-210`):

| Cookie | Domain | Required |
|---|---|---|
| `SID` | `.google.com` | yes |
| `HSID` | `.google.com` | yes |
| `SSID` | `.google.com` | yes |
| `APISID` | `.google.com` | yes |
| `SAPISID` | `.google.com` | yes |
| **`OSID`** | **`messages.google.com`** | yes |
| `__Secure-1PSIDTS` | `.google.com` | no |

`SAPISID` alone is **not** sufficient. Its separate job is computing the
`SAPISIDHASH` `Authorization` header (`client.go:66-70`, `http.go:83-86`).
`OSID` is **host-scoped to `messages.google.com`**, so a capture that reads
only `.google.com` silently returns an unusable set — this is the most common
way the flow fails. **Five of the seven — `SID`, `HSID`, `SSID`, `OSID` and `__Secure-1PSIDTS` —
are `httpOnly`**, so `document.cookie` and any injected-JS approach cannot
reach them. (`APISID` and `SAPISID` are *not* `httpOnly`: Google's own web apps
read `SAPISID` from JavaScript to compute the `SAPISIDHASH` header.) Because
the majority are `httpOnly`, a JS-only capture returns an unusable set, which
is why §11.4 reads the browser's own cookie store over CDP.

Upstream's own capture URL is
`https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config`
(`login.go:229`).

1. Owner runs `agm pair` (§11.4), or `POST /v1/pairing/start` with the
   captured cookies (§7.5). **This adds an account, or resumes an existing
   one** (§4.7).
2. Agent GM calls `AuthData.SetCookies` and then
   `DoGaiaPairing(ctx, emojiCallback)` (`pair_google.go:300-320`).
3. `StartGaiaPairing` (`pair_google.go:321-414`) signs in and enumerates the
   account's devices. **Device selection is not "the single primary"**
   (`pair_google.go:352-364`):
   - zero devices with `UnknownInt4 == 1` → `ErrNoDevicesFound`;
   - **more than one → the library sorts by `LastSeen` newest-first, logs a
     warning, and picks `primaryDevices[c.GaiaHackyDeviceSwitcher % len]`.**
     It does **not** error.
   `ErrHadMultipleDevices` is returned **only** wrapped inside
   `ErrPairingInitTimeout`, and only when the CLIENT_INIT round trip times out
   with more than one candidate (`pair_google.go:385-397`).

   > **Operational consequence.** A Google account with two Android devices
   > pairs against whichever was seen most recently, silently. If that is the
   > wrong phone the only remedy is `Client.GaiaHackyDeviceSwitcher`, surfaced
   > as `agm pair --device-index N` (§11.3). Agent GM logs the chosen
   > `dest_reg_uuid` and `dest_reg_last_seen` at pair time and records them in
   > the audit row, so "which phone did we pair?" is answerable afterwards.
4. The emoji callback fires with **one emoji character**. The CLI prints it
   large and alone.
5. **On the phone:** Google Messages shows a choice of emoji plus a "this is
   not me" option. The owner taps the matching emoji.
6. `FinishGaiaPairing` (`pair_google.go:416-468`) completes UKEY2, derives the
   request-crypto keys (derivation version 0 or 1), sets `AuthData.PairingID`,
   and returns `"<mobile sourceID>/<destRegDevice int>"`.
7. Wrong emoji → `ErrIncorrectEmoji`; "this is not me" or dismissal →
   `ErrPairingCancelled`; expiry → `ErrPairingTimeout`.

#### Which account did we just pair?

**`AuthData.Mobile.SourceID` is the account identifier**: the Google account's
address, lowercased. Evidence at `be48a58`:

- `signInGaiaGetToken` sets `AuthData.Mobile` to a clone of the signed-in
  device with `SourceID` **lowercased**
  (`pair_google.go:102-105`, `strings.ToLower(device.SourceID)`).
- Upstream itself treats it as the account identity, and as **stable across a
  re-authentication**: the cookie-refresh path refuses to continue when
  `Config.GetDeviceInfo().GetEmail() != Session.Mobile.GetSourceID()`
  (`connector/login.go:267-270`), logging the two as `old_login` and
  `new_login`. A value compared against the account's email, across a
  re-auth, is the account's identity.
- Upstream also passes it as the login's `remoteName`
  (`connector/login.go:352`).

**No other `AuthData` field is a stable account identifier**, and the spec
says so rather than picking one hopefully:

| Field | Why not |
|---|---|
| `AuthData.DestRegID` | the *device* chosen at pair time (assigned at `pair_google.go:374`). Re-pairing to a second phone on the same account changes it |
| `AuthData.SessionID` | overwritten from `Config.DeviceInfo.DeviceID` on every `FetchConfig` (`client.go:389`) |
| `AuthData.PairingID` | a fresh `uuid.New()` per `PairingSession` (`pair_google.go:120-127`); changes on every pair |
| `AuthData.Browser.SourceID` | the *non*-lowercased device, and device-scoped |
| `FinishGaiaPairing`'s return | `"<sourceID>/<destRegDevice int>"` — deliberately phone-scoped; it is what upstream keys a *login* on, which is why re-pairing a different phone creates a second login upstream but must **not** create a second account here |

So: **one phone re-paired to the same Google account is the same account**,
because the identifier ignores the device half. Agent GM stores the raw value
as `accounts.google_account` and derives the public `acct_` ID from it (§4.1),
so the address itself is never an ID and never appears in a URL.

**An empty address is refused before anything is created.** `acct_` is
`UUIDv5(ns, "account", address)`, so an empty `Mobile.SourceID` would yield one
degenerate ID that two different accounts could both land on — and §4.1 makes
that ID the root of every `conv_`, `msg_` and `contact_`. If, after
`StartGaiaPairing`, `AuthData.Mobile.SourceID` is empty or not a syntactically
plausible address, Agent GM aborts the pairing with `pairing_no_account`
(409, §7.2), writes an `account.pair_failed` audit row, and **creates no
account row and no session file**. It never falls back to a generated ID.

**The pairing row has a bounded lifecycle**, because `AuthData.Mobile` is
populated inside `StartGaiaPairing` (`pair_google.go:325` → `:101-104`) —
*before* the owner confirms the emoji — so the account identity is known well
before the pairing is known to succeed:

| Moment | Row |
|---|---|
| `POST /v1/pairing/start` accepted, cookies captured | **no account row.** The pairing is tracked by `pairing_id` alone |
| address extracted and validated (above) | if an account with that `acct_` ID already exists, it is *resumed* (§4.7) and keeps its current state until the pair completes. Otherwise a row is created with `state='pairing'` |
| `FinishGaiaPairing` succeeds | `state='connected'`, `state_reason=null`, `paired_at_ms` set, session file written |
| emoji wrong or cancelled, `GaiaLoggedOut`, `AGENT_GM_PAIRING_TIMEOUT` elapsed, `DELETE /v1/pairing/{id}`, or the process restarts | **the half-built `AuthData` is discarded and a row that has never been `connected` is deleted.** A row that *had* been connected reverts to its previous state |

**Re-pairing an existing account does not move it to `pairing`.** An account
that is `signed_out` (or `error`, or `parked`) keeps that state for the whole
of the new pairing and flips straight to `connected` on success, so a caller
polling `GET /v1/accounts/{id}` never sees it become less usable than it
already was. The in-flight pairing is visible on `GET /v1/pairing/{id}`, which
carries the `account_id` once the address is known (§7.5), and the account DTO
carries **`pairing_id`** — non-null exactly while a pairing is in flight for
it. That is how the two surfaces are related; without it a caller sees a
`signed_out` account and a `waiting` pairing with nothing joining them. Only a
**new** account passes through `pairing`.

Two rules follow, and both are tested (§16):

- A startup sweep deletes every `pairing` row with no live pairing behind it,
  and any `pairing` row older than `AGENT_GM_PAIRING_TIMEOUT` is deleted by the
  same sweep on its ordinary interval. `pairing` is never a resting state.
- **`pairing` accounts do not count toward the §7.3 ambiguity rule.** That rule
  counts accounts in a *usable or recoverable* state — `connected`, `degraded`,
  `parked`, `error`, `signed_out`, `account_changed` — so an in-flight or
  abandoned pair can never start demanding `account_id` on writes for an
  account that does not exist yet and may never.

Two caveats, both recorded rather than defended:

- A Google account's primary address **can** be changed by its owner. That is
  rare, it is not observable from the pinned library, and the consequence is
  that Agent GM would see a new account. The runbook (§15.4) says to
  `agm accounts remove` the stale one after confirming.
- The value is personal data. It is stored, and it is redacted from logs like
  a phone number (§12.2): logs carry the `acct_` ID.

**Cookies are kept for the whole life of the session, not consumed at
pairing.** They live in `AuthData.Cookies`, are attached to *every* subsequent
request (`http.go:58`, `client.go:407`,
`session_handler.go:62,88,151,382`, `longpoll.go:519`), and are rotated in
place by `UpdateCookiesFromResponse` (`client.go:73-81`). `HasCookies()` is
part of `IsLoggedIn()` (`client.go:361`) and selects the unpair path
(`pair.go:164`).

> **Therefore each account's `sessions/<acct>.enc` holds live, full-privilege
> Google account
> cookies**, not a scoped token, for as long as the pairing lives. That is a
> the whole Google account rather than a scoped token. **This is inherent to
> the only pairing flow Google still offers** (D19), not a choice between
> options. It is stated again in §11.4, §12.1, §15.2 and `docs/pairing.md`,
> and it is what the owner consents to when the headless `--server` form sends
> cookies to a remote Agent GM.

**Cookie expiry does not require a re-pair.** Upstream re-authenticates an
existing pairing with fresh cookies (`pkg/connector/login.go:246-289`):
`StartWithOverride` accepts the re-auth only when
`Session.TachyonAuthToken != nil && Session.PairingID != uuid.Nil` (`:249`);
`SubmitCookies` then sets the new cookies, calls `FetchConfig`, verifies the
same account with
`Config.GetDeviceInfo().GetEmail() == Session.Mobile.GetSourceID()` (`:268`),
`Connect`s, and saves; on any failure it restores by `SetCookies(nil)`
(`:289`). Agent GM implements exactly that as
`agm pair --refresh-cookies --account <id>` and
`POST /v1/accounts/{account_id}/refresh-cookies` (§7.5). There is no
`/v1/pairing/refresh-cookies`: a refresh names an account that already exists,
so it belongs under `/v1/accounts`. A refresh against a **different** Google
account is refused with `pairing_wrong_account` and changes nothing.

> **A declared deviation.** Upstream does *not* refuse a wrong-account refresh:
> `SubmitCookies` logs `"Reauthenticated with wrong account"`, falls out of the
> chain to `SetCookies(nil)` (`login.go:267-271`, `:289`), and proceeds to a
> **fresh pairing** — which here would silently create a second account.
> Agent GM refuses instead. This is stricter than upstream on purpose, and it
> is the only place §3 does not implement the pinned behaviour as written.

When cookies die without a refresh, the long poll emits
`events.GaiaLoggedOut` — delivered as a `GET_UPDATES` data event whose
unencrypted payload is exactly `{0x72, 0x00}`
(`event_handler.go:226-232`, `hackyLoggedOutBytes`). Agent GM sets **that account's** state to `signed_out`. Its reads keep
working; its writes are refused with `unsupported_capability` and
`details.reason = "not_signed_in"` — **not** `not_paired`, which means the
server holds no accounts at all (§7.2, §7.8). Other accounts are unaffected.
The fix is a cookie refresh, not a re-pair (above).

#### Concurrent Google Messages Web use

**Agent GM does not evict Google Messages Web, and Messages Web does not evict
Agent GM.** Nothing at this pin describes a paired-device slot limit, and the
gaia device enumeration explicitly expects and tolerates several devices
(above). What concurrent use *does* cause is session flapping: the phone emits
`BROWSER_INACTIVE(1)`, `BROWSER_INACTIVE_FROM_TIMEOUT(7)` and
`BROWSER_INACTIVE_FROM_INACTIVITY(8)` when this session goes idle, and
`BROWSER_ACTIVE(2)` when it comes back (`connector/handlegmessages.go:298-340`).
Each `BROWSER_ACTIVE` whose session ID differs from the last one triggers a
resync. The cost is bandwidth and backfill churn, not the pairing.

#### Unpairing and session invalidation

Agent GM never calls `Unpair` (Section 3.1): signing out is a local act that
shreds the session file, and Google keeps the pairing until the phone or the
Google account removes it. If the *phone* ends the pairing, the long poll
produces `events.PingFailed` wrapping `events.ErrRequestedEntityNotFound`, or
a `*gmproto.RevokePairData` event (`pair.go:50-51`). Agent GM treats both as invalidation **of that account**: set its state to
`signed_out`, stop *its* poller and ingest goroutine, and require a fresh pair
for it. Its history is untouched (§4.7), and every other account keeps
running.

Agent GM never pairs implicitly. `agm pair` is always explicit, always
interactive or `--yes`-gated, and always writes an audit record.

### 3.3 Session persistence and refresh

A session is `libgm.AuthData` marshalled to JSON. **There is one per logged-in
account**, stored **outside** the SQLite database at
`$AGENT_GM_DATA_DIR/sessions/<acct_id>.enc`, encrypted with the single data key
(§4.5) using XChaCha20-Poly1305 with a random 24-byte nonce prefix. One data
key covers every account; there is no per-account key.

```
sessions/<acct_id>.enc :=
    "AGMS1" || nonce[24] ||
    AEAD(datakey, nonce, json(AuthData), aad="agent-gm/session/v1|" || acct_id)
```

The account ID is in the AEAD's associated data, so a session file renamed or
copied to another account's name fails to open rather than silently loading
the wrong account. The `sessions/` directory is mode `0700`.

Rules:

- Written **atomically** as `sessions/<acct_id>.enc.tmp` + `fsync` + `rename`,
  and only by the store writer goroutine, so a crash mid-write cannot corrupt
  it. The temp file is in the **same directory** as its target, which is what
  makes the rename atomic; the name is per account so two accounts persisting
  at once cannot collide even if the writer were ever made concurrent.
- Persisted per account on: that account's `events.PairSuccessful`,
  `events.AuthTokenRefreshed`, every successful `Connect`, and graceful
  shutdown. Also on a 5-minute timer if the
  in-memory `AuthData` differs from what was last written — **cookies mutate
  in place via `UpdateCookiesFromResponse` (`client.go:73-81`) with no event**,
  so the timer is the only thing that captures a rotation.
- File mode `0600`. Never logged, never included in a diagnostics bundle,
  never returned by any API. On the gaia flow it contains live Google account
  cookies (§3.2), which governs how backups of it are handled (§15.2).
- **Token refresh is the library's job.** `refreshAuthToken` (`client.go:491`)
  runs inside `Connect` and inside the long-poll loop, refreshing when
  `time.Until(TachyonExpiry) <= libgm.RefreshTachyonBuffer` (1 hour,
  `client.go:105`). On success it emits `events.AuthTokenRefreshed`. Agent GM
  never calls a refresh path itself; it only persists.
- A refresh error is **fatal** if it is `events.ErrInvalidCredentials`,
  `events.ErrRequestedEntityNotFound`, or an HTTP 401/403/404
  (`client.go:477-489`, `isFatalRefreshError`). Fatal → session invalidated.
  Anything else is transient and the long poll retries.

### 3.4 The event stream

`SetEventHandler` receives `any`. This is the complete catalogue Agent GM
handles; anything else is logged at debug and dropped, and an unknown type
increments that account's `unknown_events` counter in health.

> **Every row below is one account's event, on one account's `Client`, and
> every reaction is scoped to that account** — its `accounts` row, its
> `gm.Backend`, its ingest goroutine, its backfill and its sweep. There is one
> handler per account (§2.4, §4.7). "Mark `connected`", "persist `AuthData`"
> and "→ `degraded`" all mean *for this account*; no event from one account
> ever changes another's state, and no account's failure stops another's
> stream.

**Connection lifecycle** (`pkg/libgm/events/ready.go`)

| Event | Meaning | Agent GM's reaction |
|---|---|---|
| `*events.ClientReady{SessionID, Conversations}` | long poll established, with the initial conversation list | mark `connected`; upsert those conversations; kick backfill (§5.2) |
| `*events.AuthTokenRefreshed{}` | tachyon token rotated | persist `AuthData` |
| `*events.ListenTemporaryError{Error}` | poll dropped, will retry | session → `degraded`; health counter; no user-visible failure |
| `*events.ListenRecovered{}` | poll back | session → `connected` |
| `*events.ListenFatalError{Error}` | poll cannot continue | see the matching rule below |
| `*events.PingFailed{Error, ErrorCount}` | ditto ping failed | `ErrRequestedEntityNotFound` → set this account `signed_out` (the phone no longer knows this pairing); else if `ErrorCount > 1` → this account `error` (upstream deliberately ignores the first failure) |
| `*events.PhoneNotResponding{}` | see below | this account's `phone_responding=false` in `GET /v1/health`; its sends are still allowed but will likely fail. Other accounts' phones are unaffected |
| `*events.PhoneRespondingAgain{}` | recovered | this account's `phone_responding=true` |
| `*events.NoDataReceived{}` | nothing received for `dataReceiveCheckInterval` (default 2h55m, `client.go:115`) | health counter; triggers a reconciliation sweep (§5.4) |
| `*events.HackySetActiveMayFail{}` | skip count non-zero at connect | re-issue `SetActiveSession` after a delay |

**Matching `ListenFatalError` correctly.** The library makes an HTTP
**`401` or `403`** on the listen request fatal, and renders both as
`http %d while polling` (`longpoll.go:539-547`, `events/ready.go:84-87`).
Agent GM therefore matches with
`errors.As(err, &events.HTTPError{})` and `Resp.StatusCode ∈ {401, 403}`, or
`errors.Is(err, events.ErrInvalidCredentials)` — **never on the string**.
Matching only `"http 401 while polling"` sends a 403 into the retry branch,
where it loops forever on dead credentials. Either match → set **this account**
`signed_out`: its credentials are dead, its history stays readable, and the fix
is `agm pair --refresh-cookies --account <id>` (§4.7). Anything else → this
account `error`, and the supervisor retries `Reconnect` for it with backoff,
leaving every other account alone. There is no `bad_credentials` state and no
`unpaired` state; §4.7 defines the whole vocabulary.

**`PhoneNotResponding` has two trigger paths** (`longpoll.go:72-90,134-147,196-212`):
the **first** unanswered ping, or `alertTimeoutCount` (default **4**,
`client.go:184`) missed pings in a row. The runbook timing in §15.4 depends on
knowing this is not a single fixed threshold.

**Pairing** (`pkg/libgm/pair.go`, `pkg/libgm/pair_google.go`)

| Event | Meaning |
|---|---|
| `*events.PairSuccessful{PhoneID, QRData}` | pairing done. **`QRData` is always `nil` on this path** — `DoGaiaPairing` constructs `&events.PairSuccessful{PhoneID: phoneID}` and nothing else (`pair_google.go:310`); the field is populated only by the withdrawn QR flow's `completePairing`. The **name** is vestigial upstream and does not indicate a surviving QR path. Read `PhoneID`; never dereference `QRData` |
| `*gmproto.RevokePairData` | the phone revoked this pairing → set this account `signed_out`. **Deletes nothing** (§4.7) |
| `*events.GaiaLoggedOut{}` | Google cookies dead → set this account `signed_out` and offer a cookie refresh (§3.2). **Deletes nothing** |
| `*events.AccountChange{*gmproto.AccountChangeOrSomethingEvent, IsFake bool}` | the phone's active Google account changed. `IsFake=true` means it was synthesised at startup from `EncryptedData2` (`event_handler.go:105-118`), not a real change. A real change to a different Google account sets this account `account_changed` and blocks its writes; §15.4 says what to do |

`*events.BrowserActive` is **defined but never emitted** at this pin
(`NewBrowserActive` has zero callers outside `pkg/libgm/gmtest/main.go:131`).
Agent GM must not subscribe to it. Browser activity arrives as a
`UserAlertEvent`, below.

**Data** (`pkg/libgm/event_handler.go:handleUpdatesEvent`)

All data events arrive as `ActionType_GET_UPDATES` and are demultiplexed by
`gmproto.UpdateEvents`:

| Delivered type | Source | Agent GM's reaction |
|---|---|---|
| `*libgm.WrappedMessage{*gmproto.Message, IsOld bool, Data []byte}` | `UpdateEvents_MessageEvent` | the message ingest path (§5.3). `IsOld` means replayed from the server's backlog after reconnect |
| `*gmproto.Conversation` | `UpdateEvents_ConversationEvent` | conversation upsert. Old conversation events are dropped by the library (`event_handler.go:262-267`) |
| `*gmproto.UserAlertEvent` | `UpdateEvents_UserAlertEvent` | the alert table below. Old alerts are dropped by the library (`event_handler.go:249-251`) |
| `*gmproto.Settings` | `UpdateEvents_SettingsEvent` | SIM list, RCS enablement, default-SMS-app flag |
| `*gmproto.TypingData` | `UpdateEvents_TypingEvent` | ephemeral; surfaced in `GET /v1/conversations/{id}` as `peer_typing_until`, never persisted |

`UpdateEvents_BrowserPresenceCheckEvent` is handled inside the library (it
auto-acks) and never reaches Agent GM.

**`gmproto.AlertType` has 28 values (0–27).** The ones Agent GM acts on:

| Alert | Reaction |
|---|---|
| `BROWSER_ACTIVE(2)` | **This session became active.** Compare `Client.CurrentSessionID()` against the last one Agent GM saw; if it differs — or if the session was inactive, or no data arrived recently — **resync**: run the reconciliation sweep of §5.4 since `last_data_received`. This is the pattern at `connector/handlegmessages.go:301-327`. It means "our session changed, resync", **not** "another device took over" |
| `BROWSER_INACTIVE(1)`, `BROWSER_INACTIVE_FROM_TIMEOUT(7)`, `BROWSER_INACTIVE_FROM_INACTIVITY(8)` | this session went idle; health flag, and expect a `BROWSER_ACTIVE` resync next |
| `MOBILE_DATABASE_SYNC_STARTED(13)`, `MOBILE_DATABASE_SYNCING(11)` | this phone is resyncing its own database; **defer this account's backfill only**, results are unstable. Other accounts keep going |
| `MOBILE_DATABASE_SYNC_COMPLETE(12)` | resume backfill and run a minimal reconciliation sweep |
| `MOBILE_BATTERY_LOW(5)` / `MOBILE_BATTERY_RESTORED(6)`, `MOBILE_DATA_CONNECTION(3)` / `MOBILE_WIFI_CONNECTION(4)`, `RCS_CONNECTION(9)` | phone-health fields on **this account's** row in `GET /v1/health` |

The other 19 are recorded in the audit log and otherwise ignored.

**Library-level deduplication loses messages, it does not merely suppress
duplicates.** `deduplicateUpdate` (`event_handler.go:168-181`) matches on
(id, SHA-256 of decrypted payload) against the last **8** updates
(`client.go:138`, `recentUpdates [8]updateDedupItem`) — and on a hit the
handler loop **`return`s**, abandoning *every remaining part of the batch*
(`event_handler.go:263-266` for conversations, `:272-275` for messages).

> This is the single most important correctness consequence in this section.
> The live event stream is **not** a complete record. Agent GM's own dedup
> (§5.4) cannot recover a message it never saw, so §5.4 requires a periodic
> reconciliation sweep, and §3.4's `BROWSER_ACTIVE`, `NoDataReceived` and
> `MOBILE_DATABASE_SYNC_COMPLETE` rows all trigger one.

### 3.5 Error taxonomy from the library

| Library error | Where | Agent GM code (§7.2) |
|---|---|---|
| `libgm.ErrPhoneNotResponding` | `session_handler.go:20-32,231`; the phone did not answer within `responseHardTimeout` = **60s**. Upstream notes *the server already accepted the request, so the phone may still process it later* | `phone_not_responding` (504) — and the operation stays `pending`, not `failed` (§6.4) |
| `libgm.ErrConnectionClosed` | `session_handler.go:25`; request in flight when `Disconnect` ran | `disconnected` (503) |
| `events.ErrInvalidCredentials` | tachyon type 16 (`events/ready.go:47-52`) | sets the account `signed_out`; a write against it is `unsupported_capability` with `details.reason = "not_signed_in"` (409) |
| `events.ErrRequestedEntityNotFound` | tachyon type 5 (`events/ready.go:35-45`) | same — `signed_out` + `not_signed_in` (409) |
| `events.ErrCallerNoPermission` | tachyon type 7 (`events/ready.go:54-59`) | `google_permission_denied` (502) |
| `events.RequestError{Data, HTTP}` | any non-OK tachyon response | `google_error` (502), with the numeric type and message in `details` |
| `events.HTTPError{Action, Resp, Body}` | transport level | `google_http_error` (502) |
| `pair_google.ErrNoCookies` | gaia without cookies | `pairing_no_cookies` (409) |
| `pair_google.ErrNoDevicesFound` | zero primary devices | `pairing_no_devices` (409) |
| `pair_google.ErrIncorrectEmoji` | wrong emoji tapped | `pairing_wrong_emoji` (409) |
| `pair_google.ErrPairingCancelled` | dismissed, or "this is not me" | `pairing_cancelled` (409) |
| `pair_google.ErrPairingTimeout` | expiry | `pairing_timeout` (409) |
| `pair_google.ErrPairingInitTimeout` | CLIENT_INIT round trip exceeded `GaiaInitTimeout` (20s) | `pairing_init_timeout` (409, retryable). **`ErrHadMultipleDevices` is only ever wrapped inside this**, so it is reported as `details.multiple_devices: true`, never as its own code. **There is no `details.device_count`** (D31) |

There is **no `pairing_multiple_devices` code.** Google never reports
"multiple devices" as an error; the library picks one (§3.2).

**Credential death is per account, and never `not_paired`.** The two rows above
are the library origin of `unsupported_capability` / `not_signed_in` (§7.8):
the account still exists, its history stays readable and searchable, and only
*its* writes are refused. `not_paired` (§7.2) means something different and
rarer — the server holds **no accounts at all** — and **no library error maps
to it**. An implementer building the error mapper from this table must not
collapse the two; §16 Slice 2 test 31 asserts the difference.

`events.RequestError.Is` compares `Type` and `Message` only, not the error
class (`events/ready.go:69-77`), so `errors.Is` against the three sentinel
values is reliable and is what Agent GM uses.

### 3.6 The pin, and the policy for changing it

```
module:  go.mau.fi/mautrix-gmessages
commit:  be48a58
subject: libgm/config: bump version
ConfigVersion (util.ConfigMessage): Year=2026 Month=9 Day=2 V1=4 V2=6
                                    (util/config.go:7-13)
go directive: 1.26.0 (toolchain go1.27.0)
```

`go.mod` pins by pseudo-version resolving to `be48a58`. A checkout of the
upstream tree at that commit lives at `/home/nick/code/mautrix-gmessages` on
the owner's machine and is **not** committed here; CI re-clones it for the
fixture-validation job (§13.4).

**Every claim in §3 cites `file:line` in that tree.** A claim that cannot be
cited is not a library claim and belongs in §18.1 as a field observation.

**Pin-and-bump policy.**

- The pin is a **fact recorded in three places that must agree**: `go.mod`,
  the constant `gm.PinnedUpstreamCommit` in `internal/gm/pin.go`, and this
  section. CI job `pin-consistency` fails if they diverge.
- Agent GM never floats the dependency. `GOFLAGS=-mod=readonly` is set in
  `devbox.json` so an accidental `go get` cannot silently move it.
- **Bumping is a deliberate slice**, never a drive-by commit. The slice must:
  (a) update all three places; (b) diff `pkg/libgm` and `pkg/connector`
  between old and new commit and record, in `docs/upstream-pin.md`, every
  change to a symbol in §3.1, every change to `util.ConfigMessage`, and every
  added, removed or renamed enum value in §4.4 or §3.4; (c) re-run the
  fixture-validation job; (d) pass a **live gate** (§13.3) — pair, list, send
  one text to the approved direct number, receive a reply — run by the
  coordinator, not by an implementer.
- A bump that changes `util.ConfigMessage` is expected to be **urgent**: see
  the ConfigVersion note in §3.7 and D3.

### 3.7 Sharp edges

Every item here is cited. Where Agent GM behaves on something that is *not*
in the pinned tree, it says so and points at §18.1.

- **`ListConversations` must be called once per account before that account's
  live conversation events are trustworthy.** The first call on a `Client`
  sends `MessageType_BUGLE_ANNOTATION` and later calls `BUGLE_MESSAGE`
  (`methods.go:9-20`, `client.go:141` `conversationsFetchedOnce`), and the flag
  lives on the `Client`, of which there is one per account. Agent GM issues one
  on every account's connect. Satisfying the rule for one account leaves
  another account's conversation events untrustworthy.
- **`SendMessageResponse.Status`** is `UNKNOWN=0, SUCCESS=1, FAILURE_2=2,
  FAILURE_3=3, FAILURE_4=4` (`gmproto/client.pb.go`). `FAILURE_2` and
  `FAILURE_3` are transient and retried on `[3s, 8s, 20s]`
  (`connector/handlematrix.go:112-117`, `sendRetryBackoff`); Agent GM uses the
  same set and the same backoff. **`FAILURE_4` is not retried**: upstream
  renders it as the user-facing string
  `"Google Messages is not your default SMS app"`
  (`pkg/connector/errors.go:41-42`), and Agent GM maps it to
  `not_default_sms_app`.
- **`ErrPhoneNotResponding` does not mean the send failed.** The server
  accepted it; the phone may still deliver it when it wakes
  (`session_handler.go:20-24`). Agent GM keeps the operation `pending` (§6.4)
  and lets the remote echo resolve it.
- **`SendMessageResponse.GoogleAccountSwitch`** being non-empty means the
  phone switched Google accounts; Agent GM records it and marks the session
  `account_changed`.
- **`GetOrCreateConversationResponse.Status`** declares exactly three values
  at this pin (`gmproto/client.pb.go:181-183`):

  ```proto
  enum Status { UNKNOWN = 0; SUCCESS = 1; CREATE_RCS = 3; }
  ```

  `CREATE_RCS` means the phone wants the caller to retry with
  `CreateRCSGroup=true` and a non-nil `RCSGroupName` (empty string is
  acceptable); upstream retries exactly once, exactly that way
  (`connector/startchat.go:214-219`). Agent GM does the same; a second
  `CREATE_RCS` is `google_error`. Any other non-`SUCCESS` value, **including
  values the enum has no name for**, is `google_undocumented_status` (502)
  carrying `details.status` as the bare integer — upstream itself only says
  `no conversation data in response (status: %s)`
  (`connector/startchat.go:233,238`). Agent GM never invents a name for an
  unnamed value, and never claims to know what one means. See D3 and §18.1
  for the ConfigVersion field observation and the separate
  `config_version_stale` diagnosis.
- **`config_version_stale` is Agent GM's own diagnosis, not Google's answer.**
  It is raised when a conversation-creating call returns a non-`SUCCESS`
  status **and** the live `ConfigVersion` from `FetchConfig` differs from the
  compiled-in `util.ConfigMessage` in year, month or day. The ConfigVersion
  diff is the whole detection rule; no particular status code is required or
  claimed. **The compiled version is a property of the binary; the live one is
  a property of each account's `FetchConfig`**, so `GET /v1/health` carries the
  compiled value once at the top level and a `google` block per account — §7.5
  holds the one authoritative shape of that document. The diff is therefore
  visible per account without reproducing a failure, and an account that is not
  `connected` reports `google: null`.
- **Message status ranges.** `MessageStatusType` outgoing values run 1–27,
  incoming 100–118, **tombstones 200–279**, and `MESSAGE_DELETED = 300` sits
  outside every range (`gmproto/conversations.proto:295-427`). §4.4 maps every
  one of them; nothing falls to `unknown` by omission.
- **Tombstones are not messages.** Values 200–279 are in-thread system
  events. Agent GM stores them with `kind="system"` and excludes them from
  `messages.list` unless `include_system=true` (§7.5). The set upstream
  ignores outright is carried over verbatim from
  `connector/handlegmessages.go:913-940` (`shouldIgnoreStatus`).
- **Reactions are a closed 14-value enum**, not free-form emoji
  (`gmproto/conversations.proto:70-85`, `gmproto/emojitype.go`). Eleven have
  fixed code points; `CUSTOM=8` carries arbitrary unicode in
  `ReactionData.Unicode`; `EMOTIFY=13` has none.

  | `EmojiType` | `Unicode()` |
  |---|---|
  | `LIKE(1)` | 👍 |
  | `LOVE(2)` | 😍 |
  | `LAUGH(3)` | 😂 |
  | `SURPRISED(4)` | 😮 |
  | `SAD(5)` | 😥 |
  | `ANGRY(6)` | 😠 |
  | `DISLIKE(7)` | 👎 |
  | `QUESTIONING(9)` | 🤔 |
  | `CRYING_FACE(10)` | 😢 |
  | `POUTING_FACE(11)` | 😡 |
  | `RED_HEART(12)` | ❤️ |
  | `CUSTOM(8)` | `""` — the emoji is whatever the caller sent |
  | `EMOTIFY(13)` | `""` — upstream renders it as the literal `:custom:` (`connector/handlegmessages.go:542-543`) |
  | `REACTION_TYPE_UNSPECIFIED(0)` | `""` |

  Three consequences Agent GM must handle:

  1. **Normalisation is mandatory.** `UnicodeToEmojiType` accepts both `"❤"`
     and `"❤️"` as `RED_HEART` (`emojitype.go:53-54`), but `Unicode()` always
     renders `"❤️"` (`emojitype.go:25-26`). Agent GM canonicalises **every**
     inbound emoji through `UnicodeToEmojiType` → `Unicode()` **before**
     deriving a `react_` ID or matching a URL path segment. Without it, adding
     `❤` and removing `❤️` are different reactions and the same reaction gets
     two IDs.
  2. **A type with no unicode still exists.** `EMOTIFY` and any unrecognised
     type with an empty `Unicode()` are *silently skipped* upstream
     (`handlegmessages.go:546-548`). Agent GM instead serves
     `{"emoji": null, "type": "emotify"}` — see the reaction shape in §7.5.
  3. **Anything outside the eleven becomes `CUSTOM`** and may not render on the
     recipient's phone. Every surface that takes an emoji says so.
- **`Conversation.LatestMessage` is large and duplicative.** Upstream clones
  and nils it before logging (`connector/startchat.go:227-230`). Agent GM never
  persists it on the conversation row; it goes through the message ingest path
  or nowhere.
- **Timestamps are microseconds.** `gmproto.Message.Timestamp` and
  `Conversation.LastMessageTimestamp` are Unix microseconds
  (`connector/backfill.go:126`, `chatsync.go:51`), as is `TachyonTTL`
  (`client.go:440-445`). Agent GM converts once, at the `gm` boundary, and
  stores milliseconds (§4.3).
- **`libgm` writes to the process's zerolog logger** and at trace level logs
  the base64 of decrypted payloads (`event_handler.go:logContent`). Agent GM
  configures the library logger at `info` in production and forbids `trace`
  unless `AGENT_GM_UNSAFE_TRACE=1` is set, which also stamps every log line
  with `unsafe_trace=true` (§12.2).

## 4. Data model and identifiers
### 4.1 Identifier scheme

Public IDs are opaque, URL-safe, stable strings with a typed prefix. A caller
never sees a raw Google conversation ID, message ID or participant ID on a
public surface; those live in internal `source_*` columns and are exposed only
under `admin` on `GET /v1/admin/diagnostics`.

| Prefix | Object | Shape | Derivation |
|---|---|---|---|
| `acct_` | **Google account** | UUIDv5 | ns, `"account"`, the lowercased `AuthData.Mobile.SourceID` (§3.2) |
| `conv_` | conversation | UUIDv5 | ns, `account_id`, `"conversation"`, Google conversation ID |
| `msg_` | message | UUIDv5 | ns, `account_id`, `"message"`, Google conversation ID, Google message ID |
| `att_` | attachment (one media part) | UUIDv5 | message ID, part index, Google media ID |
| `react_` | reaction | UUIDv5 | message ID, participant ID, **canonical `EmojiType.Unicode()`** (§3.7); for `CUSTOM` the caller's unicode after NFC normalisation; for a type with no unicode, the `EmojiType` name |
| `contact_` | contact | UUIDv5 | ns, `account_id`, `"contact"`, Google participant ID |
| `part_` | participant of a conversation | UUIDv5 | conversation ID, Google participant ID |
| `op_` | operation | UUIDv7 | locally created |
| `upl_` | upload reservation | UUIDv7 | |
| `authreq_` | pending OAuth authorization request | UUIDv7 | |
| `auth_` | active authorization (grant) | UUIDv7 | |
| `client_` | registered OAuth client | UUIDv7 | |
| `enroll_` | enrollment code record | UUIDv7 | |
| `req_` | request ID, in every envelope | UUIDv7 | not stored |

Rules:

- **One frozen namespace UUID**, declared once in `internal/store/ids.go` as
  `IDNamespace`, never changed.
- **`account_id` is in every root derivation** — `conv_`, `msg_` and
  `contact_`. `att_`, `react_` and `part_` inherit it transitively through
  their parent. Two consequences:
  - **Conversation, message and contact IDs are globally unique** across
    accounts, not unique-per-account. A `conv_` ID identifies a thread without
    also needing an account, so no route takes both; the account is recoverable
    from the row.
  - **Re-pairing the same Google account reuses the same `acct_` ID**, so every
    conversation and message ID survives a logout, a re-pair, or a re-pair to a
    *different phone* on that account (§3.2). A different account produces a
    disjoint ID space, which is correct.
- `acct_` is derived from the account address rather than assigned, so a
  database rebuilt from Google reproduces the same account IDs.
- UUIDv5 for everything derived from Google, so a wiped and re-backfilled
  database hands agents the same IDs. UUIDv7 for everything Agent GM creates.
- An ID with the wrong prefix for the parameter is `invalid_request` naming the
  parameter and the expected prefix — **never `not_found`**. A raw Google ID is
  the same.
- **`react_` IDs are addressable.** `DELETE /v1/reactions/{reaction_id}` and
  the `reaction_id` argument of `remove_reaction` accept them, so a caller that
  read a reaction can remove it without re-deriving the emoji (§7.6).

### 4.2 SQLite schema

One database file, `$AGENT_GM_DATA_DIR/agent-gm.sqlite3`, opened with
`journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`,
`synchronous=NORMAL`. One writer goroutine; a read-only pool for queries.

```sql
-- identity and process state -------------------------------------------------
CREATE TABLE server_meta (
    key            TEXT PRIMARY KEY,
    value          TEXT NOT NULL
);
-- keys: pending_reprocess, upstream_commit, config_version_compiled
-- Nothing account-shaped lives here. Every per-account fact -- pairing time,
-- last event, last sweep, backfill completion -- is a column on `accounts`,
-- because two accounts would otherwise race on one row (5.2).

-- accounts -------------------------------------------------------------------
CREATE TABLE accounts (
    id                       TEXT PRIMARY KEY,        -- acct_...
    google_account           TEXT NOT NULL UNIQUE,    -- AuthData.Mobile.SourceID, lowercased (3.2)
    label                    TEXT,                    -- owner-set, for humans; never an ID
    state                    TEXT NOT NULL,           -- see 4.7
    phone_id                 TEXT,                    -- FinishGaiaPairing's "<sourceID>/<int>"
    gaia_dest_reg_uuid       TEXT,
    gaia_device_last_seen_ms INTEGER,
    session_present          INTEGER NOT NULL DEFAULT 0, -- is sessions/<id>.enc on disk
    paired_at_ms             INTEGER,
    last_event_at_ms         INTEGER,
    last_sweep_at_ms         INTEGER,
    backfill_complete_at_ms  INTEGER,
    created_at_ms            INTEGER NOT NULL,
    updated_at_ms            INTEGER NOT NULL
);
CREATE INDEX accounts_state ON accounts(state);

-- conversations --------------------------------------------------------------
CREATE TABLE conversations (
    id                     TEXT PRIMARY KEY,          -- conv_...
    account_id             TEXT NOT NULL REFERENCES accounts(id),
    source_id              TEXT NOT NULL,             -- Google conversationID
    name                   TEXT,
    is_group               INTEGER NOT NULL DEFAULT 0,
    conversation_type      TEXT NOT NULL,             -- unknown|sms_mms|rcs
    send_mode_raw          TEXT NOT NULL,             -- internal; never served
    folder                 TEXT NOT NULL DEFAULT 'active', -- active|archived|spam_blocked
    unread                 INTEGER NOT NULL DEFAULT 0,
    pinned                 INTEGER NOT NULL DEFAULT 0,
    read_only              INTEGER NOT NULL DEFAULT 0,
    force_rcs_eligible     INTEGER NOT NULL DEFAULT 0, -- derived, see 4.6
    default_outgoing_id    TEXT,
    latest_message_id      TEXT,
    last_activity_ms       INTEGER NOT NULL,
    group_avatar_url       TEXT,
    sim_payload_json       TEXT,                      -- opaque, re-sent verbatim
    deleted_at_ms          INTEGER,
    created_at_ms          INTEGER NOT NULL,
    updated_at_ms          INTEGER NOT NULL,
    -- Google conversation IDs are only unique within an account.
    UNIQUE (account_id, source_id)
);
CREATE INDEX conversations_activity ON conversations(account_id, last_activity_ms DESC, id DESC);
CREATE INDEX conversations_all      ON conversations(last_activity_ms DESC, id DESC);
CREATE INDEX conversations_folder   ON conversations(account_id, folder, last_activity_ms DESC, id DESC);
CREATE INDEX conversations_name     ON conversations(name COLLATE NOCASE);
CREATE INDEX conversations_filters  ON conversations(account_id, conversation_type, is_group, unread, deleted_at_ms);
-- The all-accounts counterpart. Omitting account_id is the DEFAULT for reads
-- (7.3), so every filtered list needs an index that does not lead with it.
CREATE INDEX conversations_filters_all ON conversations(conversation_type, is_group, unread, deleted_at_ms, last_activity_ms DESC, id DESC);

-- participants ---------------------------------------------------------------
CREATE TABLE participants (
    id                TEXT PRIMARY KEY,               -- part_...
    account_id        TEXT NOT NULL REFERENCES accounts(id),
    conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    source_id         TEXT NOT NULL,                  -- Google participantID
    contact_id        TEXT REFERENCES contacts(id),
    display_name      TEXT,
    first_name        TEXT,
    phone_e164        TEXT,
    formatted_number  TEXT,
    identifier_type   TEXT,
    is_me             INTEGER NOT NULL DEFAULT 0,
    is_visible        INTEGER NOT NULL DEFAULT 1,
    UNIQUE (conversation_id, source_id)
);
CREATE INDEX participants_phone     ON participants(account_id, phone_e164);
CREATE INDEX participants_phone_all ON participants(phone_e164);
CREATE INDEX participants_name      ON participants(display_name COLLATE NOCASE);
CREATE INDEX participants_me        ON participants(account_id, is_me) WHERE is_me = 1;
CREATE INDEX participants_conv  ON participants(conversation_id);

CREATE TABLE contacts (
    id            TEXT PRIMARY KEY,                   -- contact_...
    account_id    TEXT NOT NULL REFERENCES accounts(id),
    source_id     TEXT NOT NULL,
    display_name  TEXT,
    phone_e164    TEXT,
    avatar_hash   TEXT,
    is_top        INTEGER NOT NULL DEFAULT 0,
    updated_at_ms INTEGER NOT NULL,
    UNIQUE (account_id, source_id)
);
CREATE INDEX contacts_phone ON contacts(account_id, phone_e164);
CREATE INDEX contacts_name  ON contacts(display_name COLLATE NOCASE);
CREATE INDEX contacts_all   ON contacts(updated_at_ms DESC, id DESC);
CREATE INDEX contacts_top   ON contacts(account_id, is_top) WHERE is_top = 1;
CREATE INDEX contacts_top_all ON contacts(is_top) WHERE is_top = 1;

-- messages -------------------------------------------------------------------
CREATE TABLE messages (
    id                  TEXT PRIMARY KEY,             -- msg_...
    account_id          TEXT NOT NULL REFERENCES accounts(id),
    conversation_id     TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    source_id           TEXT NOT NULL,                -- Google messageID, stable per account
    kind                TEXT NOT NULL,                -- message|system
    direction           TEXT NOT NULL,                -- incoming|outgoing
    sender_participant  TEXT,
    text                TEXT,
    subject             TEXT,
    delivery_state      TEXT NOT NULL,                -- see 4.4
    delivery_state_raw  INTEGER NOT NULL,             -- numeric MessageStatusType; admin-only
    delivery_error      TEXT,
    reply_to_message_id TEXT,
    operation_id        TEXT,
    tmp_id              TEXT,                         -- the bare UUID we sent (3.1)
    is_deleted          INTEGER NOT NULL DEFAULT 0,
    sent_at_ms          INTEGER NOT NULL,
    ingested_at_ms      INTEGER NOT NULL,
    updated_at_ms       INTEGER NOT NULL,             -- serves delivery.updated_at
    content_hash        TEXT NOT NULL,
    UNIQUE (conversation_id, source_id)
);
CREATE INDEX messages_conv_time   ON messages(conversation_id, sent_at_ms DESC, id DESC);
CREATE INDEX messages_acct_time   ON messages(account_id, sent_at_ms DESC, id DESC);
CREATE INDEX messages_time        ON messages(sent_at_ms DESC, id DESC);
CREATE INDEX messages_kind_state  ON messages(account_id, kind, delivery_state, sent_at_ms DESC, id DESC);
CREATE INDEX messages_kind_state_all ON messages(kind, delivery_state, sent_at_ms DESC, id DESC);
CREATE INDEX messages_sender      ON messages(sender_participant, sent_at_ms DESC, id DESC);
CREATE INDEX messages_tmp_id      ON messages(account_id, tmp_id) WHERE tmp_id IS NOT NULL;
-- (account_id, source_id) is the re-pair reconciliation key (4.7); it is
-- implied by UNIQUE(conversation_id, source_id) plus conversations.account_id,
-- and indexed explicitly because the sweep joins on it.
CREATE INDEX messages_acct_source ON messages(account_id, source_id);

CREATE VIRTUAL TABLE messages_fts USING fts5(
    text, subject, content='messages', content_rowid='rowid', tokenize='unicode61'
);
-- Both search modes use this index. `words` tokenises the query and ANDs the
-- terms; `exact` runs the same query and then filters the page in SQL by
-- substring, so neither mode is an unindexed table scan (7.5).

-- attachments ----------------------------------------------------------------
CREATE TABLE attachments (
    id                 TEXT PRIMARY KEY,              -- att_...
    account_id         TEXT NOT NULL REFERENCES accounts(id),
    message_id         TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    part_index         INTEGER NOT NULL,
    media_id           TEXT,
    thumbnail_media_id TEXT,
    decryption_key     BLOB,                          -- encrypted at rest, 4.5
    filename           TEXT,
    mime_type          TEXT,
    media_format       TEXT,
    size_bytes         INTEGER,
    width              INTEGER,
    height             INTEGER,
    download_state     TEXT NOT NULL,                 -- available|pending|failed|unavailable
    sha256             TEXT,
    UNIQUE (message_id, part_index)
);
CREATE INDEX attachments_message ON attachments(message_id);

-- reactions ------------------------------------------------------------------
CREATE TABLE reactions (
    id              TEXT PRIMARY KEY,                 -- react_...
    message_id      TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id  TEXT NOT NULL,
    emoji           TEXT,                             -- canonical Unicode(), NULL when the type has none
    emoji_type      TEXT NOT NULL,                    -- like|love|...|custom|emotify
    is_mine         INTEGER NOT NULL DEFAULT 0,
    updated_at_ms   INTEGER NOT NULL,
    -- One reaction per person per message: Google's picker is single-select
    -- and an add over an existing one is SWITCH, not a second entry (7.6).
    UNIQUE (message_id, participant_id)
);
CREATE INDEX reactions_message ON reactions(message_id);

-- operations (idempotency + status, NOT an outbox) ---------------------------
CREATE TABLE operations (
    id                    TEXT PRIMARY KEY,           -- op_...
    account_id            TEXT NOT NULL REFERENCES accounts(id),
    kind                  TEXT NOT NULL,
    authorization_id      TEXT NOT NULL,
    idempotency_key       TEXT NOT NULL,
    request_fingerprint   TEXT NOT NULL,
    conversation_id       TEXT,
    message_id            TEXT,
    tmp_id                TEXT,                       -- bare UUID sent as TmpID
    status                TEXT NOT NULL,              -- see 6.4
    terminal              INTEGER NOT NULL DEFAULT 0,
    terminal_at_ms        INTEGER,
    corrected_at_ms       INTEGER,
    error_code            TEXT,
    error_message         TEXT,
    error_retryable       INTEGER,
    google_status_raw     INTEGER,                    -- admin-only
    request_payload_json  TEXT NOT NULL,              -- redacted: no bodies
    created_at_ms         INTEGER NOT NULL,
    updated_at_ms         INTEGER NOT NULL,
    -- The idempotency key is scoped to the account as well as the caller and
    -- kind: the same key sending to two accounts is two operations.
    UNIQUE (authorization_id, account_id, kind, idempotency_key)
);
CREATE INDEX operations_pending ON operations(status) WHERE terminal = 0;
CREATE INDEX operations_caller  ON operations(authorization_id, created_at_ms DESC);
CREATE INDEX operations_tmp_id  ON operations(account_id, tmp_id) WHERE tmp_id IS NOT NULL;

-- backfill progress ----------------------------------------------------------
CREATE TABLE backfill_state (
    conversation_id  TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    account_id       TEXT NOT NULL REFERENCES accounts(id),
    cursor_item_id   TEXT,                            -- gmproto.Cursor.lastItemID
    cursor_ts_us     INTEGER,                         -- gmproto.Cursor.lastItemTimestamp
    oldest_seen_ms   INTEGER,
    messages_done    INTEGER NOT NULL DEFAULT 0,
    complete         INTEGER NOT NULL DEFAULT 0,
    updated_at_ms    INTEGER NOT NULL
);

-- uploads --------------------------------------------------------------------
-- Uploads carry NO account_id, deliberately: see 10.2.
CREATE TABLE uploads (
    id                  TEXT PRIMARY KEY,             -- upl_...
    authorization_id    TEXT NOT NULL,
    idempotency_key     TEXT,
    filename            TEXT NOT NULL,
    mime_type           TEXT NOT NULL,
    size_bytes          INTEGER NOT NULL,
    sha256_declared     TEXT,
    state               TEXT NOT NULL,                -- reserved|complete|consumed|expired
    token_hash          TEXT NOT NULL,
    redemptions         INTEGER NOT NULL DEFAULT 0,
    staged_path         TEXT,
    expires_at_ms       INTEGER NOT NULL,
    created_at_ms       INTEGER NOT NULL
);

-- download tickets: stateful, because the 5-redemption cap needs a counter ----
CREATE TABLE download_tickets (
    token_hash        TEXT PRIMARY KEY,
    attachment_id     TEXT NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
    authorization_id  TEXT NOT NULL,
    redemptions       INTEGER NOT NULL DEFAULT 0,
    max_redemptions   INTEGER NOT NULL DEFAULT 5,
    expires_at_ms     INTEGER NOT NULL,
    created_at_ms     INTEGER NOT NULL
);

-- media cache: the single authority for cached bytes on disk ------------------
CREATE TABLE media_cache_entries (
    attachment_id   TEXT PRIMARY KEY REFERENCES attachments(id) ON DELETE CASCADE,
    relative_path   TEXT NOT NULL,
    size_bytes      INTEGER NOT NULL,
    pinned          INTEGER NOT NULL DEFAULT 0,
    last_used_ms    INTEGER NOT NULL
);
CREATE INDEX media_cache_lru ON media_cache_entries(last_used_ms) WHERE pinned = 0;

-- auth -----------------------------------------------------------------------
CREATE TABLE oauth_clients (...);            -- client_...
CREATE TABLE enrollment_codes (...);         -- enroll_... , code_hash only
CREATE TABLE authorization_requests (...);   -- authreq_...
CREATE TABLE authorizations (...);           -- auth_...
CREATE TABLE tokens (...);                   -- hashes only, never values
CREATE TABLE oauth_attempts (...);           -- durable failure limiter

-- settings and audit ---------------------------------------------------------
CREATE TABLE settings (
    key           TEXT PRIMARY KEY,
    value_json    TEXT NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

CREATE TABLE audit_events (
    id                TEXT PRIMARY KEY,               -- UUIDv7
    kind              TEXT NOT NULL,
    account_id        TEXT,                           -- NULL for server-wide events
    authorization_id  TEXT,
    target_type       TEXT,
    target_id         TEXT,
    result            TEXT NOT NULL,                  -- ok|refused|failed
    source            TEXT,
    payload_json      TEXT NOT NULL,                  -- redacted, 12.4
    created_at_ms     INTEGER NOT NULL
);
CREATE INDEX audit_kind_time ON audit_events(kind, created_at_ms DESC);
CREATE INDEX audit_time      ON audit_events(created_at_ms DESC);
CREATE INDEX audit_auth      ON audit_events(authorization_id, created_at_ms DESC);
CREATE INDEX audit_account   ON audit_events(account_id, created_at_ms DESC);
```

**Every filtered listing is indexed twice**, once leading with `account_id` and
once without it. That is deliberate rather than redundant: §7.3 makes omitting
`account_id` the *default* for reads, so the all-accounts form is the hot path,
not the exceptional one. The `_all` indexes cost write amplification on a
workload that is overwhelmingly read-heavy, and their absence would make the
documented default a full scan. §16 Slice 2 asserts, with `EXPLAIN QUERY PLAN`,
that neither form of any advertised listing scans.

**`media_cache_entries` is the single authority for cached bytes.**
`attachments` carries no `cache_path` and no cached size: the eviction sweep,
the LRU order, the byte budget and the purge path all read `media_cache_entries`
and nothing else, so there is exactly one row per cached file and "no orphan
row" in §16 is a well-defined assertion. `pinned` exists so an in-flight
download or an open ticket is not evicted underneath itself.

### 4.3 Migrations

- **Forward-only, numbered, never edited after they ship.** `PRAGMA
  user_version` is the version. A database at a *higher* version than the
  binary knows refuses to open, naming both numbers. There is no
  down-migration.
- Each runs **inside one transaction**, on the writer goroutine, before any
  listener binds.
- A migration needing derived data recomputed sets
  `server_meta.pending_reprocess = <task name>`; the process runs that task
  once after startup and clears the key.
- `PRAGMA foreign_key_check` runs after every migration in tests and must be
  empty (§13.2).
- **Audit rows are never rewritten by a migration.** An audit row records what
  happened when it happened.

### 4.4 Timestamps and the delivery-state vocabulary

Google timestamps are **microseconds**; Agent GM converts at the `gm` boundary
and stores **milliseconds** in every `*_ms` column. Every JSON surface renders
them as RFC 3339 UTC with millisecond precision — including
`expires_at` on media tickets.

`messages.delivery_state` is Agent GM's own closed vocabulary. The mapping
covers **every** value declared in `MessageStatusType` at the pin; a value the
mapping does not name fails the table test in §13.2 rather than falling
silently to `unknown`.

| `delivery_state` | Meaning | `MessageStatusType` sources |
|---|---|---|
| `sending` | accepted locally or in flight; Google's UI says *Sending…* | `OUTGOING_DRAFT(3)`, `OUTGOING_YET_TO_SEND(4)`, `OUTGOING_SENDING(5)`, `OUTGOING_RESENDING(6)`, `OUTGOING_AWAITING_RETRY(7)`, `OUTGOING_SEND_AFTER_PROCESSING(10)`, `OUTGOING_SCHEDULED(16)`, `OUTGOING_VALIDATING(20)` |
| `sent` | the carrier took it | `OUTGOING_COMPLETE(1)`, `OUTGOING_NOT_DELIVERED_YET(14)` |
| `delivered` | the recipient's device has it | `OUTGOING_DELIVERED(2)` |
| `read` | the recipient opened it | `OUTGOING_DISPLAYED(11)` |
| `failed` | terminal failure | `OUTGOING_FAILED_GENERIC(8)`, `OUTGOING_FAILED_EMERGENCY_NUMBER(9)`, `OUTGOING_FAILED_TOO_LARGE(13)`, `OUTGOING_FAILED_RECIPIENT_LOST_RCS(17)`, `OUTGOING_FAILED_NO_RETRY_NO_FALLBACK(18)`, `OUTGOING_FAILED_RECIPIENT_DID_NOT_DECRYPT(19)`, `OUTGOING_FAILED_RECIPIENT_LOST_ENCRYPTION(21)`, `OUTGOING_FAILED_RECIPIENT_DID_NOT_DECRYPT_NO_MORE_RETRY(22)`, `OUTGOING_FAILED_RECIPIENT_NEGATIVE_DELIVERY(24)`, `MESSAGE_STATUS_OUTGOING_FAILED_EMERGENCY_PROTOCOL_DETERMINATION_MESSAGE(25)`, `OUTGOING_RESTRICTED(26)`, `OUTGOING_FAILED_TO_ENCRYPT(27)` |
| `canceled` | withdrawn before sending | `OUTGOING_CANCELED(12)`, `OUTGOING_REVOCATION_PENDING(15)` |
| `deleted` | removed | `OUTGOING_DELETED(23)`, `INCOMING_DELETED(117)`, **`MESSAGE_DELETED(300)`** |
| `received` | an incoming message that is complete, or whose content Agent GM will never get | `INCOMING_COMPLETE(100)`, `INCOMING_DELIVERED(108)`, `INCOMING_DISPLAYED(109)`, **`INCOMING_UNKNOWN_CONTENT_TYPE(116)`** |
| `downloading` | incoming media not yet fetched | `INCOMING_YET_TO_MANUAL_DOWNLOAD(101)`, `INCOMING_RETRYING_MANUAL_DOWNLOAD(102)`, `INCOMING_MANUAL_DOWNLOADING(103)`, `INCOMING_RETRYING_AUTO_DOWNLOAD(104)`, `INCOMING_AUTO_DOWNLOADING(105)`, `INCOMING_AWAITING_AUTO_DOWNLOAD(115)` |
| `download_failed` | incoming media unavailable | `INCOMING_DOWNLOAD_FAILED(106)`, `INCOMING_EXPIRED_OR_NOT_AVAILABLE(107)`, **`INCOMING_DOWNLOAD_CANCELED(110)`**, `INCOMING_DOWNLOAD_FAILED_TOO_LARGE(111)`, `INCOMING_DOWNLOAD_FAILED_SIM_HAS_NO_DATA(112)`, `INCOMING_FAILED_TO_DECRYPT(113)`, `INCOMING_DECRYPTION_ABORTED(114)`, `INCOMING_DOWNLOAD_RESTRICTED(118)` |
| `unknown` | `STATUS_UNKNOWN(0)` **only**, or a value added upstream after this pin | `STATUS_UNKNOWN(0)` |

**System events (200–279) do not get a `delivery_state`.** They are stored with
`kind='system'`, carry `delivery_state='received'` for schema uniformity, and
are excluded from `messages.list` unless `include_system=true`. §13.4's
"every value mapped exactly once" assertion covers 0–27, 100–118 and 300; the
200–279 band is asserted separately as "classified `system`".

Permitted transitions for an **outgoing** message, enforced by a SQL trigger
as well as in Go:

```text
sending -> sent -> delivered -> read
sending|sent      -> failed
sending           -> canceled
any               -> deleted
unknown           -> any            (a late authoritative status corrects it)
```

A forward skip (`sending → delivered`) is **accepted** — the phone genuinely
reports coarse jumps. A *backward* move (`read → sent`) is refused, logged, and
audited as `message.status_out_of_order`, and the stored state is left alone.
`delivery_state_raw` is always overwritten with whatever Google last said, so a
reviewer can see the raw truth; it is served only on
`GET /v1/admin/diagnostics`, never on a public message DTO (§4.1).

`read` is a state on the *message*, not on the operation (§6.4).

### 4.5 The data key

`AGENT_GM_DATA_KEY` is a 256-bit key supplied as 64 hex characters or standard
base64. It derives, by HKDF-SHA256 with distinct `info` strings:

| Purpose | `info` |
|---|---|
| session file envelope (§3.3) | `agent-gm/session/v1`, with the `acct_` ID as AEAD associated data |
| `attachments.decryption_key` column encryption | `agent-gm/attachment-key/v1` |
| upload and download ticket signing (§10.3) | `agent-gm/ticket/v1` |
| pagination cursor signing (§7.4) | `agent-gm/cursor/v1` |

**One data key covers every account**, and it is **not rotatable in place**. A
database restored without the key that sealed it cannot decrypt any session
file or any attachment key. The key
and the data directory move together, always. If the key is lost: delete
`sessions/`, re-pair each account (§11.4), re-backfill. Message text survives
because it
is not encrypted at rest; cached media and the session do not.

Refusing to start with `session envelope cannot be decrypted` means the key
differs from the one that sealed the session. Restore the original key; there
is no in-place rotation.

### 4.6 Derived conversation fields

Two public fields are computed rather than stored raw, because the raw values
are `gmproto` internals that mean nothing to a caller (§18.1 rubric):

- **`conversation_type`** is `sms_mms` for `ConversationType_SMS(1)`, `rcs` for
  `RCS(2)`, `unknown` for `0`. Google's own SMS conversation carries MMS too,
  which is why the public value names both.
- **`capabilities.force_rcs`** is true exactly when
  `conversation_type == rcs` **and** `send_mode_raw == SEND_MODE_AUTO`. The
  raw `ConversationSendMode` (`SEND_MODE_AUTO`, `SEND_MODE_XMS`,
  `SEND_MODE_XMS_LATCH`) is **never served**; a caller that wants to know
  whether `force_rcs` will be accepted reads the capability, not the mode.

### 4.7 Account lifecycle

`accounts.state`:

| State | Meaning | Reads | Writes |
|---|---|---|---|
| `pairing` | a pair is in flight; no session yet. Never a resting state (§3.2) | — | — |
| `connected` | session valid, long poll up | yes | yes |
| `degraded` | transient listen error; retrying | yes | yes, likely to fail |
| `error` | the supervisor is retrying `Reconnect` with backoff | yes | refused |
| `signed_out` | **the owner signed this account out**, or its cookies died | **yes** | refused, `unsupported_capability` / `not_signed_in` |
| `parked` | **not scheduled**: `accounts.max_concurrent` is reached, so this account holds no client and no goroutine. Not an error, and not transient | yes | refused |
| `account_changed` | the phone switched Google accounts underneath us | yes | refused |

**Signing out keeps everything.** `agm accounts sign-out <id>`
(`POST /v1/accounts/{id}/sign-out`):

1. disconnects the `libgm` client and stops that account's ingest goroutine;
2. **shreds `sessions/<acct>.enc`** and zeroes the in-memory `AuthData`, so the
   Google account cookies are gone from disk and from the process;
3. sets `state='signed_out'`, `session_present=0`;
4. writes an audit row.

It deletes **no** conversation, message, attachment, reaction, contact or
operation. Everything stays readable and searchable — the account simply
appears in listings with `state: "signed_out"`. Any write naming it is
`unsupported_capability` with `details.reason = "not_signed_in"` (§7.7),
which is a per-account condition and therefore *not* the service-level
`not_paired`.

**Re-pairing resumes the same rows.** `agm pair` on an account that already
exists — recognised by `AuthData.Mobile.SourceID` hashing to an existing
`acct_` ID (§3.2, §4.1) — does not create a second account and does not
duplicate history:

- Conversation and message IDs are derived from `account_id` plus the **Google
  IDs**, which are stable per account, so every re-derived ID equals the stored
  one.
- Ingest is an upsert on `(conversation_id, source_id)` (§5.3), so backfill
  after a re-pair rewrites nothing it already has; `content_hash` means an
  unchanged row is not even touched.
- The account returns to `connected`, a full reconciliation sweep runs with
  `since = accounts.last_event_at_ms` (§5.4), and messages that arrived while
  it was signed out are ingested in the normal way.
- A re-pair that yields a **different** account address creates a **second**
  account; it never adopts the first one's rows. `agm pair --account <id>`
  makes the intent explicit and refuses with `pairing_wrong_account` if the
  signed-in address does not match.

**Removal is the only thing that deletes.** `agm accounts remove <id>`
(`DELETE /v1/accounts/{id}`) requires `{"confirm": true}` and an interactive
`y` or `--yes`, prints the exact effect sentence

> *"permanently deletes Agent GM's copy of this account's conversations,
> messages, attachments and operations; your Google Messages account and the
> messages in it are untouched"*

signs the account out first, then deletes its rows in one transaction
(`conversations` cascades to `participants`, `messages`, `attachments`,
`reactions` and `backfill_state`; `operations`, `contacts` and the account row
follow), collects the cached media paths inside that transaction, commits, and
only then unlinks the files (§10.3's erasure order). Audit rows are **not**
deleted — they record what happened when it happened (§4.3) — and they keep
their `account_id`, so the trail of a removed account survives it.

**Nothing else deletes anything.** Signing out, a
`RevokePairData` from the phone, cookie expiry, `account_changed`, a failed or
abandoned re-pair, and `accounts.max_concurrent` parking each delete **zero**
rows. That negative is half the meaning of "only", so §16 Slice 2 tests it
directly rather than inferring it from the positive case. The audit
row for the removal carries the row counts.

**Concurrency.** Accounts are independent. `internal/accounts` supervises one
`gm.Backend`, one ingest goroutine, one backfill worker and one sweep timer per
`connected` account. A `signed_out` or `error` account holds no goroutine and
no `libgm` client. `settings.accounts.max_concurrent` (default 8, bounds 1–32)
bounds how many run at once. Accounts are connected in `last_event_at_ms`
order, newest first; the rest are **`parked`** — a state of its own, not
`degraded`, because "waiting for a slot" and "retrying a listen error" are
different things and an agent reading `state` must be able to tell them apart.
A `parked` account is fully readable and its writes are refused with
`not_signed_in`. Every account DTO also carries **`state_reason`**, a short
machine-readable string (`capacity`, `listen_error`, `credentials`,
`revoked_by_phone`, `cookies_expired`, `account_switched`, `crash_recovered`)
or `null`, so `degraded` and `error` are diagnosable without reading logs.
Parking is re-evaluated whenever an account connects or disconnects.

---

## 5. Ingestion

### 5.1 The two sources of truth

Everything in `conversations`, `messages`, `attachments`, `reactions`,
`participants` and `contacts` comes from exactly two places:

1. **Backfill** — explicit `ListConversations` / `FetchMessages` /
   `ListContacts` calls (§5.2).
2. **Live events** — the `gm.Events()` channel (§3.4, §5.3).

They write through the **same upsert functions**. There is no separate
"backfill writer" whose behaviour could drift from the live path. This is the
single most important structural rule in this section: a bug in ordering or
dedup must be reproducible from either source.

### 5.2 Initial backfill

**Per account.** Each account backfills independently, triggered by its own
`events.ClientReady` and on demand via `POST /v1/admin/backfill`
(optionally `{"account_id": …}`). Every row written carries that account's
`account_id`.

```
1. Upsert the conversations carried on ClientReady.
2. ListConversations(INBOX, count=settings.backfill.conversation_page_size)
   -- this call is also what arms the library's BUGLE_MESSAGE mode (§3.7).
3. ListConversations(ARCHIVE, ...) if settings.backfill.include_archive.
4. ListContacts() and ListTopContacts(); upsert contacts; link participants.
5. For each conversation, oldest-activity first:
     FetchMessages(convID, count=settings.backfill.message_page_size, cursor=nil)
     loop on the returned Cursor until either
       - the page is empty, or
       - settings.backfill.max_messages_per_conversation is reached, or
       - a message older than settings.backfill.horizon is seen.
6. Record per-conversation progress in backfill_state so a restart resumes
   rather than restarting.
7. Set accounts.backfill_complete_at_ms for THIS account. (Never a
   server_meta key: two accounts would race on one row and the first to
   finish would mark the whole server complete.)
```

Concurrency is `settings.backfill.concurrency` (default 2, bounds 1–8)
conversations at a time **per account**, so the worst case is
`backfill.concurrency × accounts.max_concurrent` in-flight `FetchMessages`
calls. `backfill.max_messages_per_conversation` and `backfill.horizon` are
likewise per-server settings **applied to each account**, so a generous horizon
costs its value times the number of accounts (§15.1 gives every setting a
scope). **That account's** backfill is paused while a
`MOBILE_DATABASE_SYNC_STARTED`/`SYNCING` alert from **its** phone is
outstanding, and resumes on that phone's `MOBILE_DATABASE_SYNC_COMPLETE`
(§3.4), because results during a phone-side sync are unstable. One sleepy phone
never stalls another account.

Backfill never blocks reads. `GET /v1/health` reports a `backfill` block **per
account** (§7.5), and a list or search response carries a `history_incomplete`
warning until **every account in its scope** has `backfill_complete_at_ms` set
— so a cross-account read warns while account B is still backfilling even
though account A has finished, and an empty result is never read as an absent
message.

### 5.3 Live ingestion

**One goroutine per account**, `core.ingestLoop`, drains that account's
`gm.Events()` and applies each event as one store write transaction, stamping
its `account_id` on every row. Together they are the **only** writers of
message rows, and they serialise at the single store writer (§2.4).

For a `*libgm.WrappedMessage`:

```
1. If shouldIgnoreStatus(status, isDM) says ignore -> drop, count it, done.
   (The set is carried over verbatim from
   connector/handlegmessages.go:913-940.)
2. If status is in 200..279 -> kind="system", store, exclude from
   messages.list unless include_system=true.
3. Derive msg_ ID from (account_id, conversation source ID, message source ID).
4. Compute content_hash over the canonical content (text parts joined with
   \n, then each media part's mediaID and size, then the reaction set).
5. If a row with that ID exists:
     - if content_hash and delivery_state_raw are both unchanged -> no-op.
     - else update, honouring the transition rules of 4.4.
   Else insert.
6. Upsert attachments from MessageInfo entries.
7. Replace the reaction set from Message.Reactions (it is authoritative and
   complete, so this is a set-replace, not a merge).
8. If Message.TmpID matches an operation's tmp_id **for this account**,
   resolve that operation (6.3).
9. Update the conversation's last_activity_ms and latest_message_id.
```

Conversation events (`*gmproto.Conversation`) upsert the conversation row and
replace its participant set.

### 5.4 Ordering and deduplication

- **Ordering is by `sent_at_ms` descending, then `id` descending.** Google
  timestamps collide (two messages in the same millisecond is common in a
  burst), so the ID is always the tiebreaker and it is part of every cursor.
  Ordering is *never* by ingestion order; a backfilled message and a live
  message of the same age must sort identically.
- **Events can arrive out of order and can be replayed.** After a reconnect
  the server replays its backlog with `IsOld=true`. Agent GM does not use
  `IsOld` to decide whether to store — an old event may carry information the
  database lacks — it uses it only to suppress side effects: an `IsOld`
  message never triggers an operation resolution, never bumps
  `last_activity_ms` past a newer value, and is never counted as "new".
- **Three layers of dedup**, in order:
  1. `libgm`'s own 8-entry (id, payload hash) window (§3.4). Too small to rely
     on, and on a hit it abandons the rest of the batch; Agent GM assumes it
     does nothing except lose messages.
  2. The primary key on `(conversation_id, source_id)`, and `conversation_id`
     already carries the account — a repeat is an upsert, never a second row.
     Two accounts that are both in a group with the same person hold two
     separate conversations with two separate `conv_` IDs, which is correct:
     they are two threads on two phones.
  3. `content_hash` — an upsert whose content and raw status are both
     unchanged writes nothing at all, so a replay storm does not churn the WAL
     or bump `updated_at_ms`.
- **`last_activity_ms` is monotonic per conversation.** It is only ever moved
  forward, with `MAX(existing, new)`, so a replayed old message cannot make a
  conversation jump to the top of the list.

#### The reconciliation sweep

**The live event stream is not a complete record.** The library's dedup
abandons every remaining part of a batch on a hit (§3.4,
`event_handler.go:263-266,272-275`), so messages can be *lost*, not merely
duplicated, and no amount of local dedup recovers what never arrived.

Agent GM therefore runs `core.reconcile(account, since)`, **once per
`connected` account**, on that account's own schedule:

```
1. ListConversations(active, page_size) and, if backfill.include_archive,
   ListConversations(archived, page_size).
2. For each conversation whose last_activity_ms is newer than `since`, or
   whose latest_message_id is not a row we hold:
     FetchMessages(convID, page_size, cursor=nil), walking back until a
     message older than `since` is reached.
3. Upsert everything through the same path as 5.3. Unchanged rows write
   nothing (content_hash).
4. Record accounts.last_sweep_at_ms for this account.
```

It is triggered by:

| Trigger | `since` |
|---|---|
| `BROWSER_ACTIVE` alert whose session ID differs from the last seen, or after an inactive period (§3.4) | `last_event_at_ms` |
| `MOBILE_DATABASE_SYNC_COMPLETE` | `last_event_at_ms` |
| `events.NoDataReceived` | `last_event_at_ms` |
| `events.ListenRecovered`, and every successful `Connect` | `last_event_at_ms` |
| a timer per account, every `settings.ingest.sweep_interval` (default 15m, bounds 1m–6h) | that account's `last_sweep_at_ms` |
| `POST /v1/admin/backfill` with `{"account_id"}` | that account's `last_event_at_ms` |
| `POST /v1/admin/backfill` with no body | epoch, **for every account** — a full re-backfill of the whole server. This is the most expensive operation Agent GM offers, so the route requires `{"confirm": true}` when the body is otherwise empty, and `agm admin backfill` prints how many accounts and conversations it is about to walk |

The sweep is idempotent by construction — it writes through the same upsert
functions as §5.3 — so running it more often costs bandwidth and nothing else.
`GET /v1/health` reports `last_sweep_at` and `sweeps_total` **per account**.

### 5.5 Delivery-status transitions in practice

The sequence an agent observes for its own outgoing text, in the normal case:

```
POST /v1/conversations/{id}/messages      -> 200, operation succeeded,
                                             message_id present
message.delivery_state: sending           (echo, MessageStatusType 4 or 5)
                     -> sent              (OUTGOING_COMPLETE 1)
                     -> delivered         (OUTGOING_DELIVERED 2, RCS/SMS-DR)
                     -> read              (OUTGOING_DISPLAYED 11, RCS only)
```

Facts an implementer must not get wrong:

- **SMS usually stops at `sent`.** Delivery reports are carrier- and
  handset-dependent. `delivered` and `read` are RCS features in practice. An
  agent waiting for `delivered` on an SMS thread can wait forever; this is
  stated in the MCP instructions block (§8.3) and in `agm --help`.
- **Group threads frequently stop at `sent`.**
- The echo can arrive **before** the HTTP response is written, because the
  send is synchronous and the long poll is concurrent. The operation row is
  therefore committed *before* the library call is made (§6.2), so the echo
  always has something to attach to.
- A `read` state never regresses. See §4.4.

---

## 6. Operations: sends are synchronous

### 6.1 No outbox

Agent MX needed a durable outbox because it reconciled two systems of record.
Agent GM has one. `libgm.Client.SendMessage` is a **synchronous request that
returns the phone's own answer**, with a hard 60-second bound
(`responseHardTimeout`). Therefore:

- A send handler calls the library inline and answers with the real result.
- There is **no queue, no worker, no retry schedule owned by Agent GM**, and
  no `queued`/`accepted` states that mean "we have not tried yet".
- The only retries are the three upstream-derived ones for transient Google
  statuses (`FAILURE_2`, `FAILURE_3`; backoff `[3s, 8s, 20s]`; §3.7), executed
  inside the same request. Worst case is four library calls at
  `responseHardTimeout` = 60s each plus `3 + 8 + 20` = 31s of backoff, so
  **271s**. The HTTP handler's deadline is `settings.operations.send_deadline`,
  **default 300s** (bounds 60s–600s), which is above that bound so the last
  retry can actually complete; exceeding it returns `phone_not_responding` with
  the operation left `pending`.

The `operations` table still exists, for two reasons only: **idempotency**
and **status**. It is a record of what happened, not a queue of what to do.

### 6.2 The order of a mutation

This order is part of the contract and is tested (§13.2):

```
1. Authenticate; check scope.  -> 401 / 403 at the transport
2. Parse strictly.             -> invalid_request naming the key
3. Resolve the account.        -> invalid_request when ambiguous (7.3),
                                  not_found when unknown
4. Resolve the conversation.   -> not_found (indistinguishable from unseen)
5. Check the account is usable -> unsupported_capability / not_signed_in
6. Check it is actionable.     -> unsupported_capability + details.reason,
                                  and NO operation row is created
7. Look up the idempotency key.
     same key + same fingerprint  -> return the existing operation, do nothing
     same key + different body    -> idempotency_conflict, do nothing
8. INSERT the operation row with status='running' and COMMIT.
9. Call that account's libgm client.
10. UPDATE the operation with the outcome, and COMMIT.
11. Respond.
```

Step 8 committing *before* step 9 is what makes the crash story honest: if the
process dies between 6 and 8, the operation is found at startup in `running`
and is settled to `unknown` by the recovery pass (§6.5), never silently
retried. **Agent GM never re-sends a message on behalf of a crashed request.**

### 6.3 Idempotency

Carried over from Agent MX, unchanged in substance:

- **The idempotency key is required on every mutation.** It comes from the
  `Idempotency-Key` header or from `client_request_id` in the body. Supplying
  both with *different* values is `invalid_request`, because it is a
  contradiction rather than a preference. An empty key, a key over 200 bytes,
  or a key containing control characters is `invalid_request` naming
  `client_request_id` in `details.field`, and writes nothing.
- Uniqueness is scoped to **(authorization, account, operation kind, key)**.
  Two clients may use the same key value; one client may use one key for a send
  and for a mark-read; and the same key sending to two different accounts is
  two different operations, because it is two different messages to two
  different people.
- "The same request" is decided by a **SHA-256 over the canonically
  serialised body** (keys sorted, no insignificant whitespace), so reordered
  JSON keys are a replay and any changed value is not.
- **The mirror hazard, stated where it will be read.** Because the account is
  part of the tuple, reusing a key against a *different* `account_id` is not a
  replay — it is a new operation, and it **sends a second real message to a
  real person**. This is the safe direction for the store (nothing is silently
  adopted across accounts) and the dangerous direction for a caller that
  "retries" a failed send by switching accounts. Therefore both agent-facing
  surfaces refuse it rather than obeying it: **if an idempotency key has
  already been used by this authorization for this kind against a *different*
  account, the request is `invalid_request`** with
  `details.field = "client_request_id"`, naming the account the key was first
  used with. A genuinely new send to another account uses a new key. The CLI
  enforces the same rule for `--idempotency-key`, and every write tool's
  description says it.
- A replay returns the existing operation **and its `message_id`**, and sends
  nothing. A fresh key is a different call, not a repeat — every tool
  description and CLI help text says so, because this is the mistake that
  sends a second text message to a real person.
- Keys are retained for **30 days** (`settings.operations.idempotency_ttl`,
  bounds 1d–365d), after which the row is swept. A replay of a swept key is a
  new operation; the CLI and the tool descriptions state the retention.

The operation's `tmp_id` is a **bare UUID**, minted per send attempt and
written to the indexed `operations.tmp_id` column before the library call
(§3.1, F-13). It is *not* the `op_`-prefixed operation ID. It goes into all
three of `SendMessageRequest.TmpID`, `MessagePayload.TmpID` and
`MessagePayload.TmpID2`. When the remote echo arrives carrying that value, the
ingest loop looks up `tmp_id → operation_id`, writes the message's `msg_` ID
onto the operation, and, if the operation is `pending`, settles it (§6.4).
A retry within one request reuses the same `tmp_id`, so a retried send cannot
produce two correlations.

### 6.4 Operation status

| Status | Meaning | Terminal |
|---|---|---|
| `running` | in flight, or the process died mid-call | no |
| `succeeded` | the phone accepted it (`SendMessageResponse_SUCCESS`, or a `Success: true` for reactions/deletes) | yes |
| `pending` | **`libgm.ErrPhoneNotResponding` only.** The server accepted the request; the phone may still act on it when it wakes. This is *not* a failure. | no |
| — | there is no `queued` and no `accepted`: there is no queue (D4) | — |
| `failed` | the phone refused it, or a non-retryable error | yes |
| `unknown` | never settled within `settings.operations.pending_timeout` (default 24h, bounds 1h–7d), or recovered from a crash | yes |

Transitions:

```text
running -> succeeded | failed | pending
pending -> succeeded            (the echo arrived)
pending -> failed               (the echo arrived reporting a failed status)
pending -> unknown              (pending_timeout elapsed)
running -> unknown              (crash recovery)
unknown -> succeeded | failed   (late authoritative evidence)
```

`terminal_at_ms` records when the operation *first* became terminal and is
never cleared. A correction out of `unknown` records `corrected_at_ms`. A wait
that was satisfied by `unknown` is not retroactively unsatisfied.

The critical rule, stated once and tested: **`ErrPhoneNotResponding` produces
`pending`, not `failed`.** Reporting it as a failure invites the caller to
resend, which sends the message twice when the phone wakes up.

### 6.5 The operation object

Returned inline by every mutation, and by `GET /v1/operations/{id}` and the
`get_operation` tool. This is the only definition; every surface serves
exactly these fields.

```json
{ "id": "op_01k4...",
  "account_id": "acct_...",
  "kind": "send_text",
  "status": "succeeded",
  "terminal": true,
  "terminal_at": "2026-09-06T09:41:03.201Z",
  "corrected_at": null,
  "conversation_id": "conv_...",
  "message_id": "msg_...",
  "error": null,
  "created_at": "2026-09-06T09:41:02.980Z",
  "updated_at": "2026-09-06T09:41:03.201Z" }
```

- `kind` ∈ `send_text`, `send_media`, `start_conversation`, `mark_read`,
  `add_reaction`, `remove_reaction`, `delete_message`,
  `delete_conversation`.
- `status` is §6.4's vocabulary; `terminal` is derived from it and is never
  stored independently.
- `terminal_at` is the moment the operation *first* became terminal and is
  never cleared or rewritten. `corrected_at` is set when a late fact moves it
  out of `unknown`.
- `error` is `null` or the §7.1 error object — `{code, message, retryable,
  details}` — with the same codes as REST. A `pending` operation carries
  `error.code = "phone_not_responding"` with `retryable: true` **and
  `terminal: false`**, which is how a caller tells "not yet" from "no".
- `message_id` is `null` until the remote echo lands, including on a
  `succeeded` send whose echo has not yet arrived.
- Raw Google values (`google_status_raw`) are **not** in this object; they are
  served only on `GET /v1/admin/diagnostics`.

A caller sees only its own operations, keyed on the authorization that created
them; `admin` sees all. Another authorization's ID answers `not_found` with a
body byte-identical to a genuinely absent one. A malformed ID is
`invalid_request`.

### 6.6 Crash recovery

At startup, before the listener binds:

```
UPDATE operations
   SET status='unknown', terminal=1, terminal_at_ms=?, error_code='crash_recovered'
 WHERE status='running';
```

**This sweep and the `pending_timeout` reaper are process-wide, not
per-account, and deliberately so.** A crash is a property of the process, so
every account's in-flight operations are settled at once, before any listener
binds and before any account connects. Accounts are independent in their
*network* behaviour (§4.7); recovery from a process death is not an account
concern.

Each row is audited as `operation.crash_recovered`, with its `account_id`.
Nothing is retried. If the
send did reach Google, the remote echo will arrive on reconnect and correct
the operation to `succeeded` with a `message_id` — which is exactly why the
correction transition out of `unknown` exists.

A `pending` operation is left alone at startup; the reaper settles it at
`pending_timeout` measured from `created_at_ms`.

`operations.idempotency_key` rows are swept after
`settings.operations.idempotency_ttl` (§15.1); the sweep never deletes a row
that is not terminal.

---

## 7. REST API

Base: `https://gm.agent-wx.app/v1`. JSON in, JSON out. Every route requires a
bearer token except `/healthz`, `/oauth/*` and `/.well-known/*`.
### 7.1 Envelopes

Success:

```json
{ "data": {}, "next_cursor": null, "warnings": [], "request_id": "req_..." }
```

Error:

```json
{ "error": { "code": "not_found", "message": "No such conversation.",
             "retryable": false, "details": {} },
  "request_id": "req_..." }
```

**Strict parameter rejection.** Every `/v1` route rejects an unknown query
parameter or an unknown JSON body field with `invalid_request`, naming it in
`details.parameter` or `details.field`. Nothing is allowlisted, including
cache-busting parameters such as `_=`. A misspelled filter is refused rather
than silently ignored, because `?directon=incoming` returning a full
unfiltered list looks exactly like a correct answer. A refused request has no
effect. The rule applies to `/v1` only: `/mcp`, `/oauth/*` and
`/.well-known/*` keep their RFC behaviour and ignore what they do not
recognise, because those are the surfaces third-party MCP clients drive.

**Text normalisation is never silent.** A `reason` or a `filename` carrying
control characters or exceeding its length bound is accepted, cleaned, and
reported in `warnings` as `reason_normalized`, `reason_truncated`,
`filename_normalized` or `filename_truncated`. The bounds are **500 runes for
a `reason` and 255 for a `filename`** — 255 because that is the longest name
every filesystem Agent GM writes a cached file on will take, and a value that
cannot be written is not one worth keeping whole.

**The idempotency key has exactly two transports** (§6.3): the
`Idempotency-Key` header, or the `client_request_id` body field. **There is no
`?client_request_id=` query parameter on any route**, and one presented as a
query parameter is `invalid_request` naming it, like any other unknown
parameter. The two `DELETE` routes that would otherwise have no body accept a
JSON body for it (§7.6).

### 7.2 Error codes

| Code | HTTP | Retryable | Meaning |
|---|---|---|---|
| `invalid_request` | 400 | no | malformed, unknown parameter or field, wrong ID prefix, contradictory idempotency key |
| `invalid_token` | 401 | no | absent, expired, unknown, or wrong-audience bearer. Covers what Agent MX split into `authentication_required` |
| `insufficient_scope` | 403 | no | valid token, wrong scope |
| `not_found` | 404 | no | no such object. Byte-identical whether it never existed or the caller may not see it |
| `idempotency_conflict` | 409 | no | same key, different body |
| `not_paired` | 409 | no | **there is no Google Messages session at all.** Not used for a paired-but-unusable conversation — that is `unsupported_capability` (§7.7) |
| `pairing_no_cookies` | 409 | no | Google-account pairing attempted without cookies |
| `pairing_no_devices` | 409 | no | the account has no primary device |
| `pairing_wrong_emoji` | 409 | no | the owner tapped the wrong emoji |
| `pairing_cancelled` | 409 | no | dismissed, or "this is not me" |
| `pairing_timeout` | 409 | no | no response within the window |
| `pairing_init_timeout` | 409 | yes | `GaiaInitTimeout` (20s) elapsed. Carries `details.multiple_devices` when the account had more than one candidate. There is no `details.device_count`: the count is not on the error (§3.5, D31) |
| `pairing_wrong_account` | 409 | no | a cookie refresh whose Google account differs from the paired one (§3.2) |
| `pairing_no_account` | 409 | no | the pairing completed but `AuthData.Mobile.SourceID` was empty, so there is no account address to derive an `acct_` ID from (§3.2, §4.1). Nothing is created — §16 Slice 2 test 38 asserts it |
| `unsupported_capability` | 409 | no | the action cannot apply here; `details.reason` from §7.7 |
| `payload_too_large` | 413 | no | body over 1 MiB, or media over `media.upload_max_bytes` |
| `media_unsupported_type` | 415 | no | mime not in `libgm.MimeToMediaType` |
| `rate_limited` | 429 | yes | with `Retry-After` |
| `internal_error` | 500 | yes | a bug |
| `not_default_sms_app` | 502 | no | `SendMessageResponse_FAILURE_4` |
| `config_version_stale` | 502 | no | Agent GM's own diagnosis: a conversation-creating call failed **and** the compiled and live `ConfigVersion` differ (§3.7). The message names both and says the fix is a pin bump |
| `google_undocumented_status` | 502 | no | a Google enum value the pinned proto has no name for. `details.status` is the bare integer; no meaning is claimed (§3.7) |
| `google_error` | 502 | maybe | a tachyon error; `details.google_type` (integer), `details.google_message` |
| `google_http_error` | 502 | yes | transport level; `details.status` |
| `google_permission_denied` | 502 | no | `ErrCallerNoPermission` |
| `disconnected` | 503 | yes | `ErrConnectionClosed`; the long poll is down |
| `phone_not_responding` | 504 | yes | `ErrPhoneNotResponding`. **The operation is `pending`, not failed — do not resend.** |

A body over 1 MiB is `413` carrying `payload_too_large`; a body that fails to
read for any other reason is `400`, because "too large" would be a guess.

`details.google_type` and `details.status` are the only raw Google integers on
a public surface. They are diagnostic values with no Agent GM meaning, they are
never IDs, and they exist because an owner reading a `google_error` needs
something to search for. Every other raw Google value is `admin`-only (§4.1).

`401` and `403` carry `WWW-Authenticate` with `realm="agent-gm"`, an `error`
parameter, `resource_metadata` pointing at
`https://gm.agent-wx.app/.well-known/oauth-protected-resource/mcp`, and, for a
scope refusal, the `scope` the route requires.

### 7.3 Choosing an account

Every read that lists or searches, and every write, takes **`account_id`** —
one name on all three surfaces, per §11.3's rule and the rubric. It accepts an
`acct_` ID. The rule is the same
on all three surfaces (§8.2, §11.1):

- **Exactly one account exists** → the parameter may be omitted and defaults to
  it. A single-account deployment never has to think about accounts.
- **More than one exists** → a **write** must name one. Omitting it is
  `invalid_request` with `details.field = "account_id"` and
  **`details.accounts` listing the candidates** as `{id, google_account,
  state}`, so the caller can retry without a second round trip. A **read** may
  still omit it, and then covers every account; that is a useful default for
  "what came in today" and a dangerous one for a send, which is why they
  differ.
- **Zero accounts exist** → any write is `not_paired` (§7.2); reads return
  empty pages.
- An `acct_` ID that does not exist is `not_found`. A `conv_` or `msg_` ID that
  belongs to a *different* account than the `account_id` given is
  `invalid_request` naming both — never `not_found`, which would suggest the
  thread is gone.

**A conversation or message ID already implies its account** (§4.1), so
`account_id` is not required alongside one. It is required where the target is
addressed by phone number rather than by ID — `POST /v1/conversations` — and
accepted, for confirmation, elsewhere.

Every DTO that can appear in a multi-account result carries `account_id`:
conversations, messages, contacts, attachments, operations and search results.
A cursor is bound to the `account_id` filter like any other (§7.4), so
paging cannot silently change which accounts are in scope.

### 7.4 Pagination

Cursors are **opaque, HMAC-signed (data key, `agent-gm/cursor/v1`), and bound
to the endpoint and the filter set as written**. Reusing a cursor with
different filters is `invalid_request`. The binding is to the query as
written, not to a normalised form: a cursor issued without `folder` is not
valid when replayed with `folder=active`, although the two select the same
rows. A client that walks a listing sends the same query string on every page
anyway.

`limit` defaults to 50 and caps at 100 on every listing.
`GET /v1/messages/{id}/context` takes `before`/`after`, each defaulting to 5
and capped at 100. Ordering is newest-first unless the route says otherwise.
The cursor encodes `(sent_at_ms, id)` so it is stable across equal timestamps.

### 7.5 Routes — health, auth, accounts, pairing

| Method | Path | Scope | Notes |
|---|---|---|---|
| `GET` | `/healthz` | none | liveness. Never touches SQLite. `200 {"status":"ok"}` |
| `GET` | `/v1/health` | `messages:read` | see the DTO below |
| `POST` | `/v1/auth/admin-session` | none — presents `AGENT_GM_ADMIN_SECRET` in the body | `{"secret": "..."}`. Returns an access token, a refresh token and the granted scopes. **See §9.7 for what an admin session carries.** |
| `POST` | `/v1/auth/refresh` | none — presents the admin refresh token in the body | rotates it. Reuse of a spent token revokes the session. OAuth refresh tokens are refused here (§9.6) |
| `GET` | `/v1/auth/whoami` | `messages:read` | `{authorization_id, kind, scopes, client_id, expires_at}` |
| `POST` | `/v1/auth/logout` | `messages:read` | revokes the calling authorization's tokens. Named `logout`, not `sign-out`, on purpose: it ends a *token's* session and has nothing to do with signing a Google **account** out (§4.7). Same scope as `whoami`: a token that cannot read cannot ask who it is |
| `GET` | `/v1/accounts` | `messages:read` | every account, whatever its state. `{id, google_account, label, state, state_reason, pairing_id, phone_id, phone_responding, paired_at, last_event_at}`. `google_account` is served because the owner needs to tell their accounts apart; it is never an ID and never in a URL |
| `GET` | `/v1/accounts/{account_id}` | `messages:read` | one of them, plus that account's `google`, `backfill`, `sweep` and `counters` blocks — the same per-account object `GET /v1/health` embeds. The list route omits those four to keep a many-account listing small; that asymmetry is deliberate and is why `get_session` (§8.2) exists alongside `list_accounts` |
| `PATCH` | `/v1/accounts/{account_id}` | `admin` | `{"label"?}` — a human name for a listing. Nothing else is mutable |
| `GET` | `/v1/accounts/{account_id}/events` | `messages:read` | **SSE**, one event per state change for that account, plus a 30s heartbeat. The only streaming route; it carries no message data, so it needs no replay ring and no cursor. Backs `agm session --watch`. Omitting the ID (`/v1/accounts/events`) streams every account's changes, each tagged |
| `POST` | `/v1/accounts/{account_id}/reconnect` | `admin` | force `Reconnect()` on that account |
| `POST` | `/v1/accounts/{account_id}/sign-out` | `admin` | §4.7. Shreds the session file, keeps every row. Requires `{"confirm": true}` |
| `DELETE` | `/v1/accounts/{account_id}` | `admin` | §4.7. **The only route that deletes an account's data.** Requires `{"confirm": true}`; returns the deleted row counts and the `effect` sentence |
| `POST` | `/v1/pairing/start` | `admin` | `{"cookies": {...}, "device_index"?: 0, "account_id"?}` → `{pairing_id, emoji}`. **This adds an account, or resumes an existing one** (§4.7). `account_id` asserts which account is expected; a mismatch is `pairing_wrong_account`. There is one pairing flow, so there is no `method` field; a body carrying one is `invalid_request` naming it |
| `GET` | `/v1/pairing/{pairing_id}` | `admin` | poll: `{state: waiting\|paired\|failed\|expired, account_id?, emoji?, error?}`. `account_id` appears once the address is known |
| `DELETE` | `/v1/pairing/{pairing_id}` | `admin` | abandon an in-flight pairing |
| `POST` | `/v1/accounts/{account_id}/refresh-cookies` | `admin` | `{"cookies": {...}}`. Re-authenticates that account (§3.2). A different Google address is `pairing_wrong_account` |

Account `state` vocabulary is §4.7's: `pairing`, `connected`, `degraded`,
`error`, `signed_out`, `parked`, `account_changed`. There is no server-level
"unpaired" state — an Agent GM with no accounts is a healthy Agent GM with no accounts.

`GET /v1/accounts` and `GET /v1/health` are `messages:read`, and both return
every account's `google_account`. Under D29 a `messages:read` token therefore
**enumerates all of the owner's Google addresses**; that is a consequence of
global scopes, and it is what the authorization screen discloses (§9.4).

`GET /v1/health`:

```json
{ "status": "ok",
  "version": "1.0.0", "commit": "abc1234",
  "source_url": "https://github.com/thisnick/agent-gm/tree/abc1234",
  "config_version_compiled": "2026.9.2",
  "upstream_commit": "be48a58",
  "accounts_summary": { "total": 2, "connected": 1, "degraded": 0,
                        "error": 0, "signed_out": 1,
                        "backfill_complete": 2 },
  "accounts": [
    { "account_id": "acct_...", "google_account": "…", "label": "personal",
      "state": "connected", "phone_responding": true,
      "last_event_at": "2026-09-06T09:40:59.000Z",
      "google": { "config_version_live": "2026.9.2",
                  "config_version_stale": false,
                  "is_default_sms_app": true },
      "backfill": { "state": "complete", "conversations_done": 41,
                    "conversations_total": 41,
                    "completed_at": "2026-09-06T09:12:00.000Z" },
      "sweep": { "last_sweep_at": "2026-09-06T09:38:00.000Z",
                 "sweeps_total": 12 },
      "counters": { "dropped_events": 0, "unknown_events": 0,
                    "pending_operations": 0 } },
    { "account_id": "acct_...", "google_account": "…", "label": "work",
      "state": "signed_out", "phone_responding": false,
      "last_event_at": "2026-09-05T21:02:11.000Z",
      "google": null, "backfill": { "state": "complete", … }, … }
  ],
  "client_source": "100.64.0.7" }
```

`config_version_live` and `is_default_sms_app` are per account, cached from
that account's last `FetchConfig` and `IsBugleDefault`, refreshed on connect
and every `settings.ingest.sweep_interval`, so `/v1/health` never blocks on a
phone. They are `null` for an account that is not `connected`.

**`status` describes the server, not the accounts.** It is `ok` whenever the
process is serving: an account in `signed_out` or `error` is a fact about that
account, reported in its row and in `accounts_summary`, and it does **not**
make Agent GM unhealthy. A deployment with zero accounts is `ok`. Anything
watching for "is the service up" reads `status`; anything watching for "can I
send" reads the account.

### 7.6 Routes — reads

`messages:read` on all of these.

| Method | Path | Parameters |
|---|---|---|
| `GET` | `/v1/conversations` | `account_id`, `query`, `participant`, `folder` (`active`\|`archived`\|`spam_blocked`), `type` (`sms_mms`\|`rcs`), `unread_only`, `group_only`, `include_deleted`, `cursor`, `limit` |
| `GET` | `/v1/conversations/{conversation_id}` | — |
| `GET` | `/v1/conversations/{conversation_id}/messages` | `cursor`, `limit`, `direction` (`incoming`\|`outgoing`), `sender`, `after`, `before` (RFC 3339), `has_attachment`, `delivery_state`, `include_system` |
| `GET` | `/v1/messages` | the same plus `account_id` and `conversation_id` |
| `GET` | `/v1/messages/{message_id}` | — |
| `GET` | `/v1/messages/{message_id}/context` | `before` (default 5, max 100), `after` |
| `GET` | `/v1/messages/{message_id}/attachments` | metadata only. Folded into `get_message` on MCP (§8.2) |
| `GET` | `/v1/search/messages` | `q` (required), `account_id`, `mode` (`words` default, `exact`), `conversation_id`, `sender`, `after`, `before`, `has_attachment`, `cursor`, `limit` |
| `GET` | `/v1/contacts` | `account_id`, `query`, `top`, `cursor`, `limit`. Returns `{id, account_id, display_name, phone, is_top, avatar_hash, updated_at}` — `account_id` is always present, because the same person in two accounts is two contact rows and a cross-account list would otherwise be unattributable. `avatar_hash` is a SHA-256 of the avatar bytes or `null`; there is no avatar *content* route, because Agent GM stores the hash so a caller can detect a change, not the picture |
| `GET` | `/v1/attachments/{attachment_id}` | metadata + a download ticket (§10) |
| `GET` | `/v1/attachments/{attachment_id}/content` | bytes. Access token **or** download ticket |
| `GET` | `/v1/operations` | `messages:write`. The caller's own operations: `account_id`, `kind`, `status`, `terminal`, `after`, `before`, `cursor`, `limit`. Newest first |
| `GET` | `/v1/operations/{operation_id}` | `messages:write`. The §6.5 object |
| `GET` | `/v1/uploads/{upload_id}` | `messages:write`. The caller's own reservation |

`participant` accepts an E.164 number (`+12025550123`), the bare digits, a
national form, or a `part_`/`contact_` ID. `sender` accepts the same plus the
literal `me`.

**Matching across accounts.** A raw phone number is not account-specific — the
same person can be in threads on several of the owner's accounts — so:

- With `account_id`, a number matches participants in **that account only**,
  served by `participants(account_id, phone_e164)`.
- Without it, a number matches in **every** account and the results carry
  `account_id` so the caller can tell them apart, served by
  `participants(phone_e164)`. Both directions are indexed (§4.2).
- A `part_` ID already belongs to one account; passing it with a
  **different** `account_id` is `invalid_request` naming both.
- **`sender=me` means "whichever account's own participant"**, resolved per
  account rather than to one identity: on an account-scoped query it is that
  account's `is_me` participant, and on a cross-account query it is *each*
  account's, so `sender=me` returns everything the owner sent from anywhere.
  It is never ambiguous and never an error. `participants(account_id, is_me)`
  is a partial index for exactly this.

**Search has two modes and no SQLite vocabulary.** `words` tokenises the query
and requires every term (the ordinary case). `exact` matches the phrase as
written. Both run against `messages_fts`; `exact` additionally filters the page
by substring, so neither is an unindexed scan. There is no way to inject FTS5
operator syntax, and no mode named after a SQLite extension.

Search returns `results[{message, rank, snippet, conversation}]` plus:

```json
"coverage": { "complete": false,
              "oldest_indexed_at": "2025-09-06T00:00:00.000Z",
              "conversations_pending": 3 }
```

`coverage.complete` is false while backfill is outstanding, and the response
carries a `history_incomplete` **warning** (not an error code), so an empty
result during backfill is not read as an absent message.

Conversation DTO:

```json
{ "id": "conv_...", "account_id": "acct_...", "name": "Alex", "is_group": false, "type": "rcs",
  "folder": "active", "unread": true, "pinned": false, "read_only": false,
  "participants": [ { "id": "part_...", "contact_id": "contact_...",
                      "display_name": "Alex", "phone": "+12025550123",
                      "is_me": false } ],
  "peer_typing_until": null,
  "is_deleted": false, "deleted_at": null,
  "last_activity_at": "2026-09-06T09:41:02.115Z",
  "latest_message_id": "msg_...",
  "capabilities": { "send_text": true, "send_media": true, "reply": true,
                    "react": true, "mark_read": true, "typing": true,
                    "force_rcs": true, "archive": true, "pin": true,
                    "delete_message": true, "delete_conversation": true },
  "created_at": "…", "updated_at": "…" }
```

`is_deleted` is true once Google's delete-for-me has been applied to this
thread (`conversations.deleted_at_ms`), which is what `include_deleted`
filters on. A deleted conversation keeps its ID and its indexed history; only
Google's copy is gone.

`peer_typing_until` is an RFC 3339 instant or `null`. It is held in memory
only, never persisted, and is always `null` in a list response — typing is
per-conversation live state, so it is populated only by
`GET /v1/conversations/{id}`.

**`participants` are the people in a thread; `recipients` are the phone
numbers you address when creating one** (§7.6). They are two different things,
not two names for one, and Google's own group-messaging setting uses
"recipients" the same way.

Message DTO:

```json
{ "id": "msg_...", "account_id": "acct_...", "conversation_id": "conv_...",
  "kind": "message",
  "direction": "outgoing", "sender": { "id": "part_...", "is_me": true },
  "text": "on my way", "subject": null,
  "delivery": { "state": "delivered", "error": null,
                "updated_at": "2026-09-06T09:41:07.900Z" },
  "reply_to_message_id": null, "operation_id": "op_...",
  "attachments": [ { "id": "att_...", "mime_type": "image/jpeg",
                     "filename": "IMG_0421.jpg", "size": 184320,
                     "width": 1024, "height": 768,
                     "download_state": "available" } ],
  "reactions": [ { "id": "react_...", "emoji": "👍", "type": "like",
                   "participant_id": "part_...", "is_mine": false } ],
  "is_deleted": false,
  "sent_at": "2026-09-06T09:41:02.115Z" }
```

A reaction whose `EmojiType` has no unicode serves
`{"emoji": null, "type": "emotify"}` rather than being dropped (§3.7).

### 7.7 Routes — writes

`messages:write` unless noted.

| Method | Path | Body | Answer |
|---|---|---|---|
| `POST` | `/v1/conversations` | `{"account_id", "recipients": ["+1…"], "name"?, "client_request_id"}`. `account_id` is required when more than one account exists (§7.3) — this is the one write whose target is a phone number rather than an ID, so nothing else can imply the account | `200` with the existing or newly created conversation plus the operation. `GetOrCreateConversation`; the `CREATE_RCS` retry of §3.7 is internal. `name` is accepted only for 2+ recipients. Zero recipients, or two that normalise to one number, is `invalid_request` **before** an operation row exists |
| `POST` | `/v1/conversations/{id}/messages` | `{"text"?, "upload_ids"?, "reply_to_message_id"?, "force_rcs"?, "client_request_id"}` | `200` with `{operation, message_id}`. At least `text` or one upload. `upload_ids` is an array but **currently accepts exactly one element**; two is `invalid_request` naming the limit (§10.2) |
| `POST` | `/v1/conversations/{id}/typing` | `{}` | `204`. Fire-and-forget, no operation, no idempotency key: it has no lasting effect |
| `POST` | `/v1/conversations/{id}/read` | `{"message_id", "client_request_id"}` | `200`. Marks the conversation read through that message |
| `PATCH` | `/v1/conversations/{id}` | `{"folder"?, "pinned"?, "unread"?, "client_request_id"}` | `200`. Archive, unarchive, pin, unpin and mark-unread, through `UpdateConversation` (§3.1). Returns `operation: null` and `changed: false` when already in the requested state |
| `POST` | `/v1/messages/{id}/reactions` | `{"emoji", "client_request_id"}` | `200`. `SendReaction` `ADD`, or `SWITCH` when the owner already has a different reaction. `operation: null` when the owner already has exactly that one. `emoji` is canonicalised first (§3.7) |
| `DELETE` | `/v1/messages/{id}/reactions/{emoji}` | `{"client_request_id"}` | `200`. `REMOVE`. `operation: null` when there is nothing to remove. The path segment is canonicalised before matching |
| `DELETE` | `/v1/reactions/{reaction_id}` | `{"client_request_id"}` | `200`. The same removal by `react_` ID. A reaction somebody else sent is `unsupported_capability` with `reason: "not_my_reaction"` |
| `POST` | `/v1/uploads` | §10.2 | `201` with an upload ticket |
| `DELETE` | `/v1/uploads/{upload_id}` | — | `204`. Drops the reservation and its staged bytes |
| `PUT` | `/v1/uploads/{upload_id}/content` | raw bytes | authenticated by the **upload token**, not the access token |

`messages:delete` — and only these two:

| Method | Path | Body | `effect` |
|---|---|---|---|
| `DELETE` | `/v1/messages/{message_id}` | `{"client_request_id"}` | *"deletes this message from your Google Messages account only; the recipient keeps it"* |
| `DELETE` | `/v1/conversations/{conversation_id}` | `{"client_request_id"}` | *"deletes this conversation from your Google Messages account only; the other people in it keep it"* |

Both responses carry an **`effect` field** holding that exact sentence. The
same string is the MCP tool description's closing sentence and the `agm`
confirmation prompt, so a model or a human cannot read a broader claim off one
surface than another (§11.3). There is no other delete and no delete option: a
request carrying an `action`, `scope` or similar switch is `invalid_request`
naming it.

`admin`:

| Method | Path | Notes |
|---|---|---|
| `GET` | `/v1/admin/settings` | effective value, source (`default`\|`environment`\|`database`), mutability, restart requirement |
| `GET` | `/v1/admin/settings/{key}` | one key, same shape |
| `PATCH` | `/v1/admin/settings` | validates the whole body; any invalid key rejects the request and changes nothing |
| `POST` | `/v1/admin/backfill` | `{"account_id"?, "conversation_id"?}`; re-opens backfill for one conversation, one account, or every account |
| `POST` | `/v1/admin/backup` | writes `<data_dir>/backups/agent-gm-<ts>-<id>.sqlite3` via the SQLite backup API. The caller does not choose the path |
| `GET` | `/v1/admin/audit` | `kind`, `kind_prefix`, `account_id`, `authorization_id`, `after`, `before`, `cursor`, `limit` |
| `GET` | `/v1/admin/diagnostics` | `?account_id=`; the raw Google view for one account or all, and the **only** place raw values appear: last 100 events by type, `dropped_events`, `unknown_events`, per-message `delivery_state_raw`, `operations.google_status_raw`, `CurrentSessionID`, the gaia device that was chosen, compiled/live `ConfigVersion` |
| — | `/v1/admin/enrollment-codes`, `/v1/admin/authorization-requests`, `/v1/admin/authorizations`, `/v1/admin/clients` | §9 |

### 7.8 `unsupported_capability` reasons

A closed vocabulary, in `details.reason`. Emitted **before** any operation row
exists, so a refused action never leaves a record that looks like an attempt.
The order of checks is part of the contract: resolve the object, check the
capability, validate the request, *then* create the operation.

| Reason | Meaning |
|---|---|
| `not_signed_in` | **the account this touches is `signed_out`, `error`, `parked` or `account_changed`** (§4.7) — every state that reads but does not write. Its history stays readable; only writes are refused. `parked` is in the list because a parked account holds no client to write with; an agent that wants to know *why* reads the account's `state` and `state_reason`, which distinguish "waiting for a slot" (`capacity`) from the rest. Distinct from the service-level `not_paired`, which means there are no accounts at all |
| `conversation_read_only` | `Conversation.ReadOnly` is set |
| `conversation_deleted` | delete-for-me has been applied locally |
| `not_my_message` | deleting a message the owner did not send |
| `not_my_reaction` | removing somebody else's reaction |
| `reply_not_supported` | `reply_to_message_id` on an SMS/MMS conversation; replies are RCS-only |
| `rcs_not_available` | `force_rcs` where `capabilities.force_rcs` is false (§4.6) |
| `media_pending` | the attachment's bytes are not downloaded yet |

An `unsupported_capability` answer carries the object ID, the action, the
capability's current value, and `details.reason`.

**`not_paired` is not in this table.** Having **no accounts at all** is a
service-level condition rather than a property of a conversation, so it is the
top-level `not_paired` code of §7.2. Having an account that is merely not
usable right now is `unsupported_capability` with `not_signed_in`, because the
conversation exists, is readable, and will be writable again after
`agm pair`. The two are never interchangeable.

## 8. MCP

### 8.1 Transport

Streamable HTTP at `https://gm.agent-wx.app/mcp`. Protocol revision
`2026-07-28`, with `2025-11-25` accepted for compatibility. Stateless at the
application layer: every request carries its own bearer token and its own
protocol metadata, and durable state lives in SQLite.

Checked **before the transport parses anything**:

- `Origin`, when present, must equal `https://gm.agent-wx.app`; a foreign one
  is `403`. A non-browser client sending no `Origin` is supported.
- Body over 1 MiB → `413`.
- `Content-Type` must be `application/json`.
- `Accept` must admit `application/json` or `text/event-stream`.
- Exactly one `Authorization` header is parsed, and only the `Bearer` scheme.
  Two headers, another scheme, or a value carrying two tokens are all refused
  rather than resolved to whichever happens to be first.
- The token must carry **at least one** messaging scope. No token → `401` with
  the RFC 9728 challenge. Valid token with no messaging scope → `403`
  `insufficient_scope` with the same challenge, deliberately distinct from
  `401`.
- Concurrency: 8 in flight per authorization, 32 across the process; excess is
  `429` with `Retry-After`.

`serverInfo` carries `name: "agent-gm"`, the version, the built commit, and
`source_url` (§1.4).

### 8.2 Tools

**Twenty-one tools** — eleven reads, eight writes, two deletes. Each is a facade over
the REST route that serves the same data, so the filters, the validation, the
error codes and the DTOs are the same on both surfaces by construction rather
than by discipline.

**MCP serves the messaging surface, not the whole API.** A `/v1` route has a
tool if and only if it is something an agent does with messages. The
exclusions, and the reason for each:

| Excluded route(s) | Reason |
|---|---|
| `POST /v1/conversations/{id}/typing` | no lasting effect and no result a model can act on |
| `GET /v1/messages/{id}/attachments` | its data is already inside `get_message`; a tool would only add a round trip |
| `GET`, `DELETE /v1/uploads/{id}` | an agent that has just called `create_upload` already holds everything they would return |
| `GET /v1/auth/whoami`, `POST /v1/auth/logout` | **credential self-management.** A model does not choose its own token, cannot act on the answer, and must not be able to log its client out mid-conversation. The client owns its credential; the model does not |
| `POST /v1/auth/admin-session`, `POST /v1/auth/refresh` | credential *issuance*. Reachable only by presenting a secret or a refresh token, neither of which a model holds |
| `GET /v1/accounts/{account_id}/events` and its all-accounts form | a **stream**; MCP tools are request/response. `get_session` and `list_accounts` answer the same question at a point in time |
| `GET /v1/attachments/{id}/content` | served as an MCP **resource**, `agm://attachments/{id}`, not as a tool. Bytes belong in a resource so a client can fetch them without putting them through the model's context |
| `GET /v1/operations` | `get_operation` covers the ID-addressed case, the only one a model reaches: it holds the `operation_id` the write returned. Listing operations is an owner's audit question, served by `agm operations list` |
| all `/v1/pairing/*` | `admin` scope. Pairing is a physical act at the owner's browser and phone (§11.4); no token an agent can hold reaches these |
| `PATCH`, `DELETE /v1/accounts/{id}`, `/sign-out`, `/reconnect`, `/refresh-cookies` | `admin` scope. Signing an account out and **deleting** its history are owner acts with confirmation prompts; a model must not be able to do either. `list_accounts` and `get_session` give it everything it can act on |
| every `/v1/admin/*` | `admin` scope, same reason. The two diagnostics an agent genuinely needs — backfill progress and the two settings that break sending — are served by `get_health` instead, without exposing the raw Google view |

So the rule is: **every `/v1` route carrying a `messages:*` scope is served to
MCP in exactly one of three ways — as a tool, as a resource, or as a named
exclusion above.** Every `admin`-scoped route and every credential route is
served in none of the three, by design. §16 Slice 3 test 16 is a two-way table
test over exactly that statement, with three categories rather than two.

**Reads — `messages:read`**

| Tool | REST | Arguments |
|---|---|---|
| `list_conversations` | `GET /v1/conversations` | `account_id`, `query`, `participant`, `folder`, `type`, `unread_only`, `group_only`, `include_deleted`, `cursor`, `limit` |
| `get_conversation` | `GET /v1/conversations/{id}` | `conversation_id` |
| `list_messages` | `GET /v1/messages` | `account_id`, `conversation_id`, `cursor`, `limit`, `direction`, `sender`, `after`, `before`, `has_attachment`, `delivery_state`, `include_system` |
| `get_message` | `GET /v1/messages/{id}` | `message_id` |
| `message_context` | `GET /v1/messages/{id}/context` | `message_id`, `before`, `after` |
| `search_messages` | `GET /v1/search/messages` | `q` (**required**), `account_id`, `mode` (`words`\|`exact`), `conversation_id`, `sender`, `after`, `before`, `has_attachment`, `cursor`, `limit` |
| `get_attachment` | `GET /v1/attachments/{id}` | `attachment_id` |
| `list_contacts` | `GET /v1/contacts` | `account_id`, `query`, `top`, `cursor`, `limit` |
| `get_session` | `GET /v1/accounts/{id}` | `account_id` (optional under the §7.3 rule) |
| `get_health` | `GET /v1/health` | — . Serves per-account backfill progress, `is_default_sms_app` and `config_version_stale`, which §15.4 makes the primary diagnostics |
| `list_accounts` | `GET /v1/accounts` | — . Every Google account this server holds, with its `acct_` ID, address, label and state. **A model calls this first when it does not already know an `account_id`** |

**Writes — `messages:write`**

| Tool | REST | Arguments |
|---|---|---|
| `send_message` | `POST /v1/conversations/{id}/messages` | `conversation_id`, `text`, `upload_ids`, `reply_to_message_id`, `force_rcs`, `client_request_id` (required) |
| `start_conversation` | `POST /v1/conversations` | `account_id`, `recipients` (E.164 array), `name`, `client_request_id` |
| `mark_read` | `POST /v1/conversations/{id}/read` | `conversation_id`, `message_id`, `client_request_id` |
| `add_reaction` | `POST /v1/messages/{id}/reactions` | `message_id`, `emoji`, `client_request_id` |
| `remove_reaction` | `DELETE /v1/messages/{id}/reactions/{emoji}` or `DELETE /v1/reactions/{id}` | either `reaction_id`, or `message_id` plus `emoji`; supplying neither is an `invalid_request` result. Plus `client_request_id` |
| `update_conversation` | `PATCH /v1/conversations/{id}` | `conversation_id`, `folder`, `pinned`, `unread`, `client_request_id` |
| `create_upload` | `POST /v1/uploads` | `filename`, `mime_type`, `size_bytes` (required), `sha256`, `client_request_id` |
| `get_operation` | `GET /v1/operations/{id}` | `operation_id`. **A read, but gated on `messages:write`**: it exposes only operations the caller created, and `messages:write` is the scope that creates them. Its annotations say `readOnlyHint: true`; visibility and read-ness are different questions |

**Deletes — `messages:delete`**

| Tool | REST | Arguments |
|---|---|---|
| `delete_message` | `DELETE /v1/messages/{id}` | `message_id`, `client_request_id` |
| `delete_conversation` | `DELETE /v1/conversations/{id}` | `conversation_id`, `client_request_id` |

Emoji arguments are canonicalised through `EmojiType` before anything else
happens (§3.7), and their descriptions name the eleven reactions Google's own
picker offers and say that anything else becomes a custom reaction that may not
render on the recipient's phone.

Schema rules:

- Every input schema is **closed** (`additionalProperties: false`), enforced by
  the Go decoder rejecting unknown fields, not only declared. A model that
  invents an argument name is told which one, in a result it can read.
- **Every argument carries a description** — not only the ones the MCP layer
  declares for itself. The claim is measurable: fetch `tools/list` from a
  running server, count the arguments, count the descriptions; they must be
  equal, and a single stale description fails the claim (§13.5).
- Closed vocabularies (`folder`, `type`, `direction`, `delivery_state`,
  `mode`) are real JSON Schema `enum`s with a per-value description, plus a
  `oneOf` of `const`s, because that is the only place JSON Schema lets a
  per-value description live. Optional *filters* stay nullable strings that
  name their accepted values in the description, because an `enum` that
  omitted `null` would make omitting the filter invalid.
- Every tool declares an `outputSchema` describing the envelope it really
  returns, with `data` typed by that tool's own DTO.
- Every conversation argument is `conversation_id` and takes a `conv_` ID;
  every message argument is `message_id` and takes a `msg_` ID. Google's own
  IDs are not accepted in their place.

Results carry `structuredContent` = `{ "data", "next_cursor", "warnings" }` —
the REST envelope minus the request ID — plus a one-line text summary. The
text half says what was returned and whether more exists, so a model that
reads only the text is not misled about completeness.

Annotations, accurate rather than conventional:

| Tools | `readOnlyHint` | `destructiveHint` | `idempotentHint` | `openWorldHint` |
|---|---|---|---|---|
| all reads, `get_operation`, `get_session`, `get_health`, `list_accounts` | true | false | true | false |
| `create_upload`, `update_conversation` | false | false | true | **false** |
| `send_message`, `start_conversation`, `mark_read`, `add_reaction` | false | false | true | true |
| `remove_reaction` | false | **true** | true | true |
| `delete_message`, `delete_conversation` | false | true | true | **false** |

The two deletes are `openWorldHint: false` because Google's delete is
delete-for-me: it changes the owner's own copy and nothing leaves the
building. `create_upload` and `update_conversation` are `false` for the same
reason — a reservation is purely local, and archiving or pinning is a change to
the owner's own thread list. `remove_reaction` *is* open-world and destructive:
it removes something the owner sent and the recipient sees it go.

All writes are `idempotentHint: true`, and that is true **because**
`client_request_id` is required. Every write tool's description ends with the
same sentence: *"Repeating this call with the same client_request_id returns
the same operation and sends nothing further. A fresh client_request_id is a
different call, not a repeat."*

**`isError` semantics.** A domain failure is a **result** with `isError: true`
carrying the REST error envelope in `structuredContent.error`. That covers
every code in §7.2 except the transport-level ones. Only a malformed request,
an unknown method, an unknown tool name, or an authorization failure at the
transport is a JSON-RPC error. The split matters: many MCP clients surface a
JSON-RPC error as a transport failure and never hand it to the model, so a
`not_found` reported that way is a fact the model never learns and cannot
correct itself from.

`insufficient_scope` appears on both sides deliberately, addressed to
different readers. The transport's `401`/`403` is addressed to the client. A
scope refusal *inside* a tool call is addressed to the model, so it is a
result with `isError: true` naming `required_scope`, because that one the
model can act on by choosing a different tool.

**Scope gating.** `tools/list` returns only the tools the calling
authorization may use, so a model is never invited to attempt something that
will be refused. `tools/call` checks again: visibility is not authorization,
and a client may call a name it learned elsewhere.

**Resources.** `resources/read` serves `agm://attachments/{attachment_id}`
under `messages:read`, with the same media limit and cache path as
`GET /v1/attachments/{id}/content`. Text media is returned as text; everything
else base64. `resources/list` is empty on purpose — attachments are addressed
by template, not enumerated. `resources/templates/list` offers the attachment
template, and only to a caller holding `messages:read`.

`get_attachment` decides its content form by **size and type, not
preference**: a supported image under `settings.media.inline_mcp_image_max_bytes`
(default 1 MiB) comes back as image content in the result; anything larger or
non-inlinable comes back as an `agm://attachments/{id}` resource link. The
download ticket comes back either way. The summary text is the first content
block, so a client that reads only the first block reads a sentence rather
than a megabyte of base64.

### 8.3 The `instructions` block

Returned from `initialize`. Written for an agent that has never seen this
server, has no memory of prior calls, and cannot ask a human. It is
reproduced verbatim in `docs/mcp.md` so the two can be diffed by a test.

> **This server is one person's Google Messages — possibly more than one
> account of it.** Each account is paired directly with an Android phone, the
> way the Google Messages web client pairs. Everything you can see here, they
> can see in the Messages app on that phone, and everything you send leaves as
> a real SMS, MMS or RCS message to a real person. There is no sandbox and no
> undo.
>
> **Accounts.** Call `list_accounts` first if you do not already know which
> account you are working in. Each has an ID starting `acct_`, the Google
> address it belongs to, a label the owner chose, and a `state`. **If there is
> exactly one account you can leave `account_id` out of every call and it will
> be used.** If there is more than one, reads without `account_id` cover all of
> them, but **a write must name one** — omit it and you get `invalid_request`
> listing the accounts to choose from, which you can retry against
> immediately. A `conv_` or `msg_` ID already belongs to one account, so you
> never need to pass both.
>
> An account whose `state` is not `connected` is still fully readable — its
> history is here — but writes to it are refused with
> `unsupported_capability` and `reason: "not_signed_in"`. That means the owner
> signed it out or its credentials expired; only they can fix it, and no
> amount of retrying will.
>
> **The thing you address is a conversation.** A conversation is a Google
> Messages thread: one other person, or a group. It has an ID that starts
> `conv_`. Messages in it have IDs that start `msg_`. Every ID here is an
> Agent GM ID with a typed prefix — `conv_` conversations, `msg_` messages,
> `att_` attachments, `react_` reactions, `part_` people in a thread,
> `contact_` contacts, `upl_` uploads, `op_` operations. Google's own IDs are
> not accepted in their place.
>
> Words mean what they mean in Google Messages. A *conversation* is a thread.
> A *contact* is somebody in the phone's contact list. *RCS* is the modern
> protocol; *SMS/MMS* is the fallback. *Delete* means delete from this
> account only — the other person keeps their copy, always.
>
> 1. **Find the conversation.** `list_conversations` with `participant` set
>    to a phone number (`+15105550123`, the bare digits, or a national form)
>    returns the threads that number is in, newest activity first. Add
>    `account_id` to look in one account, or leave it out to look in all of
>    them — results carry `account_id` either way. With only a name, use
>    `query`, a substring match over the thread name and every participant's
>    name and number. `sender: "me"` means the owner in whichever account a
>    message belongs to, so it works across accounts as well as within one.
>    `list_contacts` maps names to
>    numbers.
> 2. **Read it.** `list_messages` with that `conversation_id`. Newest first,
>    so the first item is the latest message. Pass the result's `next_cursor`
>    back as `cursor` for the next page; page size defaults to 50 and caps at
>    100. `search_messages` needs `q`; it searches **every** account unless
>    you pass `account_id`.
> 3. **Reply.** `send_message` with the `conversation_id`, `text`, and a
>    `client_request_id` you invent. Add `reply_to_message_id` to thread a
>    reply — **but replies are an RCS feature; on an `sms_mms` conversation
>    that argument is refused with `unsupported_capability` and
>    `reason: "reply_not_supported"`.** Check the conversation's `type` first,
>    or read `capabilities.reply`.
> 4. **Know whether it arrived.** The result carries a `message_id` and an
>    `operation`. Then watch the message's `delivery.state`, which walks
>    `sending → sent → delivered → read`. **On SMS it usually stops
>    at `sent`, and on group threads it usually stops at `sent`. Delivery and
>    read receipts are an RCS feature and a carrier feature; waiting for
>    `delivered` on an SMS thread can wait forever.** `sent` means the
>    carrier took it, and that is as much as SMS will ever tell you.
> 5. **Send a photo or a file.** `create_upload` with the filename, mime type
>    and byte length; run the `curl` command it returns, with `FILE` replaced
>    by the path; then `send_message` with `upload_ids: ["upl_…"]` and
>    optionally `text` as a caption. An upload is **not** tied to an account —
>    the conversation you send it into decides that — but it can be sent only
>    once. You cannot attach a file through this protocol any other way, and
>    base64 through the model is not acceptable.
> 6. **Start a new thread.** `start_conversation` with `recipients` as E.164
>    phone numbers. One recipient is a direct chat; two or more is a group,
>    and `name` is only accepted for a group. **If a thread with exactly
>    those recipients already exists you get that thread back and nothing is
>    sent** — starting is safe, sending is not.
> 7. **React, or take something back.** `add_reaction` with `emoji` set to a
>    bare emoji — Google offers eleven (👍 😍 😂 😮 😥 😠 👎 🤔 😢 😡 ❤️) and
>    anything else is sent as a custom reaction that may not render on the
>    recipient's phone. One reaction per person per message: adding a second
>    replaces the first. `remove_reaction` takes the same `emoji`, or the
>    `reaction_id` you read. `delete_message` and
>    `delete_conversation` delete from **this account only** — the recipient
>    keeps their copy. There is no delete-for-everyone and no mode to choose.
>
> **Every write needs a `client_request_id` that you invent.** Repeating a
> call with the same one returns the same operation and sends nothing
> further. A **fresh** `client_request_id` is a different call, not a repeat
> — reusing this to "retry" is how a person gets the same text twice.
> **Changing `account_id` while keeping the same `client_request_id` is also a
> different call**, not a retry: it would send a second real message from the
> other account. The server refuses that combination with `invalid_request`
> rather than obeying it, so if a send fails, retry it **against the same
> account** with the same key, or use a new key. If a
> send times out with `phone_not_responding`, the operation is `pending`, not
> failed: the server accepted it and the phone may still send it when it
> wakes. **Poll `get_operation`; do not resend.**
>
> The phone has to be awake and online for anything to happen.
> `list_accounts` and `get_session` tell you whether it is: `state` and
> `phone_responding`, per account. `get_health` tells
> you whether the index is complete (`backfill`) and whether the two things
> that break sending are right (`is_default_sms_app`,
> `config_version_stale`). If `state` is not `connected`, reads still work
> from the local index but writes will fail, and only the owner can fix it.
>
> A call that is refused comes back as an ordinary result with
> `isError: true` and
> `structuredContent.error = {code, message, retryable, details}`. Read it
> and correct the call rather than repeating it. `not_found` means no such
> object. `invalid_request` names the parameter you got wrong in
> `details.parameter` or `details.field` — this server refuses a misspelled
> filter rather than silently ignoring it. `unsupported_capability` means the
> action cannot apply here and `details.reason` says why.
> `phone_not_responding` and `rate_limited` are retryable; almost nothing
> else is.
>
> Scopes: `messages:read` gives you `list_accounts`, `list_conversations`,
> `get_conversation`, `list_messages`, `get_message`, `message_context`,
> `search_messages`, `get_attachment`, `list_contacts`, `get_session` and
> `get_health`. A scope covers **every** account this server holds; there is
> no per-account permission.
> `messages:write` adds `send_message`, `start_conversation`, `mark_read`,
> `add_reaction`, `remove_reaction`, `update_conversation`, `create_upload`
> and `get_operation`. `messages:delete` adds `delete_message` and
> `delete_conversation`. You only see the tools your token allows.

`docs/mcp.md` opens with the same text under the heading "First five
minutes", and a test asserts the two are byte-identical apart from the
markdown quoting, so a client that surfaces instructions to its model has
already told it this page.

### 8.4 Conformance

`devbox run conformance` builds the server, starts it on a free loopback port
with a throwaway data directory and admin secret, mints a token **through the
whole OAuth flow** (admin bootstrap → enrollment code → DCR → authorization
screen → admin approval → token endpoint), starts a small loopback proxy that
adds the token, and runs the pinned `@modelcontextprotocol/conformance`
package against the proxy. An admin bootstrap token is *not* used, because
that would leave the client-shaped path unmeasured; a failure to complete the
OAuth flow is announced loudly rather than silently downgraded.

The suite is run at the spec revision it actually knows. **A version that runs
zero scenarios is a failure, not a green line that tested nothing.** The
baseline lives in `scripts/mcp-conformance-baseline.yaml` and is checked in
both directions: a new failure fails the run, and so does a listed scenario
that starts passing, so the file cannot rot into a list of excuses.

---

## 9. OAuth 2.1

claude.ai and ChatGPT connectors require it. The design is carried over from
Agent MX with the Matrix-specific parts removed and the issuer fixed.

### 9.1 Public routes

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
`{ "error", "error_description" }` with `Cache-Control: no-store`, including
the ones the HTTP framework would otherwise answer itself: a body over 1 MiB
is `413` with that shape, a wrong method is `405` with that shape and an
`Allow` header, and an unknown path under `/oauth` is the REST `not_found`
envelope.

### 9.2 Metadata

`GET /.well-known/oauth-protected-resource` and its `/mcp` twin (RFC 9728):

```json
{
  "resource": "https://gm.agent-wx.app/mcp",
  "authorization_servers": ["https://gm.agent-wx.app"],
  "scopes_supported": ["messages:read", "messages:write", "messages:delete"],
  "bearer_methods_supported": ["header"],
  "resource_documentation": "https://github.com/thisnick/agent-gm"
}
```

`GET /.well-known/oauth-authorization-server` (RFC 8414):

```json
{
  "issuer": "https://gm.agent-wx.app",
  "authorization_endpoint": "https://gm.agent-wx.app/oauth/authorize",
  "token_endpoint": "https://gm.agent-wx.app/oauth/token",
  "registration_endpoint": "https://gm.agent-wx.app/oauth/register",
  "revocation_endpoint": "https://gm.agent-wx.app/oauth/revoke",
  "response_types_supported": ["code"],
  "grant_types_supported": ["authorization_code", "refresh_token"],
  "token_endpoint_auth_methods_supported": ["none"],
  "code_challenge_methods_supported": ["S256"],
  "scopes_supported": ["messages:read", "messages:write", "messages:delete"],
  "authorization_response_iss_parameter_supported": true
}
```

`issuer` equals `AGENT_GM_PUBLIC_URL` **byte for byte**, and `resource` is
that plus `/mcp` with no trailing-slash drift. Both are tested as string
equality, not as parsed-URL equivalence.

The `401` challenge:

```http
WWW-Authenticate: Bearer realm="agent-gm",
  resource_metadata="https://gm.agent-wx.app/.well-known/oauth-protected-resource/mcp",
  scope="messages:read messages:write"
```

### 9.3 Dynamic client registration (RFC 7591)

`POST /oauth/register`, JSON, answers `201` with a `client_id` and **no**
`client_secret`. Public native clients only:

- `token_endpoint_auth_method` must be `none`.
- Grants must be a subset of `authorization_code` and `refresh_token`.
- The only response type is `code`.
- A client-chosen `client_id` is refused.

Redirect URIs, per RFC 8252:

| Accepted | Refused |
|---|---|
| `https://` with a fully qualified host | `http://` on any non-loopback host |
| `http://127.0.0.1[:port]/…`, `http://[::1][:port]/…`, `http://localhost[:port]/…` | `http://localhost.evil.example/…` |
| a private-use scheme containing a dot | `https://` with an IP literal |
| | any URI with a fragment or embedded credentials |

At most 10 redirect URIs, each at most 500 characters. Registration is limited
to **20 per source per hour**. A registration expires **24 hours** after
creation unless an authorization activates it; a maintenance pass removes
expired unreferenced registrations every 60 seconds and audits each removal.

A registered loopback redirect **matches any port at authorization time**
(RFC 8252 §7.3), for `127.0.0.1`, `[::1]` and `localhost` alike. The token
endpoint still requires `redirect_uri` to equal the one bound to the code
exactly.

> **Deliberate deviation, recorded as contract text.** `http://localhost/…`
> is accepted, against RFC 8252 §8.3, which prefers the IP literals because
> `localhost` resolution depends on the host's name service. That hazard
> exists only on the client's own machine, and widely used MCP clients
> register the name; refusing them buys little. The allowance is for the
> literal host name and nothing else: the comparison is exact and
> case-insensitive, so `localhost.evil.example`, `notlocalhost`, `local.host`
> and `localhost@evil.example` are ordinary domains with no plain-http
> exemption. A suffix or substring match here would be far worse than the
> problem the allowance solves. `agm auth login` registers
> `http://127.0.0.1:<port>/callback`.

**Client resolution is DCR-only.** Agent MX resolved a client through three
tiers — preregistered metadata, a fetched Client ID Metadata Document, then
DCR — with an SSRF-safe outbound fetcher behind it. Agent GM keeps only DCR
(D20). There is no `AGENT_GM_OAUTH_CLIENTS`, no CIMD fetch, and therefore no
outbound HTTP from the OAuth layer at all, which removes the SSRF surface
rather than defending it. Every client this server will ever see registers
itself.

### 9.4 Authorization

`GET /oauth/authorize` takes `response_type=code`, `client_id`,
`redirect_uri`, `state`, `code_challenge`, `code_challenge_method=S256`,
`resource`, and an optional `scope` (default `messages:read messages:write`).

`code_challenge_method` must be present and `S256`: RFC 7636 defaults an
omitted method to `plain`, which is not supported.

An unknown client or an unregistered redirect URI answers `4xx` and **never
redirects** (RFC 6749 §4.1.2.1). Every later failure redirects to the verified
callback carrying `error`, `state`, and the RFC 9207 `iss`:

| Condition | `error` |
|---|---|
| `response_type` is not `code` | `unsupported_response_type` |
| missing `state`, missing or non-`S256` PKCE, malformed challenge | `invalid_request` |
| `resource` is not `https://gm.agent-wx.app/mcp` | `invalid_target` |
| unknown or empty scope, or `admin` requested | `invalid_scope` |

The page sets `agm_oauth_context`, a signed cookie with `Path=/oauth`,
`HttpOnly`, `Secure`, `SameSite=Lax`. The form carries the signed context, an
anti-CSRF `form_token`, hidden echoes of the OAuth parameters, one `scope`
checkbox per requested scope, an `enrollment_code` text input, and — **because
scopes are global across accounts (D29)** — a fixed disclosure line above the
scope checkboxes, rendered verbatim:

> *"This will let the client read and send as **any** Google account on this
> server, including accounts added later."*

followed by the current accounts' labels and addresses, so the owner approves
knowing what is in scope. That sentence is part of the screen's contract and is
asserted as a string by §16 Slice 3, which is what makes §9.7's claim to
honesty testable rather than aspirational.

`POST /oauth/authorize` verifies, **in this order**: `Origin` when present,
the cookie, the signed context, that the cookie's handle hashes to the
context, the form token, every hidden echo, that the client and redirect are
still valid, and that the selected scopes are a nonempty subset of the
requested set. **Only then is the enrollment code examined.**

- Success → `303` to `/oauth/requests/{id}`.
- Invalid code → `200` re-rendering the form with **one generic message,
  identical for unknown, expired, revoked and consumed codes**. No pending
  request is created and nothing is consumed.
- Scopes above the code's ceiling → `200` re-rendering, showing the allowed
  access, code still redeemable.
- Eleventh failed code attempt within 15 minutes, per signed context or per
  source → `429` with `Retry-After`. The per-source bucket is checked before
  the submission is examined, so loading a fresh authorization page does not
  reset it.

### 9.5 Enrollment codes and owner approval

There is no self-service. A connector cannot get a token unless the owner does
two separate things: issue a code, and approve the request.

`POST /v1/admin/enrollment-codes` with `{"label", "expires_in"?, "scopes"?,
"allow_scopes"?}` answers `200` (not `201`); `data.code` is the **only** time
the value is returned, and only its SHA-256 is stored. `expires_in` is a
duration string or seconds, defaulting to `settings.oauth.enrollment_default_ttl`
(15m, bounds 1m–24h), capped at 24 hours. `scopes` replaces the default
ceiling `messages:read messages:write`; `allow_scopes` extends it; they are
mutually exclusive. **`admin` can never be enrolled.** Enrollment codes carry
**no account dimension**, consistent with D29: a code caps which scopes may be
granted, never which accounts they reach.

`GET`/`DELETE /v1/admin/enrollment-codes[/{id}]` list, show and revoke.
Revocation takes its reason as the query parameter `?reason=`, and repeating
it answers `200` with `revoked: false`.

The waiting page polls `GET /oauth/requests/{id}/status`, which requires the
context cookie and answers
`{"request_id","status","expires_in_seconds","poll_interval_seconds"}` with
`no-store`. `poll_interval_seconds` is the server's decision; the page clamps
it to 1–60 seconds. **Without a valid cookie every `/oauth/requests` route
answers `404`: the request ID alone conveys no authority.** The polling script
is served from `/oauth/poll.js` rather than inlined, so `script-src` stays
`'self'`.

The owner approves with
`POST /v1/admin/authorization-requests/{id}/approve` (optional `scopes`, which
may only **narrow** the browser-selected set; widening or empty is
`invalid_request`; a no-longer-pending request is `idempotency_conflict`; an
expired one is `invalid_request`), or denies with `.../deny` and an optional
`reason`.

`POST /oauth/requests/{id}/complete` requires the cookie, the waiting page's
form token, and a same-origin `Origin`. An approved, unexpired request answers
`303` to the exact registered callback with `code`, `state`, `iss` and the
granted `scope`, and clears the cookie. A **denied** request redirects with
`error=access_denied`, because a denial is a decision the client is entitled
to hear. Expired, completed or still-pending answers `409`, and a completed
request can never mint a second code.
`GET /oauth/requests/{id}` answers `200` for every state including expiry — it
renders a page rather than performing an operation.

### 9.6 Tokens

`/oauth/token` and `/oauth/revoke` are `application/x-www-form-urlencoded`
with `Cache-Control: no-store`.

`grant_type=authorization_code` takes `code`, `redirect_uri`, `client_id`,
`code_verifier`, and an optional `resource` that must equal the canonical
resource. `resource` is **mandatory at `/oauth/authorize` and optional here**:
this server has exactly one audience, so an omitted value is read as the
canonical resource and a present one must match it (RFC 8707 §2.2 permits this
for a single-audience server, and the binding that matters was fixed at
authorization time).

- A **replayed code** is `invalid_grant` and **revokes the tokens the first
  exchange produced**.
- A PKCE verifier that does not hash to the bound challenge is `invalid_grant`
  and **consumes the code**, so a verifier cannot be guessed by retrying.

`grant_type=refresh_token` takes `refresh_token`, `client_id`, and an optional
`scope` that may only narrow; widening is `invalid_scope` and **does not spend
the presented token**. Refresh tokens rotate on every use, and reuse of a
spent token revokes the whole family and its authorization. A rotation, its
audit record, and the family revocation that reuse triggers all commit in one
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
| `admin.access_token_ttl` | 15m | 5m–1h |
| `admin.refresh_token_idle_ttl` | 30d | 1d–90d |
| `admin.refresh_token_absolute_ttl` | 90d | 7d–365d |
| `oauth.authorization_code_ttl` | 2m | 30s–5m |
| `oauth.authorization_request_ttl` | 15m | 1m–1h |
| `oauth.enrollment_default_ttl` | 15m | 1m–24h |

Access tokens are bound to `https://gm.agent-wx.app/mcp`. **If
`AGENT_GM_PUBLIC_URL` ever changes, every token minted under the previous
origin is refused with `401 invalid_token` on both `/mcp` and `/v1`, and every
registered client is orphaned.** See §15.6.

The admin bootstrap (`POST /v1/auth/admin-session`, exchanging
`AGENT_GM_ADMIN_SECRET`) is a separate credential path and never crosses
endpoints: an OAuth refresh token at `/v1/auth/refresh` is `invalid_token`,
and an admin refresh token at `/oauth/token` is `invalid_grant`. Neither
attempt revokes anything.

**Admin sessions expire on the same schedule as OAuth ones**, under their own
settings (`admin.access_token_ttl`, `admin.refresh_token_idle_ttl`,
`admin.refresh_token_absolute_ttl`, §15.1, defaulting to the OAuth values).
The credential that outranks everything else does not get a longer life than
the ones it outranks. Admin refresh tokens rotate on every use and reuse of a
spent one revokes the session, exactly as for OAuth (§9.6).

**An admin refresh may never widen.** `POST /v1/auth/refresh` accepts an
optional `scopes` that may only narrow further, relative to **the scopes the
session was minted with**, which are recorded on the session row. Widening —
including back to the full set after a narrowing — is `invalid_scope` and
**does not spend the presented token**. Re-widening requires presenting
`AGENT_GM_ADMIN_SECRET` again. Without this rule a narrowed session is one
refresh away from full privilege, and the scope-refusal tests of §16 Slice 2
would prove nothing.

### 9.7 Scopes

| Scope | Grants |
|---|---|
| `messages:read` | every read route and read tool; `whoami`; `logout`; download-ticket redemption |
| `messages:write` | send, start, mark read, reactions, uploads; `get_operation`; upload-ticket redemption |
| `messages:delete` | `delete_message` and `delete_conversation`, and nothing else |
| `admin` | `/v1/admin/*` and all pairing routes. Issued only by the admin bootstrap; **never enrollable**, and `invalid_scope` at `/oauth/authorize` |

**The admin bootstrap session carries `admin` *plus all three messaging
scopes*.** `POST /v1/auth/admin-session` mints
`admin messages:read messages:write messages:delete`, because the owner
presenting `AGENT_GM_ADMIN_SECRET` is by definition the person the whole
service belongs to, and a credential that could administer the server but not
read a message would be useless. Two consequences:

- Slice 2 is executable. Its own acceptance tests call messaging routes with
  the only credential that slice has (§16).
- Scope *refusal* is still testable in Slice 2: `POST /v1/auth/admin-session`
  accepts an optional `{"scopes": [...]}` narrowing the session to a subset,
  which is how the exit-code matrix produces exits 3 and 4 and how the
  rate-limit test proves three scopes do not multiply the allowance. Widening
  beyond the four is `invalid_scope`.

`admin` is still never enrollable and still `invalid_scope` at
`/oauth/authorize`: the *only* way to hold it is the admin secret.

A `messages:write` token that lacks `messages:delete` neither sees the two
delete tools in `tools/list` nor may call them.

**Scopes are global across accounts** (D29). A token holding `messages:read`
reads every account this server holds; one holding `messages:write` can send
from any of them. Per-account scoping — `messages:write:acct_…` — is
**deferred, not refused**: it needs a scope grammar, a UI for choosing accounts
at the authorization screen, and enrollment ceilings that can name accounts
that may not exist yet. Until then the honest statement is the one the
authorization screen renders verbatim above its scope checkboxes (§9.4),
naming the accounts currently in scope and saying that later ones are
included too. An owner who needs a genuinely separated
account runs a second Agent GM.

### 9.8 Budgets on unauthenticated endpoints

`/oauth/revoke`, the `refresh_token` grant, and **`POST /v1/auth/refresh`**
accept a bearer value from an unauthenticated caller: the presented token *is*
the credential, so an attacker can guess at all three in the same shape. All
three carry **60 requests/minute per source, burst 20**
(in memory; a restart forgives an anonymous caller's request debt, which costs
nothing) plus a **durable limit of 30 unknown or invalid presented tokens per
15 minutes**, with a cooldown that doubles per further failure in the window
and caps at 24 hours, held in `oauth_attempts` because that is the limit an
attacker would restart-cycle to reset. Both are checked **before** the
presented token is examined, so a `429` never distinguishes a real token from
a guess, and it carries `Retry-After`. A successful presentation does not
clear the failure counter.

The presented token is looked up read-only **before any write transaction
opens**, so a caller presenting a value that belongs to nobody cannot take the
single writer's lock and queue every other writer behind itself. When the
token does exist, the transaction re-reads the row before cascading, so
revocation stays a single atomic decision.

`POST /v1/auth/admin-session` is budgeted separately and more tightly, under
§12.3's admin-secret failure limit (5 per 15 minutes per source, 20 globally,
exponential cooldown), because a success there yields more than a success
anywhere else.

### 9.9 Browser security headers

OAuth pages carry `Content-Security-Policy` with `default-src 'none'`,
`frame-ancestors 'none'`, `base-uri 'none'`, `form-action 'self'`;
`Cache-Control: no-store`; `Referrer-Policy: no-referrer`;
`X-Content-Type-Options: nosniff`; `X-Frame-Options: DENY`.

---

## 10. Media

An MCP client cannot attach a file, and base64 through the model is not
acceptable. Bytes move by `curl`, with a ticket.

### 10.1 Download

`GET /v1/attachments/{attachment_id}` returns metadata **and a download
ticket**:

```json
{ "attachment_id": "att_9f3c...",   // UUIDv5, 4.1
  "account_id": "acct_...",
  "filename": "IMG_0421.jpg", "mime_type": "image/jpeg",
  "size": 184320, "sha256": "…", "sha256_available": true,
  "width": 1024, "height": 768, "download_state": "available",
  "inline": false,
  "resource_uri": "agm://attachments/att_9f3c...",
  "download_url": "https://gm.agent-wx.app/v1/attachments/att_9f3c.../content",
  "token": "agm_dt_…", "token_audience": "download:att_9f3c...",
  "expires_at": "2026-09-06T10:11:07.000Z", "max_redemptions": 5,
  "curl": "curl --fail -H 'Authorization: Bearer agm_dt_…' -o 'IMG_0421.jpg' 'https://gm.agent-wx.app/v1/attachments/att_9f3c.../content'" }
```

`GET /v1/attachments/{id}/content` accepts either a `messages:read` access
token or a download ticket. It serves the decrypted bytes with
`Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`, and a
sandboxing CSP. **SVG, HTML and every other unrecognised type are served as
`application/octet-stream`, never with their own type.** Bounded at 8
concurrent downloads per authorization and 32 globally.

The bytes come from the media cache if present, else from
`libgm.DownloadMedia(mediaID, decryptionKey)`, which is then cached. An
attachment whose `media_id` is empty but whose `thumbnail_media_id` is set
triggers `GetFullSizeImage` first; while that is outstanding the state is
`pending` and the route answers `unsupported_capability` with
`reason: "media_pending"`.

`sha256` is the digest of the **decrypted** bytes, computed on demand and
cached, because Google carries a digest of the ciphertext, which is not what
an agent comparing its downloaded copy would compute. When it cannot be
computed, `sha256` is `null`, `sha256_available` is `false`, and
`sha256_unavailable_reason` is one of `larger_than_cache_budget`,
`download_budget_exhausted`, `bytes_unavailable`; the reason also appears in
`warnings` as `sha256_unavailable:<reason>`.

### 10.2 Upload — three requests

1. **`POST /v1/uploads`** reserves: `filename`, `mime_type`, `size_bytes`
   (required), optional `sha256`, `client_request_id`. Answers `201`:

```json
{ "upload_id": "upl_01k4...",
  "upload_url": "https://gm.agent-wx.app/v1/uploads/upl_01k4.../content",
  "method": "PUT",
  "token": "agm_ut_…", "token_audience": "upload:upl_01k4...",
  "expires_at": "2026-09-06T12:11:07.000Z",
  "limits": { "max_bytes": 104857600, "size_bytes": 184320,
              "mime_type": "image/jpeg", "sha256": "…",
              "expires_in_seconds": 7200 },
  "curl": "curl --fail -X PUT -H 'Authorization: Bearer agm_ut_…' -H 'Content-Type: image/jpeg' --data-binary @FILE 'https://gm.agent-wx.app/v1/uploads/upl_01k4.../content'" }
```

2. **`PUT {upload_url}`** with `Authorization: Bearer <token>` streams the
   bytes. The token works **once**, for **that upload only**, and its
   redemption re-checks the issuing authorization's `messages:write` scope and
   revocation state — a token outlives neither. The stream is refused the
   moment it exceeds the reserved length. Completion verifies the byte count,
   the declared digest, and the detected content type. **A reservation that
   fails verification is spent: reserve again rather than retry.**

3. **`send_message` with `upload_ids: ["upl_…"]`.** The handler reads the
   staged bytes, calls `libgm.UploadMedia(data, filename, mime)` (which
   generates its own AES-GCM key and does the resumable upload to Google), and
   puts the returned `MediaContent` into the `SendMessageRequest`. Only the
   authorization that owns an upload may send it, and an upload can be sent
   once. Naming an upload twice is refused; naming another authorization's is
   `not_found`.

**Uploads are deliberately account-agnostic.** An `upl_` reservation carries no
`account_id`: it is bytes staged by an authorization, and **the account is
fixed at send time** by the conversation named in `send_message`. So the same
upload can be sent into any account the caller may write to — though only
**once**, so it cannot be fanned out — and §7.3's "every multi-account DTO
carries `account_id`" does not apply to it, because an upload never appears in
a multi-account *result*. Under D29 there is nothing account-shaped for a
ticket redemption to re-check anyway: scopes are global. Slice 2 asserts both
halves — an upload reserved and sent into a second account succeeds, and a
second send of it is refused.

`mime_type` is validated against `libgm.MimeToMediaType` at reservation time,
with the same type-prefix fallback upstream uses; an unsupported type is
`media_unsupported_type` (415) and reserves nothing. **One attachment per
message.**

### 10.3 Ticket rules

Tokens are typed to one audience — `upload:<upload_id>` or
`download:<attachment_id>` — and the audience is part of the redemption
rather than a check after it. **A token presented at the wrong URL is refused
and not spent**, so learning a token value does not let anyone destroy it.
Every refusal is the same message whatever the reason, so a status code
teaches an attacker nothing about which guesses were once valid.

The token appears **in the body and never in a URL**, so it cannot be captured
from a proxy log or a browser history, and it is accepted only in the
`Authorization` header — there is no `?t=` form.

**Every redemption re-checks the issuing authorization's scope and revocation
state** — `messages:write` for an upload, `messages:read` for a download. A
ticket outlives neither, so revoking an authorization immediately kills every
ticket it minted.

**Download tickets are stateful.** A five-use cap cannot be enforced by a
signed blob, so the ticket is a row in `download_tickets` (§4.2) keyed by the
token hash, holding `redemptions`, `max_redemptions` and `expires_at_ms`; the
counter is incremented in the same transaction that authorises the read.
Upload tickets are rows in `uploads` for the same reason. §12.1's "signed, not
stored" applies to the *value*: the token is signed with the ticket key and
only its hash is stored, exactly like an OAuth token.

| Thing | Value |
|---|---|
| upload reservation and token life | 2 hours |
| upload token redemptions | 1 |
| download token life | 15 minutes |
| download token redemptions | 5, counted in `download_tickets.redemptions` |
| `media.upload_max_bytes` | 100 MiB (104857600), runtime-mutable downward |
| `media.cache_max_bytes` | 2 GiB, LRU eviction among unpinned entries. **Global across accounts, not per account** — one account's backfill can evict another's cached media. The cache is a cache; the bytes are always re-fetchable from Google |
| `media.inline_mcp_image_max_bytes` | 1 MiB |
| sweep of expired reservations and tickets | at startup and every 5 minutes |
| token prefixes | `agm_ut_` upload, `agm_dt_` download |

`upload_url` and `download_url` are built from `AGENT_GM_PUBLIC_URL` and
**never** from the request's `Host`, so an agent in a sandbox on another
machine reaches the same origin the tunnel exposes.

`client_request_id` makes the reservation idempotent; each attempt returns a
fresh token, because the first token's value left the process and cannot be
recovered, and a token minted on a repeat never outlives the reservation it
fills — which is what `limits.expires_in_seconds` counts down to.

**Erasure order.** Staged bytes and cache objects are removed by collecting
the relative paths inside the delete transaction, committing, and only then
unlinking. Deleting the rows without the files would be worse than either: the
eviction sweep finds every file it deletes through those rows, so an orphaned
file is never reclaimed and the cache budget silently stops matching the disk.
A crash between the commit and the unlink leaves a file nothing references,
which an operator can delete.

---

## 11. CLI

`agm`, one binary, speaks the REST API. It never touches SQLite or `libgm`
directly, so anything the CLI can do an agent can do too, and vice versa.

### 11.1 Global flags

```text
--server <url>        Override the configured server.
--profile <name>      Select a saved server and authorization profile.
--json                Emit one stable JSON value on stdout.
--output <format>     table (default) | json | jsonl
--timeout <duration>  Bound the request, and any operation wait.
--quiet               Suppress non-result output.
--verbose             Diagnostic detail on stderr.
--idempotency-key     Resume one logical state-changing request.
--yes                 Skip the confirmation prompt on a destructive command.
```

Durations are integer + unit, `s|m|h|d`: `30s`, `15m`, `2h`, `7d`. Anything
else is exit 2 naming the flag.
### 11.2 Exit codes

```text
0   success
2   CLI usage or validation error
3   authentication required or expired credentials
4   authorization or insufficient scope
5   requested resource absent
6   unsupported capability for this account, conversation or message
7   retryable network, Google, phone, or rate-limit failure
8   an operation reached a terminal failure
9   local configuration or credential-store failure
10  server contract or internal failure
```

`1` is deliberately unassigned. Every code in §7.2 maps onto exactly one exit
code, and the mapping is exhaustive — §16 Slice 2 test 18 enumerates it:

| §7.2 code | exit |
|---|---|
| `invalid_request`, `idempotency_conflict`, `payload_too_large`, `media_unsupported_type` | `2` |
| `invalid_token` | `3` |
| `insufficient_scope` | `4` |
| `not_found` | `5` |
| `unsupported_capability`, including `not_signed_in` — an **account**-level condition, which is why the gloss above names the account | `6` |
| `rate_limited`, `disconnected`, `phone_not_responding`, `google_http_error`, `pairing_init_timeout` | `7` |
| an operation reaching `failed` or `unknown` while waiting | `8` |
| `not_paired`, every other `pairing_*`, `not_default_sms_app`, `config_version_stale`, `google_error`, `google_undocumented_status`, `google_permission_denied` | `10` |
| `internal_error` | `10` |

`idempotency_conflict` is exit `2` because it is a caller mistake: the same key
was reused with a different body. A command that times out waiting returns `7`,
prints the operation ID, and leaves it available to `agm operations wait`.
`phone_not_responding` is exit `7`, **not** `8`, and the message says the
operation is pending and must not be resent.

### 11.3 Commands

```text
agm pair [--account <acct-id>] [--device-index N] [--timeout 5m]
                                               # adds an account, or resumes one
agm pair --refresh-cookies --account <acct-id> # re-auth without re-pairing
agm pair --paste [--paste-file <path>]         # fallback: curl / JSON paste
                                               # combines with --refresh-cookies

agm accounts list [--all]
agm accounts show <acct-id>
agm accounts label <acct-id> <label>
agm accounts sign-out <acct-id> [--yes]        # keeps everything, refuses writes
agm accounts remove <acct-id> [--yes]          # the ONLY thing that deletes data

agm session [--account <acct-id>] [--watch]
agm reconnect --account <acct-id>
agm health

agm conversations list [--account|--query|--participant|--folder|--type
                        |--unread|--group|--include-deleted|--all]
agm conversations show <conv-id>
agm conversations start <e164>... [--account <acct-id>] [--name <name>]
agm conversations archive|unarchive|pin|unpin|mark-unread <conv-id>
agm conversations mark-read <conv-id> --message <msg-id>
agm conversations delete <conv-id> [--yes]
agm conversations typing <conv-id>

agm messages list [<conv-id>] [--account|--direction|--sender|--after|--before
                   |--has-attachment|--delivery-state|--include-system|--all]
agm messages show <msg-id>
agm messages context <msg-id> [--before 5] [--after 5]
agm messages search <query> [--account] [--mode words|exact]
                            [--conversation <conv-id>]
                            [--sender|--after|--before|--has-attachment]
agm messages send <conv-id> [--text <t>] [--file <path>] [--reply-to <msg-id>]
                            [--force-rcs]
                            [--wait] [--wait-for sent|delivered|read|terminal]
agm messages delete <msg-id> [--yes]
agm messages add-reaction <msg-id> <emoji>
agm messages remove-reaction <msg-id> <emoji> | --reaction <react-id>

agm attachments list <msg-id>
agm attachments show <att-id>
agm attachments download <att-id> [--output <path>]

agm contacts list [--account] [--query <q>] [--top]

agm operations list [--account] [--kind|--status|--terminal|--after|--before|--all]
agm operations show <op-id>
agm operations wait <op-id> [--for sent|delivered|read|terminal] [--timeout 60s]

agm auth login [--admin] [--server <url>] [--scopes ...] [--no-browser]
               [--secret-stdin]
agm auth logout
agm auth whoami

agm admin settings list|get|set
agm admin enrollment-codes create|list|show|revoke
agm admin authorization-requests list|show|approve|deny
agm admin authorizations list|show|revoke
agm admin clients list|show|revoke
agm admin audit list [--account <acct-id>] [--kind|--kind-prefix|--after|--before]
agm admin backfill [--account <acct-id>] [--conversation <conv-id>] [--yes]
agm admin backup
agm admin diagnostics

agm completion bash|zsh|fish
agm version
```

**Every `/v1` route parameter has a flag, and every `/v1` route has a
command**, with one stated exception: `GET`/`DELETE /v1/uploads/{id}` have no
command, for the same reason they have no MCP tool (§8.2) — `agm messages send --file` performs the whole reserve /
`PUT` / send sequence in one process and never surfaces an `upl_` ID for a
human to inspect or cancel. `agm attachments list <msg-id>` is
`GET /v1/messages/{id}/attachments`; MCP folds that data into `get_message`
(§8.2) but the CLI keeps the command, because the rule above is "every `/v1`
route has a command" and an owner asking what a message carried should not
have to read a whole message DTO. `agm messages list` with no
conversation ID is `GET /v1/messages` across every account, or one account
with `--account`. `agm pair` covers `POST`/`GET`/`DELETE /v1/pairing/*` —
abandoning is Ctrl-C, which issues the `DELETE`. `agm reconnect` is
`POST /v1/accounts/{id}/reconnect`. `agm session --watch` consumes
`GET /v1/accounts/{id}/events`, or the all-accounts form when `--account` is
omitted. `agm operations wait` polls
`GET /v1/operations/{id}` on the client side and takes its default bound from
the server's `operations.wait_timeout` setting, reported by
`GET /v1/admin/settings`.

`agm messages send --file <path>` does the whole media dance for the owner:
reserve, `PUT`, send. An agent does the three steps itself (§10.2), because it
has no filesystem the server can reach. `--file` takes **one** path, matching
the one-attachment-per-message limit of §7.6.

Names are the same words on all three surfaces: `mark-read` is `mark_read` is
`POST …/read`; `add-reaction`/`remove-reaction` are `add_reaction`/
`remove_reaction`. There is no `unreact` and no `read` subcommand that could be
misread as "read the conversation".

`--wait` defaults to `--wait-for sent`. `terminal` means an operation status of
`succeeded`, `failed` or `unknown`, **or** a message `delivery_state` of
`delivered`, `read`, `failed`, `canceled` or `deleted`. `delivered` and `read`
are message states, not operation states, and `--wait-for delivered` continues
to wait after the operation is terminal — that is deliberate, and
`--wait-for terminal` is the flag that means "stop as soon as anything is
settled". **`agm` warns on stderr when `--wait-for delivered` or `read` is
used on an `sms_mms` conversation**, for the reason in §5.5.

Destructive commands — `accounts remove` (the only one that deletes an
account's history), `accounts sign-out`, `conversations delete`,
`messages delete`, `auth logout`, `admin ... revoke`,
and `admin backfill` with neither `--account` nor `--conversation` — print the
**exact
`effect` sentence returned by the route** (§7.6) and require an interactive
`y`, or `--yes`. The `effect` field, the MCP tool description's closing
sentence, and this prompt are the same words.

Human output prints the operation ID first for every mutation. `--json` writes
the server envelope plus CLI metadata (effective profile and server) as one
value on stdout; diagnostics, progress, warnings and confirmation prompts go to
stderr, so stdout stays machine-readable.

### 11.4 Pairing UX

**`agm pair` is the Google-account flow, driven through a dedicated Chrome
profile** (D19). It is one command from start to paired, and it is the only
flow. The one fallback, which the CLI offers itself when it cannot find
Chrome, is `--paste`.

#### `agm pair` — the default: dedicated Chrome over CDP

The gaia flow needs seven cookies, six of them required, one of them (`OSID`)
host-scoped to `messages.google.com` (§3.2). **Five of the seven are
`httpOnly`** — only `APISID` and `SAPISID` are readable from JavaScript — so no
injected script and no `document.cookie` read can produce a usable set. The
only honest capture is the browser's own cookie store, read through the
browser's own debugging protocol.

```console
$ agm pair
Adding a Google account to Agent GM.
Opening a dedicated Chrome window for Google sign-in.
This profile belongs to agm alone; your normal Chrome is untouched.

  Sign in to your Google account in that window, and wait for
  Google Messages for web to load.

  [Chrome opens at accounts.google.com/AccountChooser?continue=
   https://messages.google.com/web/config]

  Captured 7 cookies.  Closing Chrome.

  Your phone will show three emoji and ask which one matches.
  Tap this one:

        🦋

  Waiting for the phone...
```

What the CLI does, precisely:

1. Launches the **system Chrome** with a **freshly created, short-lived**
   `--user-data-dir` under the platform's temporary directory and
   `--remote-debugging-port=<random>` bound to `127.0.0.1`.
   **The dedicated `--user-data-dir` is mandatory, not stylistic**: current
   Chrome refuses `--remote-debugging-port` against the default profile
   directory. No other flag is passed — no `--enable-automation`, no
   `--headless` — so `navigator.webdriver` is unset, there is no automation
   infobar, and Google's sign-in sees an ordinary Chrome.
2. Navigates to
   `https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config`
   (upstream's own capture URL, `connector/login.go:229`).
3. Waits for the user to finish signing in, and then **waits for `OSID` to
   exist** before reading. `OSID` is not set by the `accounts.google.com`
   sign-in alone; it appears once `messages.google.com` has been loaded.
   Reading before it exists silently returns an unusable set.
4. Reads, over CDP, **exactly** `SID`, `HSID`, `OSID`, `SSID`, `APISID`,
   `SAPISID` and `__Secure-1PSIDTS` — and nothing else — using
   `Storage.getCookies`/`Network.getCookies`, which return `httpOnly` cookies.
   **The read must cover `https://messages.google.com` as well as
   `https://www.google.com`**, because `OSID` is host-scoped to the former;
   a read of `.google.com` alone is the most common way this fails.
   `__Secure-1PSIDTS` is read last, because it rotates.
5. **Terminates Chrome immediately and deletes the profile directory**, then
   starts `DoGaiaPairing` and prints the emoji. The deletion happens on every
   path out of the capture — success, a Chrome that died on launch, the
   timeout, and Ctrl-C — so a directory holding a logged-in Google session
   never outlives the command that made it (D33).
6. Never writes the cookies to a file of its own. They go from CDP into the
   request body and nowhere else.

Three things the owner is told, once, at this point:

- **A brand-new empty Chrome profile is a new device to Google** and may
  trigger 2FA or a device-verification challenge. That is a one-time cost, not
  a failure, and the CLI waits rather than aborting.
- **The debugging port is a live credential channel.** Anything that can
  connect to it reads every cookie in that profile. It is bound to loopback on
  a random port and Chrome is killed the moment the capture completes.
- **The profile is short-lived.** It is created fresh for this capture under
  the platform's temporary directory at mode `0700`, and deleted the moment
  Chrome closes — including on failure, on timeout and on Ctrl-C. Nothing is
  written under `$XDG_STATE_HOME/agent-gm/`, and a directory holding a
  logged-in Google session is never left on the machine. There is therefore
  no `--forget-browser`: there is nothing kept to forget. The cost is that
  every capture is a new device to Google and every `--refresh-cookies` asks
  for a sign-in again — Google may or may not also ask for the password —
  which the owner is told in the message the command prints (D33).
- **Chrome's own failure is surfaced, not swallowed.** Chrome's stderr is
  kept and, if the browser exits before the debugging port answers, it is the
  error the CLI prints, naming the exit. When `DISPLAY` and `WAYLAND_DISPLAY`
  are both unset, or `DISPLAY` is set with no `XAUTHORITY` and no
  `~/.Xauthority`, the CLI adds the desktop-session hint: this is the common
  Linux case, where a terminal that is not part of the graphical session
  cannot open a window and Chrome says only "cannot open display".

**Reading the user's existing Chrome profile is explicitly not done.** On Linux
the cookie DB key lives in the login keyring, on Windows Chrome has used
app-bound encryption since version 127, and the database is locked while Chrome
runs. It is platform-forked and brittle for no gain over a dedicated profile.

#### When there is no Chrome

If no Chrome or Chromium binary is found — `$AGENT_GM_CHROME`, then the
platform's usual locations, then `PATH` — `agm pair` **does not fail with a
missing-binary error**. It explains what it needs and offers the three
alternatives, in this order:

```console
$ agm pair
Agent GM could not find Chrome or Chromium on this machine.

The default pairing flow signs in to your Google account in a dedicated
Chrome profile and reads the seven session cookies Google Messages for web
uses. Five of them are httpOnly, so only a real browser can produce them.

Two ways forward, best first:

  1. Run this same command from a machine that has Chrome, pointing at this
     server -- only the cookies travel, over TLS:

         agm pair --server https://gm.agent-wx.app

  2. Paste them yourself. In a browser already signed in to
     messages.google.com, open devtools -> Network, right-click any request
     -> Copy as cURL, then:

         agm pair --paste                 # reads the paste from stdin
         agm pair --paste-file ./curl.txt

Set AGENT_GM_CHROME to a binary path if Chrome is somewhere unusual.
```

The exit code is `9` (local configuration) if the owner takes neither, not
`2`: nothing about the command was malformed.

**`agm pair --paste` is the documented fallback**, for a machine with no
Chrome at all. It reads from stdin, or from `--paste-file <path>`, and accepts
either a JSON object of cookie name → value or a **cURL command copied from
browser devtools** — matching upstream's own instruction text, *"Enter a JSON
object with your cookies, or a cURL command copied from browser devtools"*
(`connector/login.go:227`). The input is parsed once, never echoed, and never
copied to disk. If the paste is missing a required cookie the CLI names the
missing ones and the domain each comes from (§3.2), because a paste scoped to
`.google.com` alone silently omits `OSID`.

#### Headless servers

```console
$ agm pair --server https://gm.agent-wx.app
```

Chrome runs on the owner's laptop; the CLI POSTs **only the seven cookies** to
`POST /v1/pairing/start` over TLS. Stated plainly, because it is the trust
decision being made:

> **The server then holds live Google account cookies for the whole life of the
> pairing** (§3.2), inside `sessions/<acct>.enc`. Not "saw them once" — *holds
> them*,
> and rotates them in place as Google reissues them. Anyone with the data
> directory **and** `AGENT_GM_DATA_KEY` has the owner's Google account. That is
> why session files are mode `0600` in a `0700` directory, why §12.2 redacts
> cookies from every log
> and audit payload, why §15.2 treats a backup of it as equivalent to a
> credential, and why §15.2 treats a backup of it accordingly.

#### `agm pair --refresh-cookies --account <acct-id>`

`--refresh-cookies` says *what to do with a capture*; `--paste` says *where
the capture comes from*. They combine: `agm pair --refresh-cookies --account
<id> --paste` re-authenticates an existing pairing from a paste, which is the
only way to refresh cookies on a machine with no Chrome.

When cookies expire the session goes `signed_out` and writes fail, but **the
pairing survives** (§3.2). This re-runs the capture in a **fresh, short-lived**
Chrome profile and re-authenticates the existing pairing. Because nothing is
kept between captures (D33), it asks for a Google sign-in again; Google may or
may not also ask for the password, and the command says so before it opens the
window. A capture from a different Google account is refused with
`pairing_wrong_account` and changes nothing.

#### Several Android devices on one Google account

The library picks the most-recently-seen (§3.2). If that is the wrong phone,
`agm pair --device-index 1` selects the next one. `agm pair` prints the
`dest_reg_uuid` of the device it chose, so a second run can be compared
against the first and the audit row names the same value.

> **It does not print when that device was last seen, and there is no
> device count.** Both live only inside `StartGaiaPairing`, which
> `DoGaiaPairing` — the call §3.1 mandates — does not surface; the last-seen
> timestamp reaches nothing but an upstream log line
> (`pair_google.go:352-370`). D31 records the choice.

#### After either flow

```console
Paired.  account: acct_7f2a...  (alex@example.com)  phone: <phone_id>
Backfilling 41 conversations... done (2,183 messages).

This is account 2 of 2. Writes need --account when there is more than one;
`agm accounts list` shows them and `agm accounts label` gives them names.

Two things to check on the phone, once:
  * Google Messages must be your default SMS app, or sends will fail.
    `agm health` reports this as is_default_sms_app.
  * For group messages, Settings -> Advanced -> Group messaging should be
    "Send an MMS reply to all recipients". With the SMS setting, a group
    send may fan out as separate SMS threads instead.

Using Google Messages for web in a browser at the same time is fine; it
causes extra resyncs, not a lost pairing.
```

Both phone settings are repeated in `docs/pairing.md`.

### 11.5 Credentials

Tokens are stored per server in `$XDG_STATE_HOME/agent-gm/credentials.json`
(`~/.local/state/agent-gm/credentials.json`), mode `0600` inside a `0700`
directory, written atomically under a `credentials.lock` advisory lock.
Override with `--credentials-file` or `AGENT_GM_CREDENTIALS_FILE`.

Precedence:

| Source | Use |
|---|---|
| `--server`, else `AGENT_GM_URL`, else the profile's server | which server a command talks to |
| `AGENT_GM_ACCESS_TOKEN` | one access token, used as given, not refreshable |
| `AGENT_GM_REFRESH_TOKEN_FILE` + `AGENT_GM_CLIENT_ID` | automation. Every invocation exchanges the token and rewrites the file with the rotated value, atomically, at mode `0600` |
| the stored profile | the ordinary case, written by `agm auth login` |

**Write-back safety.** The destination is proved writable *before* the token is
spent — the temporary file that the write later renames into place is created
first. An unwritable directory is **exit 9 while the token is still good**. A
token the server has already refused is **exit 3** (log in again), never
retried.

`agm auth login` performs the same OAuth flow as any other MCP client:
discovery, dynamic registration, a loopback callback on `127.0.0.1`, PKCE
`S256`, and verification of `state` and the RFC 9207 `iss` before the code is
exchanged. A callback whose `iss` does not match is refused, **and so is one
carrying no `iss` at all**, because the server's metadata advertises
`authorization_response_iss_parameter_supported` (RFC 9207 §2.4).
`--no-browser` prints the URL. `--scopes` requests a specific set; `admin` is
refused there and is issued only by `agm auth login --admin`, which exchanges
`AGENT_GM_ADMIN_SECRET` over `--secret-stdin` or a TTY prompt.

Each profile is bound to an exact server issuer and resource. Changing
`--server` selects credentials for that server; it never forwards one
profile's token to a new origin.

---

## 12. Security

### 12.1 Secrets

| Secret | Where it lives | Rotatable |
|---|---|---|
| `AGENT_GM_ADMIN_SECRET` | environment only. ≥43 characters; the server refuses shorter | yes — changing it revokes every previous admin bootstrap authorization on next start |
| `AGENT_GM_DATA_KEY` | environment only. 256 bits as 64 hex or base64 | **no.** §4.5 |
| Google session (`AuthData`), one per account | `sessions/<acct>.enc`, sealed with the single data key, mode `0600` in a `0700` directory. The account ID is AEAD associated data, so files cannot be swapped between accounts | by re-pairing, per account |
| **Google account cookies**, one set per account | inside `AuthData`, therefore inside `sessions/<acct>.enc`, **for the whole life of that pairing** (§3.2). Never on disk unencrypted, never logged | by `agm pair --refresh-cookies` |
| Short-lived Chrome profile (the default flow) | a fresh temporary directory on the **client** machine, mode `0700`, for the length of one capture only. It contains a logged-in Google session while Chrome is open and is deleted when Chrome closes, on every path (D33) | n/a — it does not outlive the command |
| OAuth tokens | SQLite, **hashes only** | rotate on use |
| Enrollment codes | SQLite, **SHA-256 only**; the value is returned once, at creation | revoke and reissue |
| Media tickets | signed, not stored as values | expire |

Generate both with `devbox run gen-secret` (`openssl rand -hex 32`).

**Every secret comparison is constant-time.** Any check of a caller-supplied
value against a stored one — `AGENT_GM_ADMIN_SECRET`, an enrollment code, an
access or refresh token, an upload or download ticket, an OAuth authorization
code, a PKCE verifier's derived challenge, a signed cursor's MAC, an OAuth
form token or context cookie MAC — is performed with
`crypto/subtle.ConstantTimeCompare` (or `hmac.Equal`, which wraps it) over
fixed-length hashes, **never with `==` on strings or `bytes.Equal`**. Values are
hashed before comparison so the compared lengths are constant and a length
difference cannot leak. This is the highest-value check in the system, because
`POST /v1/auth/admin-session` (D25) mints the strongest credential Agent GM
issues. A unit test enumerates every comparison site and fails on a `==` or a
`bytes.Equal` against a secret-derived value; the reviewer's plant discipline
(§13.5) mutates each site to `==` and requires a named test to kill it.

Secrets never appear in `argv`: `--secret-stdin`, `--paste`, `--paste-file`.
Passphrases
and cookies are read from a TTY prompt, stdin, a file read once, or the CDP
capture of §11.4, and are excluded from process arguments, logs, audit payloads
and error messages.

**Each `sessions/<acct>.enc` is equivalent to that Google account's
credential.** Anyone holding both the data directory and `AGENT_GM_DATA_KEY`
can act as **every** account the server holds, not merely as their Messages
sessions — one key covers all of them, so the blast radius grows with each
account added. `agm accounts sign-out` shreds one account's file, which is the only way to
shrink it without deleting data. This
governs §15.2 (a backup of `sessions/` is a credential backup and is stored
accordingly). There is no lower-privilege alternative: the gaia flow is the
only pairing Google Messages still offers (D19), so this exposure is inherent
to running Agent GM at all and must be planned for rather than avoided.

### 12.2 Logging

Structured logs carry request IDs, operation IDs, coarse event types, latency,
error codes and state transitions. Mandatory redaction covers:

- `Authorization` headers and bearer tokens.
- The admin secret and the data key.
- The Google tachyon token, refresh key, request-crypto keys, and **every one
  of the seven Google account cookies by name** (§3.2) — including in error
  strings bubbled up from `libgm`.
- Enrollment-code values and OAuth authorization codes.
- Message bodies, subjects and attachment bytes.
- Full phone numbers — logs carry a stable salted hash and the last four
  digits, never the number.
- **Google account addresses.** Logs carry the `acct_` ID, never the address;
  it is served on `/v1/accounts` and `/v1/health` because the owner needs to
  tell their accounts apart, and nowhere else.
- OAuth form bodies.

**Two loggers, and neither ever writes to stdout.** Agent GM's own logger and
the one handed to `libgm` are separate (`internal/logging`). Stdout carries a
command's result — a table, or the one JSON value `--json` promises (§11.3) —
so a log line in it is a parse error for anything downstream; both loggers
write to stderr, always.

`libgm`'s logger is tagged `component=libgm` and **floored at `warn`**, and a
one-shot CLI command silences it altogether. Upstream's narration of the long
poll ("Skip count is non-zero in postConnect, waiting longer") is a
commentary on a protocol Agent GM does not control, is not a diagnosis an
owner can act on, and printed inline with a command's output at the Slice 1
live gate. `AGENT_GM_LOG_LEVEL=debug` does not lower the floor. At `trace`
`libgm` base64-logs decrypted payloads (`event_handler.go:logContent`), so
`trace` is refused unless `AGENT_GM_UNSAFE_TRACE=1` is set — the one thing
that does go past the floor — which also stamps every line with
`unsafe_trace=true` and writes one `security.unsafe_trace_enabled` audit row
at startup.

The acceptance test is a scan: run the whole flow with sentinel values, and
assert none of them appear in captured logs, in `agent-gm.sqlite3`, in its
`-wal` or `-shm`, or in any audit payload (§13.2).

### 12.3 Rate limits and client source

| Surface | Limit |
|---|---|
| reads (`messages:read`) | 300 req/min per authorization, burst 100 |
| mutations (`messages:write`, `messages:delete`) | 120 req/min per authorization, burst 30 |
| `/v1/admin/*` | 120 req/min per authorization, burst 30 |
| admin-secret failures | 5 / 15 min per source, 20 / 15 min globally, exponential cooldown |
| enrollment-code failures | 10 / 15 min per signed OAuth context and per source |
| DCR | 20 / hour per source |
| unauthenticated `/oauth/token` refresh and `/oauth/revoke` | 60 req/min per source, burst 20; plus 30 invalid tokens / 15 min, durable |
| concurrent uploads | 4 per authorization, 16 globally |
| concurrent downloads | 8 per authorization, 32 globally |
| `/mcp` in flight | 8 per authorization, 32 globally |

Holding several scopes does not multiply the allowance, and **neither does
holding several accounts**: the buckets are per authorization and per source,
never per account (D29). One authorization sending to three accounts shares one
120/min mutation budget, so adding an account divides the per-account send rate
rather than adding to it. That is deliberate — the limits protect the server
and the phones from a runaway agent, and an agent is no less runaway for
spreading itself across accounts — but an operator should know it before adding
the fourth account. §16 Slice 2 asserts it. Exceeding one is
`rate_limited` with `Retry-After`. Token buckets for ordinary traffic;
durable cooldown metadata for the credential-failure limiters.

**Client source** — used by every per-source limit and by the `source` field
of every audit record:

| `AGENT_GM_TRUSTED_PROXY_CIDRS` | Source |
|---|---|
| unset or empty (default) | the TCP peer. Forwarded headers ignored entirely |
| a comma-separated CIDR list | if the TCP peer is inside the list, the **rightmost** `X-Forwarded-For` entry that is not itself inside the list; otherwise the TCP peer |

Rules:

- The walk is **right to left**. Taking the leftmost entry is the usual bug and
  would turn every per-source limit in the system into decoration.
- An unparseable entry **stops** the walk and falls back to the TCP peer;
  it is not skipped.
- Entries may be CIDRs (`127.0.0.1/32`, `10.0.0.0/8`, `::1/128`) or bare
  addresses read as host routes. An IPv4-mapped IPv6 peer
  (`::ffff:127.0.0.1`) matches an IPv4 CIDR.
- An invalid value **refuses to start**. An operator who mistypes the list
  should find out immediately, not discover months later that the trust they
  configured was never in force.
- The resolved source is never the empty string.
- `X-Forwarded-Proto` and `X-Forwarded-Host` are recorded but **never build a
  URL**. The issuer, the canonical resource, both discovery documents, every
  redirect, and every media URL come from `AGENT_GM_PUBLIC_URL` alone.

Cloudflare Tunnel terminates TLS and connects to `agent-gm:8080` over plain
HTTP from inside the Docker network, so a deployment sets
`AGENT_GM_TRUSTED_PROXY_CIDRS` to the tunnel container's network. Startup logs
`client_source_mode=socket_peer` or `=trusted_proxy` once, and
`GET /v1/health` reports the `client_source` **this** request resolved to, so
a misconfiguration where every caller collapses to one source is visible.
**With a trusted proxy configured, the listening port becomes a trust
boundary:** any process that can reach it can assert an arbitrary
`X-Forwarded-For` and choose its own source.

### 12.4 Audit

**Every audit row carries `account_id`** where the event belongs to an account
— pairing, logout, removal, every write operation, every state change — and
`NULL` where it does not, such as an OAuth or settings event. This is how "what
happened to this account" stays answerable after the account is removed (§4.7).

`audit_events` records security-relevant metadata:
**`account.paired`**, **`account.resumed`**, **`account.signed_out`**,
**`account.removed`** (with the deleted row counts),
**`account.label_changed`**, **`account.pair_failed`**,
**`account.state_changed`** (from, to, `state_reason`);
**`auth.admin_session_minted`** (source, granted scopes, whether narrowed —
never the secret), **`auth.admin_session_narrowed`** and
**`auth.admin_session_refreshed`** (scopes before and after),
**`auth.admin_secret_failed`** (source and the resulting cooldown state, never
the presented value); every write operation
and its outcome; enrollment-code creation, consumption, expiry and
revocation; authorization request creation, approval, denial and expiry;
authorization and client revocation; refresh-token reuse detection; settings
changes; backup and pruning; crash recovery; `message.status_out_of_order`;
`security.unsafe_trace_enabled`.

Payloads carry IDs, counts, codes and outcomes — never message text, never a
full phone number, never a token, never a cookie, never the data key. An audit
row is **never rewritten**, by a migration or by anything else: it records
what happened at the time it happened.

`GET /v1/admin/audit` and `agm admin audit list` filter by `kind`,
`kind_prefix`, **`account_id`**, `authorization_id`, `after`, `before`, with a
signed cursor.

---

## 13. Testing

### 13.1 The fake backend

`internal/gm/fake` implements `gm.Backend` (§2.3) as a deterministic in-memory
Google Messages. **One instance is one account**, so a test wanting two
accounts constructs two fakes and registers both with `internal/accounts`,
exactly as production does. The fake never models several accounts internally;
if it did, the multi-account paths above it would be tested against a shape
production does not have. It is not a mock with canned returns; it is a small
simulator, and it is what makes every layer above `gm` testable with no phone,
no network and no flakiness.

It must:

- Hold conversations, participants, messages, reactions and contacts, and
  answer `ListConversations` / `FetchMessages` with real cursors, including
  the equal-timestamp case.
- Accept a send, mint a message ID, and **emit the remote echo on the event
  channel**, carrying back the `TmpID` it was given — so the echo-correlation
  path (§6.3) is exercised, not stubbed.
- Walk delivery status on a script: `sending → sent → delivered → read`, or
  stop at `sent` when the conversation is `sms_mms`, which is what makes the
  §5.5 behaviour testable.
- Be scriptable to return, on the next call: each `SendMessageResponse.Status`
  (including `FAILURE_2`/`FAILURE_3` twice then success, to exercise the
  retry backoff), `ErrPhoneNotResponding`, `ErrConnectionClosed`,
  `ErrInvalidCredentials`, `ErrRequestedEntityNotFound`,
  `ErrCallerNoPermission`, an arbitrary `RequestError`, and
  `GetOrCreateConversation` status 3 then 1, and status 4.
- Emit any event in §3.4 on demand, including `IsOld=true` replays,
  `RevokePairData`, `GaiaLoggedOut`, `AccountChange`, and the alert types.
- Run the pairing flow: hand out an emoji, then accept or reject it with each
  `GaiaPairingErrorCode`, and yield a **settable** `AuthData.Mobile.SourceID`
  so a test can pair two distinct accounts, or re-pair the same one.
- Be able to drop events, so the `dropped_events` counter is tested, and to
  **abandon the rest of a batch** the way the library's dedup does (§3.4), so
  the reconciliation sweep of §5.4 is exercised against real loss.
- Answer `FetchConfig` and `IsDefaultSMSApp` from scriptable values, including
  a live `ConfigVersion` differing from the compiled one, so
  `config_version_stale` is testable with no phone.
- Return a scriptable `ResolveResult.Status`, including an integer the proto
  has no name for, so `google_undocumented_status` is testable.
- Run the pairing flow including `RefreshGoogleCookies`, a wrong-account
  refresh, and several primary devices with a settable `device_index`.

**Accounts diverge on purpose.** Because one fake is one account, every
scriptable behaviour above is scripted **per fake**, and the tests that matter
depend on it: account A healthy while B returns `ErrInvalidCredentials`; A's
batch abandoned by the dedup while B's is not; A backfilling while B is paused
by a `MOBILE_DATABASE_SYNC_STARTED`; A `connected` while B is `parked`. Nothing
in the fake is process-global.

**The fake takes a clock, and each fake takes its own.** Every timeout in this
spec —
`operations.pending_timeout` (24h), the `[3s, 8s, 20s]` send backoff,
`responseHardTimeout`, ticket expiry, the sweep interval — is measured against
an injected `Clock` whose test implementation advances on demand. Two fakes may
share one clock or hold two, and each test says which: §16 Slice 2's sign-out
and re-pair tests advance **one** account's clock while the other's stands
still, which is only expressible with separate clocks. No test sleeps, and no
test waits out a real 24 hours.

`AGENT_GM_BACKEND=fake` **together with `AGENT_GM_ALLOW_FAKE=1`** selects it at
runtime — both are required, and a `fake` backend without the second refuses to
start, so a production deployment cannot be talked into serving an empty
in-memory phone (§15.1). With both set, the CLI, the REST suite and the MCP
conformance run all drive a real server with no phone.

### 13.2 Unit and integration

Under `devbox run test`, no gate:

- **ID derivation**, including that the `account_id` component is present in
  every root derivation, and — the direction that matters — that **the same
  Google account on a *different phone* produces the *same* IDs**, while a
  **different Google account** produces different ones (§3.2, §4.1, D28). A
  test that computes the UUIDv5 independently, not by calling the same function
  twice.
- **Delivery-state mapping** for every one of the ~70 `MessageStatusType`
  values, as a table test enumerating the enum from the pinned proto (§13.4),
  so a value added upstream fails the test rather than silently landing in
  `unknown`.
- **Transition enforcement**: a backward move is refused, audited, and leaves
  the stored state alone; a forward skip is accepted.
- **Idempotency**: same key + same body returns the same operation and calls
  the backend **once** (asserted on the fake's call counter); same key +
  different body is `idempotency_conflict` and calls it zero times.
- **Crash recovery**: kill between the operation commit and the backend call,
  restart, assert `unknown` + `crash_recovered`, then deliver the echo and
  assert the correction to `succeeded`.
- **Ordering and dedup**: interleave backfill and live events for the same
  messages in both orders; assert one row each, identical content, identical
  ordering, and that `last_activity_ms` never moves backwards.
- **Strict parameter rejection** on every `/v1` route, including `_=`.
- **Cursor signing**: tamper rejection, filter-binding rejection, stability
  across equal timestamps.
- **Error mapping**: every library error in §3.5 to its code and status.
- **`config_version_stale`**: the fake returns a **non-`SUCCESS`
  `ResolveResult.Status`** *and* a live `ConfigVersion` differing from the
  compiled one → the error names both versions and says a pin bump is the fix.
  The test must **not** script a particular status number: §3.7's detection
  rule is the version diff alone, and asserting on a status code would
  re-import the unsourced claim §18.1 withdrew. A separate case proves the
  converse — a non-`SUCCESS` status with *matching* versions is
  `google_undocumented_status`, not `config_version_stale`.
- **Exit-code matrix**: every code in §11.2 produced by a real CLI invocation
  against a fake-backed server.
- **Golden JSON** for every response and error shape.
- **Log and database scanning** for sentinel secrets (§12.2), across
  `.sqlite3`, `-wal`, `-shm`, log capture and audit payloads.
- **`PRAGMA foreign_key_check`** empty after every migration, and a database
  at a higher `user_version` refuses to open.
- **Instructions parity**: the `initialize` instructions block and the "First
  five minutes" section of `docs/mcp.md` are byte-identical modulo quoting.
- **Cold-agent schema audit**: fetch `tools/list` from a live fake-backed
  server; assert every tool has a description, every argument has a
  description, every schema is `additionalProperties: false`, every write tool
  requires `client_request_id`, and every write tool's description ends with
  the fresh-key sentence.

- **Two accounts, in the ordinary suite**: ingest, ordering, dedup and the
  reconciliation sweep run with two fakes interleaved, asserting no cross-talk
  — every row lands under the right `account_id`, one account's sweep does not
  touch the other's rows, and one account's dedup loss does not trigger the
  other's sweep. These are the paths most likely to break under D27, and they
  need no gate.
- **Every query carries its account**: a test enumerates the store's queries
  and fails on one that filters conversations, messages, participants,
  contacts, operations or backfill state without either an `account_id`
  predicate or an explicit all-accounts marker.

Nothing here needs Docker or a phone, so **nothing here is behind a gate**.
Parking a test behind a gate it does not need is how a clause stays unverified
for a phase.

### 13.3 Live gates

Live tests are `go test -tags live` plus `AGENT_GM_LIVE=1`, run by
`devbox run test-live`. They are not in CI. They are run **by the coordinator
only**, never by an implementer or a reviewer, from a checkout pinned to the
reviewer-accepted commit — never from an implementer's working tree, because a
mid-edit tree failing to build is what makes a live gate meaningless.

**The approved-test-number rule.** Live sends go **only** to numbers the owner
has approved, referred to throughout this repository by placeholder:

| Placeholder | Purpose |
|---|---|
| `<APPROVED_DIRECT_NUMBER>` | the direct conversation used by every live gate |
| `<APPROVED_GROUP_NUMBER_1>`, `<APPROVED_GROUP_NUMBER_2>` | the two participants of the group live gate |

> **The real values are never written into this repository.** It is public
> (§1.4). They live in the operator's private notes, and at run time they come
> from `AGENT_GM_LIVE_NUMBERS` or an untracked, git-ignored
> `testdata/live-numbers.local`, read only by the live-gated tests. No commit,
> no fixture, no doc page, no example, no log and no commit message contains a
> real phone number. A commit that adds one is reverted, not amended.

**There is one approved set, for one account.** A second account would need a
second phone and a second approved set; §17 resolves this by running every
two-account test against two **fakes**, so no live gate is ever multi-account
and no second placeholder is needed.

Fictional `555` numbers (`+12025550123`) are reserved for examples and
fixtures and must never be dialled. **Only the coordinator sends live**; an
implementer or reviewer that believes it needs a live send reports that
instead of performing one.

A CI job, `no-real-numbers`, fails on any string in the tree that looks like a
NANP number and is not a `555` example.

Every destructive live action displays its exact effect and requires the
owner's confirmation before it runs.

### 13.4 Fixtures validated against the pinned source

Fixtures live in `testdata/libgm/be48a58/`, named for the pinned commit, and
are **source-derived, not live captures** — every identifier is a fixture
label or a `555` number, and every key is a nonfunctional placeholder.

A CI job, `fixture-validation`, clones mautrix-gmessages at `be48a58` and
asserts, against that tree and not against Agent GM's own code:

1. Every symbol named in §3.1 exists with the signature stated.
2. `util.ConfigMessage` equals `2026.9.2` with `V1=4, V2=6`.
3. §4.4 covers every `MessageStatusType` value in `conversations.proto`
   **outside 200–279** exactly once, with no value unmapped; every value
   **inside** 200–279 classifies as `kind='system'` (assertion 12). A value in
   neither set fails.
4. `GetOrCreateConversationResponse.Status` still declares only `0, 1, 3`,
   so §3.7's claim that 2 and 4 are unnamed is still true.
5. `SendMessageResponse.Status` still declares `0..4`.
6. `SendReactionRequest.Action` still declares `0..3`.
7. `ListConversationsRequest.Folder` still declares `0, 1, 2, 5`.
8. `AlertType` still has **28** values (0–27), and the ones §3.4 acts on still
   carry the numbers stated.
9. `responseHardTimeout` is still 60s, `RefreshTachyonBuffer` still 1h,
   `alertTimeoutCount` still defaults to 4, and `GaiaInitTimeout` still 20s.
10. `shouldIgnoreStatus`'s ignore set matches the one Agent GM carries.
11. The `sendRetryBackoff` values are still `[3s, 8s, 20s]` and
    `isTransientSendFailure` still names only `FAILURE_2` and `FAILURE_3`.
12. `MessageStatusType` system events still occupy **200–279** and
    `MESSAGE_DELETED` is still **300**; §4.4 maps every value outside 200–279.
13. `EmojiType` still declares exactly the 14 values of §3.7, `Unicode()` still
    renders `RED_HEART` as `❤️`, and `UnicodeToEmojiType` still accepts both
    `❤` and `❤️`.
14. `util.GenerateTmpID` still returns a bare `uuid.NewString()`.
15. The gaia required-cookie list is still exactly
    `SID, HSID, OSID, SSID, APISID, SAPISID`, with `OSID` scoped to
    `messages.google.com` and the rest to `.google.com`.
16. `StartGaiaPairing` still selects by last-seen and
    `GaiaHackyDeviceSwitcher` rather than erroring on several devices, and
    `ErrHadMultipleDevices` still appears only wrapped in
    `ErrPairingInitTimeout`.
17. The listen loop still makes **401 and 403** fatal.
18. `deduplicateUpdate`'s callers still `return` out of the batch loop on a
    hit — the loss behaviour §5.4's sweep exists for.
19. `events.NewBrowserActive` still has no callers outside `pkg/libgm/gmtest`,
    so §3.4 is right not to subscribe to it.
20. `connector/login.go` still re-authenticates an existing pairing from fresh
    cookies, gated on a non-nil tachyon token and a non-nil `PairingID`.

A test that only asserts Agent GM's own serialisation round-trips is **not**
compatibility evidence. If a claim in §3 cannot be checked against the pinned
tree by a job, it is written in §3 with the file and symbol it was read from,
so a reviewer can check it by hand.

### 13.5 Reviewer discipline and plants

The reviewer's default assumption is that nothing works.

- **Evidence is something the reviewer ran.** A description from the
  implementer is not evidence. Real output is quoted, with the command and the
  exit code.
- The reviewer diffs the implementation against this spec **clause by
  clause**. Each public contract item — route, DTO field, error code, tool
  schema, state transition, CLI flag, exit code, setting — is either verified
  by a test the reviewer ran, or listed as unverified. Rows are
  **SHA-qualified**: `verified <sha>`, `failed <sha>`, `partial <sha>`,
  `gap <sha>`, `unverified`, or `n/a` — and `n/a` rows are still listed, so
  every deferral is explicit.
- **Plants are the reviewer's, not the implementer's.** Two kinds:
  - *Planted tests*, written on the review branch **before** the code exists,
    `t.Skip`ped so `devbox run check` stays green, and un-skipped by the
    reviewer with `-run` at every reported SHA. Each declares the slice that
    unblocks it and its wire assumptions up front. A plant must fail at the
    *expected* point against the baseline, proving it exercises real routes
    rather than nothing. The implementer may adapt a plant's call shape; it may
    **not** weaken an assertion.
  - *Planted mutations*: the reviewer edits production source at a named
    file:line, runs the full suite, records killed (by a **named** test) or
    **SURVIVED** (with the pass count), and reverts with `git checkout --`. A
    survivor is a finding, not a note; the reviewer states the production
    consequence. After the fix the mutation is re-planted and must be killed,
    and the killing test carries a doc comment recording the plant and its
    date. Documentation contracts are mutated too — putting a wrong sentence
    back into a served tool description must be caught by a lint test.
- Findings are `<Letter>-<n>`, severity-ordered, each with the spec clause, the
  evidence run, and the fix required. The verdict is `accept`,
  `accept with required fixes`, or `reject`.
- The reviewer works in its own worktree and syncs with
  `git fetch . <branch>` then `git reset --hard <sha>`, so its tests run
  against exactly what was reported.

**A name lint** (`internal/lint`, run under `devbox run test`) reads the
catalogue the server actually serves — tool names, descriptions, argument
names and descriptions, enums, the instructions block — plus every markdown
page under `docs/`, and fails on a Matrix vocabulary asserted as
a live contract: `room`, `portal`, `event_id`, `provider`, `redact`,
`generation`, `outbox`, **`tombstone`**, a bare `mode` argument, or a `room_`
prefix.

Three scoping rules, all load-bearing, because a lint that fails its own spec
is a lint nobody will keep:

- **The served catalogue is checked for every banned word**: tool names, tool
  descriptions, argument names and descriptions, enum values, and the
  instructions block — the things a cold agent reads.
- **Markdown is checked under `docs/` only**, and only outside fenced code
  blocks and block quotes. `plans/AGENT_GM_SPEC.md` is exempt in full: it has
  to say what Agent MX called things (§1.3, §18.3), name upstream files such
  as `connector/handlematrix.go`, and use English like "exit-code matrix" and
  "log redaction".
- **The banned `mode` is an argument name**, matched whole against a deny
  list, never as a substring. `send_mode` never reaches a surface (§4.6), and
  `search`'s `mode` is `words|exact`, a search vocabulary rather than a switch
  between destructive behaviours — which is the thing Agent MX retired.

Its meta-test proves both directions: the banned words are caught in a served
description and in a `docs/` page, and the exempt cases — §18.3, a fenced
block quoting upstream, `send_mode`, `search.mode` — are not.

### 13.6 CI

GitHub Actions, on push and pull request:

| Job | What |
|---|---|
| `check` | `devbox run check` — build, vet, `golangci-lint run`, `go test -race` |
| `pin-consistency` | `go.mod`, `internal/gm/pin.go` and §3.6 all name `be48a58` |
| `fixture-validation` | §13.4, against a fresh clone of the pinned upstream tree |
| `conformance` | `devbox run conformance` against the baseline (§8.4) |
| `lint-names` | the name lint of §13.5 |
| `no-real-numbers` | §13.3 — nothing in the tree looks like a real phone number |
| `build-matrix` | `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` binaries |
| `image` | build the Dockerfile; on a tag, push to GHCR |

CI installs Devbox and runs the same scripts, so the local and CI environments
are identical. **Every job in that table is a `devbox run <script>`** —
including `pin-consistency`, `fixture-validation`, `build-matrix` and `image`,
which are `devbox.json` scripts calling `scripts/*.sh` exactly as
`conformance` does. A CI step that shells out to something with no script is a
step nobody can reproduce locally, and is rejected in review.

`devbox run check` is what a reviewer runs, so it is the aggregate: build,
vet, `golangci-lint run`, `go test -race`, **`lint-names`** and
**`no-real-numbers`**. Those two lints are cheap and they guard contracts a
test suite cannot see.

---

## 14. Packaging

### 14.1 Dockerfile

Two stages. Build with `golang:1.27` on the pinned Go version; run on
`gcr.io/distroless/static-debian12:nonroot`, because `CGO_ENABLED=0` and a
pure-Go SQLite driver (`modernc.org/sqlite`) mean there is nothing to link
against.

```dockerfile
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/agent-gm ./cmd/agent-gm

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent-gm /usr/local/bin/agent-gm
ENV AGENT_GM_DATA_DIR=/data AGENT_GM_LISTEN_ADDR=0.0.0.0:8080
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/agent-gm"]
CMD ["serve"]
```

`main.commit` is what `GET /v1/health` reports as `source_url` and what
`serverInfo` carries (§1.4).

`compose.example.yml` ships in this repository as a **generic** example: the
service, the volume, the environment, and a commented-out Cloudflare Tunnel
sidecar. The owner's real Compose service lives in `openclaw-custom` and is
**out of scope for this repository**, as is the tunnel configuration. What
this repository owes any host is: a Dockerfile, a compose example, an
environment-variable table (§15.1), and the operations docs.

Healthcheck: `agent-gm healthcheck` — an internal subcommand that does an
HTTP `GET /healthz` against its own listen address, so distroless needs no
`curl`.

### 14.2 GHCR

`ghcr.io/thisnick/agent-gm`. Tags: `vX.Y.Z`, `vX.Y`, `latest` on a release;
`sha-<short>` on every `main` build. Multi-arch `linux/amd64` and
`linux/arm64`. **Deployments pin by digest, not by tag** — a tag is a label
and a digest is evidence. Every release note records the digest and the
upstream `libgm` commit.

### 14.3 Releases and the npm wrapper

A GitHub release for tag `vX.Y.Z` carries, from a public repository:

```text
agent-gm_X.Y.Z_linux_amd64.tar.gz
agent-gm_X.Y.Z_linux_arm64.tar.gz
agm_X.Y.Z_linux_amd64.tar.gz
agm_X.Y.Z_linux_arm64.tar.gz
agm_X.Y.Z_darwin_amd64.tar.gz
agm_X.Y.Z_darwin_arm64.tar.gz
checksums.txt
checksums.txt.sig        (cosign keyless, GitHub OIDC)
```

`@agent-gm/cli` on npm is a **thin wrapper**, not a bundle:

- `package.json` declares `"bin": { "agm": "bin/agm.js" }`, `"license":
  "AGPL-3.0-or-later"`, and `"version"` equal to the Go release version, so
  `npm i -g @agent-gm/cli@1.4.2` gets `agm 1.4.2`.
- A `postinstall` script maps `process.platform` × `process.arch` to an asset
  name, downloads it from
  `https://github.com/thisnick/agent-gm/releases/download/v<version>/…`,
  **verifies the SHA-256 against a `checksums.txt` that is itself pinned into
  the npm tarball at publish time** (so a compromised release page cannot
  serve a different binary to an old package version), extracts to
  `vendor/agm`, and `chmod 0755`.
- `bin/agm.js` is a shim that `execFileSync`s `vendor/agm` with the process's
  argv and propagates the exit code **exactly** — the §11.2 codes must survive
  the wrapper, and there is a test that asserts `agm --nonsense` exits 2
  through npm as it does natively.
- An unsupported platform fails `postinstall` with a message naming the
  platform and pointing at the release page, rather than installing something
  that cannot run.
- `AGENT_GM_CLI_SKIP_DOWNLOAD=1` skips the download for CI images that supply
  the binary themselves; `AGENT_GM_CLI_BINARY=<path>` points the shim at an
  existing binary.
- The npm package is AGPL-3.0-or-later and says so, because it distributes
  AGPL binaries (§1.4).

Releases are cut by tagging; the `release` workflow builds the matrix, signs
the checksums, creates the GitHub release, pushes the GHCR image, and
publishes to npm with an OIDC-authenticated token. **No release is cut from a
commit that has not passed a live gate** (§16).

---

## 15. Operations

### 15.1 Configuration

Environment only, plus a runtime settings table. **There is no config file.**

| Variable | Default | Meaning |
|---|---|---|
| `AGENT_GM_PUBLIC_URL` | *(required)* | `https://gm.agent-wx.app`. Issuer, canonical resource, and the base of every URL handed out. Never derived from `Host` |
| `AGENT_GM_LISTEN_ADDR` | `0.0.0.0:8080` | bind address |
| `AGENT_GM_DATA_DIR` | `/data` | holds `agent-gm.sqlite3`, `sessions/` (one file per account, §3.3), `media-cache/`, `backups/` |
| `AGENT_GM_ADMIN_SECRET` | *(required)* | owner bootstrap credential, ≥43 chars |
| `AGENT_GM_DATA_KEY` | *(required)* | 256-bit, 64 hex or base64. Not rotatable |
| `AGENT_GM_TRUSTED_PROXY_CIDRS` | empty | §12.3. Invalid value refuses to start |
| `AGENT_GM_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `AGENT_GM_LOG_FORMAT` | `json` | `json` \| `text` |
| `AGENT_GM_UNSAFE_TRACE` | unset | `1` permits `libgm` trace logging. §12.2 |
| `AGENT_GM_BACKEND` | `libgm` | `libgm` \| `fake` |
| `AGENT_GM_ALLOW_FAKE` | unset | `1` permits `AGENT_GM_BACKEND=fake`. Without it a `fake` backend refuses to start, so a production deployment cannot be talked into serving an empty in-memory phone |
| `AGENT_GM_LIVE_NUMBERS` | unset | live-gate targets, `direct,group1,group2` (§13.3). Read only by `-tags live` tests; never committed |
| `AGENT_GM_PAIRING_TIMEOUT` | `5m` | how long a started pairing may sit unconfirmed before Agent GM discards the half-built `AuthData` and deletes any `pairing` row (§3.2). Distinct from upstream's `GaiaInitTimeout` (20s) and from Google's own `ErrPairingTimeout` |
| `AGENT_GM_ACCOUNT` | unset | CLI only. Default `acct_` ID for `--account` (§11.1) |
| `AGENT_GM_CHROME` | unset | CLI only. Path to a Chrome or Chromium binary for the default pairing flow; searched before the platform's usual locations and `PATH` (§11.4) |
| `AGENT_GM_URL` / `AGENT_GM_ACCESS_TOKEN` / `AGENT_GM_REFRESH_TOKEN_FILE` / `AGENT_GM_CLIENT_ID` / `AGENT_GM_CREDENTIALS_FILE` | — | CLI side only (§11.5) |
| `AGENT_GM_CLI_SKIP_DOWNLOAD` / `AGENT_GM_CLI_BINARY` | — | npm wrapper only (§14.3) |

Runtime settings, mutable through `PATCH /v1/admin/settings`, each reporting
its effective value, source (`default` \| `environment` \| `database`),
mutability and restart requirement:

**Scope** says whether a value is one server-wide budget (`server`) or a
server-wide value **applied to each account independently** (`per account`).
Getting this wrong is how a two-account deployment does twice the work it was
configured for, or half.

| Key | Default | Bounds | Scope |
|---|---|---|---|
| `backfill.concurrency` | 2 | 1–8 | per account — worst case is this × `accounts.max_concurrent` |
| `backfill.conversation_page_size` | 100 | 10–500 | per account |
| `backfill.message_page_size` | 100 | 10–500 | per account |
| `backfill.max_messages_per_conversation` | 2000 | 100–100000 | per account |
| `backfill.horizon` | 365d | 7d–3650d | per account — cost multiplies by account count |
| `backfill.include_archive` | true | — | per account |
| `ingest.sweep_interval` | 15m | 1m–6h | per account |
| `accounts.max_concurrent` | 8 | 1–32 (§4.7) | **server** |
| `operations.send_deadline` | 300s | 60s–600s | server |
| `operations.idempotency_ttl` | 30d | 1d–365d | server |
| `operations.pending_timeout` | 24h | 1h–7d | server |
| `operations.wait_timeout` | 60s | 5s–10m | server |
| `media.upload_max_bytes` | 104857600 | mutable downward only | server |
| `media.cache_max_bytes` | 2 GiB | — | **server** — the LRU is cross-account (§10.3) |
| `media.inline_mcp_image_max_bytes` | 1 MiB | — | server |
| `oauth.*`, `admin.*` | §9.6 | | server |
| `backup.keep` | 7 | 1–100 | server |
| `logging.level` | `info` | reloadable without restart | server |

A retired key becomes an unknown key: setting one answers `invalid_request`
naming its replacement, rather than silently writing a key nothing reads.

### 15.2 Backup and restore

`POST /v1/admin/backup` (or `agm admin backup`) writes
`<data_dir>/backups/agent-gm-<timestamp>-<id>.sqlite3` using the **SQLite
backup API**, not a file copy: pages are copied under the database's own
locking and the copy restarts if a writer changes a page it has already taken,
so the result is a usable database while the server keeps serving. The
snapshot has no `-wal` sidecar and opens on its own. The caller does not
choose the path.

After each successful backup the newest `backup.keep` snapshots are kept and
older ones deleted, each removal audited as `admin.backup_pruned`. Pruning
never touches a file not named `agent-gm-<timestamp>-<id>.sqlite3`, and a file
it cannot delete is logged and left rather than failing a backup that had
already succeeded. **Retention counts calls, not days** — an hourly cron with
`backup.keep=7` keeps seven hours.

**A complete backup is three things and they move together:**

1. the snapshot (or the whole `data/` directory),
2. **the whole `sessions/` directory** — one file per account (§3.3),
3. `AGENT_GM_DATA_KEY`.

Without (3), (2) is unreadable and cached media is unreadable. Restore = put
all three back. Restoring the database without `sessions/` leaves every
account `signed_out` with its history intact (§4.7) — a safe state, recoverable
by re-pairing each one.

**A backup of `sessions/` is a backup of the owner's Google account
credentials — all of them** (§3.2, §12.1), not merely of a Messages
session. Store it the way a credential is stored, and never in the same place
as `AGENT_GM_DATA_KEY`. There is no pairing flow that stores anything less
(D19), so this is the standing handling rule, not a per-flow caveat. If (1) and (3) survive but `sessions/` does not, every account starts
`signed_out` with its history intact (§4.7) and the owner re-pairs each one.
Because `acct_` is derived from the **Google account address**, not from the
phone (§3.2, §4.1), re-pairing keeps every existing `conv_` and `msg_` ID —
**including onto a different phone**, which is the whole reason the identifier
was chosen that way (D28).

### 15.3 Health and diagnosis

`GET /healthz` for liveness — `ok` whenever the process is serving, whatever
the accounts are doing. `GET /v1/health` for everything else, in the shape §7.5
defines: `version`, `commit`, `source_url`, `config_version_compiled`,
`upstream_commit`, an **`accounts_summary`** of totals by state, and an
**`accounts[]`** array carrying per account its `state`, `state_reason`,
`phone_responding`, `google` block, `backfill`, `sweep` and `counters` — plus
the `client_source` this request resolved to. There is no server-level
`session` object; §7.5 holds the one authoritative shape.

`agm accounts list` is the fast operator view. `agm session --account <id>
--watch` streams one account's state changes; `agm session --watch` streams
every account's.

### 15.4 Runbook

| Symptom | Cause | Fix |
|---|---|---|
| `session envelope cannot be decrypted` at startup | `AGENT_GM_DATA_KEY` differs from the key that sealed `sessions/*.enc` | restore the original key. **There is no in-place rotation.** Without it, every account is `signed_out` with its history intact |
| every write is `not_paired` | there are **no accounts at all** | `agm pair`. A server with zero accounts is healthy (§7.5), it just cannot send |
| a write is refused `unsupported_capability` / `not_signed_in` | that one account is not usable; the rest may be fine | see the account's `state` in `agm accounts list` |
| an account goes to `error` with `RevokePairData` in the audit log | that phone revoked the pairing | `agm pair --account <id>`; history resumes (§4.7) |
| starting a conversation fails and `agm health` shows `config_version_stale: true` | the pinned `libgm` `ConfigVersion` is older than Google's | **bump the pin** (§3.6). Retrying does not help. See D3 for the field observation this rule comes from |
| starting a conversation fails with `google_undocumented_status` and the ConfigVersions match | Google returned a status the pinned proto has no name for | record `details.status` and the request, and report it upstream. Do not invent a meaning |
| one account is `signed_out`, others fine | its cookies expired, or the owner signed it out | `agm pair --refresh-cookies --account <id>`, or `agm pair` again. Its history was never touched (§4.7) |
| an account's Google address changed | the owner renamed the Google account (§3.2) | a re-pair creates a **second** account. Confirm, then `agm accounts remove` the stale one — that is the only command that deletes |
| paired against the wrong Android phone | the account has several and the library picked the most recently seen | `agm pair --device-index 1` (§11.4) |
| an account sits in `parked` | `accounts.max_concurrent` is reached, so it holds no client — **not** an error and not transient; `state_reason` is `capacity` | raise `accounts.max_concurrent`, or sign out an account that is no longer needed. `agm accounts list` shows which accounts hold the slots |
| an account is `account_changed` and refuses writes | the phone switched to a different Google account underneath the pairing (§3.4) | decide which account the phone should serve. To keep the original, switch the phone back and `agm pair --refresh-cookies --account <id>`. To adopt the new one, `agm pair` it as a **new** account; the old account's history stays readable and is deleted only by `agm accounts remove` |
| `agm pair` reports no Chrome found | no Chrome or Chromium on this machine | use `--server` from a machine that has one, or `--paste` (§11.4). Set `AGENT_GM_CHROME` if Chrome is in an unusual place |
| every send is `not_default_sms_app` | Google Messages is not the phone's default SMS app | change it on the phone; `agm health` reports `is_default_sms_app` |
| a group send fans out as separate SMS threads | the phone's *Group messaging* is set to "Send an SMS reply to all recipients" | set it to MMS on the phone (§11.4). Agent GM can only report what Google returns |
| sends time out with `phone_not_responding` | the phone is asleep or offline | wait; the operations are `pending` and the echo will settle them. **Do not resend.** |
| `dropped_events` climbing | ingest is slower than the event stream | a bug; capture `agm admin diagnostics` and file it |
| every caller collapses to one source in the audit log | `AGENT_GM_TRUSTED_PROXY_CIDRS` does not cover the tunnel container | fix the CIDR; compare `client_source` in `/v1/health` against the address you came from |
| the process refuses to start naming a CIDR | a mistyped proxy list | fix it. This is deliberate (§12.3) |
| a WAL checkpoint reports `busy` | a long-lived reader | not an error; the next pass finishes it |

Hourly upkeep task: **optimize the FTS index, then checkpoint the WAL with
`TRUNCATE`, in that order** — `optimize` writes, so checkpointing first would
leave its pages in the log for the next pass to carry. The pass measures the
log *file size*, not pragma page counts, because a successful `TRUNCATE`
resets the log and therefore reports zero pages checkpointed, which is
indistinguishable from having done nothing.

### 15.5 Upgrading

Migrations are the whole mechanism (§4.3). Pull the new image tag by digest,
recreate the container, and the process migrates before it binds. Downgrade is
not supported: a database at a higher `user_version` refuses to open. A
migration that adds a column every existing row needs — `account_id` being the
archetype — sets `server_meta.pending_reprocess` and derives the values once
after startup (§4.3) rather than shipping a hand-written data migration.

Upgrading the `libgm` pin is a **deliberate slice**, never part of a routine
upgrade — see §3.6 for the required steps and the live gate.

**What to do about `config_version_stale: true`.** It is informational (D32).
Google ships a new `ConfigVersion` on its own schedule, so an account will
report `config_version_stale: true` whenever Google is ahead of the pin, and
that is the normal resting state between pin bumps. It does not change
`GET /v1/health`'s `status`, it is not an error, and it does **not** on its
own call for any action:

- **Everything works** → do nothing. Note it and carry on.
- **Starting a conversation fails** *and* the versions differ → this is the
  `config_version_stale` **error code** (§7.2), and the fix is a pin bump as
  its own slice with its own live gate (§3.6). Retrying does not help.
- **Starting a conversation fails and the versions match** → it is not this;
  see `google_undocumented_status` in §15.4.

### 15.6 If the public URL ever has to change

This is a migration, not a config edit. Changing `AGENT_GM_PUBLIC_URL`:

1. invalidates every access and refresh token (they are audience-bound),
2. orphans every registered OAuth client, because a client's redirect and the
   issuer it verified are tied to the old origin,
3. means claude.ai and ChatGPT connectors must be **removed and re-added by
   hand**, each requiring a fresh enrollment code and a fresh approval.

The procedure is: issue new enrollment codes, change the variable, restart,
re-add each connector, then revoke the stale authorizations and clients. There
is no way to make old tokens keep working, and no attempt should be made.

---

## 16. Phases and slices

Four slices. Each is implemented, reviewed, and — where it touches the phone —
live-gated by the coordinator before the next begins. `main` advances only
when a slice is accepted by its reviewer **and** its live gate has passed.
Tags: `slice-1`, `slice-2`, `slice-3`, `v1.0.0`.

Every acceptance test below is a falsifiable assertion with a runnable
evidence recipe. A slice is not accepted with any test unproven, and an
unproven test is listed as `gap <sha>`, never quietly dropped.
### Slice 1 — the spike

**Goal:** prove Agent GM can pair, read and write against the real thing,
before any of the surfaces exist.

**Deliverables.** The Go module; `devbox.json` as shipped; `internal/gm` with
the complete `Backend` interface (§2.3), the `libgm` implementation and the
fake; a minimal `internal/accounts` able to hold **more than one** backend;
`internal/store` with migration `0001` covering `accounts`, `conversations`,
`messages`, `participants` and `server_meta`; a `cmd/agent-gm spike`
subcommand with `pair`, `list`, `send`, `watch` and `diag`;
`internal/gm/pin.go`; the `pin-consistency`, `fixture-validation` and
`no-real-numbers` CI jobs.

**Explicitly not in this slice:** REST, MCP, OAuth, media, the CLI proper,
idempotency, the audit log.

**Acceptance tests.**

1. `devbox run check` is green, and `go.mod`, `internal/gm/pin.go` and §3.6 all
   name `be48a58`; `pin-consistency` fails if any one is edited alone.
2. `fixture-validation` passes all twenty assertions of §13.4 against a fresh
   clone of mautrix-gmessages at `be48a58`.
3. Against the **fake**: pair, list 3 conversations, send a text, receive the
   echo, and see `delivery_state` walk `sending → sent → delivered`. No
   network, no sleeps — the fake's clock is advanced explicitly.
4. **Two fake accounts run concurrently.** Register two fakes with distinct
   `AuthData.Mobile.SourceID` values; both reach `connected`; each ingests its
   own messages; every row carries the right `account_id`; the two `acct_` IDs
   are the UUIDv5 of their lowercased addresses and differ. Pairing the
   *same* address twice yields **one** account, not two.
5. Every error in §3.5 maps to the `gm`-level error value §7.2 will later
   render, proven by a table test that **enumerates** the list rather than
   sampling it. This is a unit test inside `internal/gm`; no REST exists yet.
6. `internal/gm/fake` satisfies the whole `Backend` interface — asserted by a
   compile-time `var _ gm.Backend = (*fake.Backend)(nil)` plus a test that
   calls every method once. If a method is unimplementable against the fake,
   it does not belong in the interface.
7. **Live gate (coordinator only).** `agent-gm spike pair` — the only pairing
   flow (D19) — launches system Chrome with the dedicated
   `--user-data-dir` and a loopback CDP port; the owner signs in; the spike
   waits for `OSID`, captures exactly the seven cookies across
   `messages.google.com` and `www.google.com`, terminates Chrome, prints the
   emoji, and the owner taps it. The process reports `Paired` with a phone ID
   and persists `sessions/<acct>.enc` at mode `0600`. Nothing writes the
   cookies to any other file, asserted by watching the process's `openat`
   calls for the duration.
8. **Live gate.** On a machine with Chrome hidden from `PATH`,
   `agent-gm spike pair` prints the two-option no-Chrome message of §11.4 in
   the stated order and exits `9` — it does not report a missing binary.
9. **Live gate.** `agent-gm spike list` returns the owner's real conversation
   list, and the row for `<APPROVED_DIRECT_NUMBER>` is present with a `conv_`
   ID.
10. **Live gate.** `agent-gm spike send <conv> "agent-gm slice 1 test"` to
   **`<APPROVED_DIRECT_NUMBER>` and no other number** returns
   `SendMessageResponse_SUCCESS`; `spike watch` shows the remote echo carrying
   the same bare-UUID `TmpID` we sent, then at least one delivery-status
   update.
11. **Live gate.** The owner replies from that phone; `spike watch` prints the
   inbound message within 10 seconds, with the right text and sender.
12. **Live gate.** Restart the process; each account's session file reloads, `Connect`
    succeeds without re-pairing, and `IsLoggedIn()` is true.
13. **Live gate.** `agent-gm spike diag` prints the compiled and live
    `ConfigVersion` and `is_default_sms_app` from a real round trip; both are
    recorded in the slice report. (The fake can produce these too — test 3
    covers that — but the *live* values are the point here.)

### Slice 2 — store, REST, CLI, media

**Deliverables.** The full schema and migrations; the ingest loop with
backfill, live events and the reconciliation sweep; operations with
idempotency and crash recovery; every `/v1` route of §7 including
`/v1/auth/admin-session` and the admin subtree; the media ticket system of
§10; the `agm` CLI of §11 including the pairing flow and its `--paste` fallback,
the `agm accounts` subtree and `--account`; the `internal/accounts`
supervisor; the audit log; rate
limits and trusted-proxy resolution; `docs/api.md`, `docs/cli.md`,
`docs/pairing.md`, `docs/operations.md`.

Authentication in this slice is the **admin bootstrap only**. Per §9.7 that
session carries `admin` **plus all three messaging scopes**, and accepts a
`scopes` narrowing, so this slice can call and refuse its own routes. OAuth
arrives in Slice 3 and adds a second token source, not a second security
model.

**Acceptance tests.**

1. A populated fake-backed database survives a restart with identical list
   output, identical IDs and identical ordering, including across two messages
   with the same millisecond timestamp.
2. Backfill and live ingestion of the same 500 messages, interleaved in both
   orders, produce exactly 500 rows with identical content hashes; a third
   replay writes zero rows and does not change any `updated_at_ms`.
3. `last_activity_ms` never moves backwards, proven by replaying an old
   message after a new one.
4. **The reconciliation sweep recovers real loss.** Script the fake to
   abandon the rest of a batch the way the library's dedup does (§3.4); assert
   the messages are missing; fire a `BROWSER_ACTIVE` with a changed session
   ID; assert the sweep runs and every missing message is now present exactly
   once. Repeat for the timer, `NoDataReceived` and
   `MOBILE_DATABASE_SYNC_COMPLETE` triggers.
5. Every `MessageStatusType` value at the pin outside 200–279 maps to a
   `delivery_state` in §4.4, and every value in 200–279 classifies as
   `kind='system'`. A value absent from both fails the test.
6. A backward transition (`read → sent`) is refused, leaves the stored state
   alone, still writes `delivery_state_raw`, and emits
   `message.status_out_of_order`. A forward skip is accepted.
7. The same `client_request_id` with the same body returns the same operation
   and calls the backend **once** (fake call counter); with a different body it
   is `idempotency_conflict` and calls it **zero** times. A key supplied as a
   query parameter is `invalid_request`.
8. Killing the process between the operation commit and the backend call, then
   restarting, leaves the operation `unknown` with `crash_recovered`; feeding
   the echo afterwards corrects it to `succeeded` with a `message_id` and sets
   `corrected_at`. **Nothing is resent.**
9. `ErrPhoneNotResponding` yields HTTP 504 `phone_not_responding` **and an
   operation in `pending` with `terminal: false`**, not `failed`; the echo
   settles it to `succeeded`, or to `failed` if the echo reports a failed
   status; with no echo it becomes `unknown` after
   `operations.pending_timeout` — **advanced on the injected clock, not
   waited out**.
10. `FAILURE_2` twice then `SUCCESS` succeeds after backoffs of `3s` and `8s`
    on the injected clock, reusing **one** `tmp_id` across the retries;
    `FAILURE_4` is `not_default_sms_app` and is **not** retried.
11. `ResolveResult.Status = CREATE_RCS` retries once with
    `CreateRCSGroup=true` and succeeds; a second `CREATE_RCS` is
    `google_error`; an **unnamed integer** is `google_undocumented_status`
    with `details.status` carrying the bare number and no invented name; and a
    failure with a compiled/live `ConfigVersion` mismatch is
    `config_version_stale` naming both versions.
12. Every `/v1` route rejects an unknown query parameter and an unknown body
    field with `invalid_request` naming it, including `?_=1`; route by route,
    not a sample.
13. A `msg_` ID where a `conv_` is expected is `invalid_request` naming the
    parameter and the expected prefix — **never `not_found`**. A raw Google ID
    is the same.
14. A cursor replayed with a changed filter is `invalid_request`; a tampered
    cursor is `invalid_request`; pagination across ten equal timestamps
    returns each row exactly once.
15. Emoji canonicalisation: `add_reaction` with `❤` then
    `DELETE …/reactions/❤️` removes it, and the reverse also works; both
    derive the same `react_` ID. A second, different emoji from the same
    person **replaces** the first (one row, `SWITCH` sent). A reaction whose
    type has no unicode serves `{"emoji": null, "type": "emotify"}`.
    `DELETE /v1/reactions/{react_id}` removes by ID; somebody else's is
    `unsupported_capability` with `not_my_reaction`.
16. `PATCH /v1/conversations/{id}` archives, unarchives, pins, unpins and marks
    unread; repeating one returns `changed: false` with `operation: null` and
    calls the backend zero times.
17. The full upload path: reserve, `PUT` the exact bytes, send. **Uploads are
    account-agnostic** (§10.2): an upload reserved with no account in mind
    sends successfully into a *second* account's conversation, and a second
    send of the same upload — into either account — is refused. A short body, a
    long body, a wrong `sha256` and a contradicted content type are each
    refused **and spend the reservation**. A second `PUT` with the same token
    fails. The token presented at another upload's URL is refused **and not
    spent**, proven by then redeeming it successfully at its own URL. Two
    `upload_ids` in one send is `invalid_request`.
18. A download ticket redeems 5 times and the 6th fails, proven against the
    `download_tickets.redemptions` counter and across a process restart. Each
    redemption re-checks the issuing authorization: narrowing that session's
    scopes to drop `messages:read` (via `POST /v1/auth/admin-session` with
    `scopes`) makes the next redemption fail.
19. `upload_url` and `download_url` are built from `AGENT_GM_PUBLIC_URL` even
    when the request carries a hostile `Host` and `X-Forwarded-Host`.
20. Erasure order, tested through `DELETE /v1/accounts/{id}` specifically,
    since §4.7 makes account removal *the* erasure path: it collects the cached
    media paths inside the transaction, commits, then unlinks; a fault injected
    between commit and unlink leaves an orphan file and **no** orphan
    `media_cache_entries` row.
21. The exit-code mapping table of §11.2 in full: every §7.2 code produced
    against a fake-backed server maps to the stated exit code. Exit 3 comes
    from an expired admin access token (advanced past
    `admin.access_token_ttl` on the injected clock); exit 4 comes from a
    **narrowed** admin session calling a route outside its scopes.
22. **A narrowed admin session cannot re-widen.** `POST /v1/auth/refresh` with
    `scopes` wider than the session was minted with — including the full set
    after a narrowing — is `invalid_scope`, **does not spend** the presented
    refresh token, and leaves the session's scopes unchanged; the next call to
    the out-of-scope route is still exit 4. Widening requires presenting the
    admin secret again. Test 21's exit-4 case is void without this.
23. **Every secret comparison is constant-time.** A test enumerates the
    comparison sites of §12.1 and fails on `==` or `bytes.Equal` against a
    secret-derived value; the reviewer plants a `==` at each site and each
    plant is killed by a named test (§13.5).
24. `POST /v1/auth/admin-session` mints `admin` plus the three messaging
    scopes, honours a `scopes` narrowing, rejects a widening, and writes
    `auth.admin_session_minted`; a wrong secret writes
    `auth.admin_secret_failed` and neither audit row contains the presented
    value. Six wrong secrets in 15 minutes from one source trips the §12.3
    cooldown, which survives a restart.
25. `POST /v1/auth/refresh` is subject to the §9.8 durable budget: 30 invalid
    presented tokens in 15 minutes from one source trips a cooldown that
    survives a restart, and a `429` is indistinguishable between a real and a
    guessed token.
26. `--json` puts exactly one JSON value on stdout and everything else on
    stderr, for every command, asserted by parsing stdout as JSON.
27. Every `/v1` route parameter is reachable from a CLI flag, and every CLI
    command maps to a route — asserted by a table test over both inventories,
    so a route added without a flag fails.
28. `agm messages delete` and `agm conversations delete` print the `effect`
    string **taken from the route's response**, and refuse without `y` or
    `--yes`; the printed string equals the response field byte for byte.
29. Trusted-proxy resolution: with the CIDR list set, a forged
    `X-Forwarded-For` from an untrusted peer is ignored; from a trusted peer
    the **rightmost** non-trusted entry wins; an unparseable entry stops the
    walk at the TCP peer; an invalid CIDR list refuses to start.
30. Rate limits: exceeding each bucket of §12.3 is `rate_limited` with
    `Retry-After`, and a session holding all four scopes gets one allowance per
    surface, not four.
31. **Signing out keeps history and refuses writes.** With two fake accounts,
    `POST /v1/accounts/{a}/sign-out` shreds `sessions/{a}.enc`, sets
    `state='signed_out'` and deletes **zero** rows; `GET /v1/conversations`,
    `list_messages` and `search_messages` still return account `a`'s history;
    every write naming `a` is `unsupported_capability` with
    `reason: "not_signed_in"` — **not** `not_paired`; account `b` is
    unaffected and still sends.
32. **Re-pairing resumes without duplicates.** After that logout, pair `a`
    again with the same address: the same `acct_` ID is reused, no second
    account row appears, a full backfill and sweep run, and the message and
    conversation counts are **unchanged** — every re-derived ID equals the
    stored one. Messages that arrived while it was signed out are ingested
    once.
33. **Remove purges, and only remove.** `DELETE /v1/accounts/{a}` without
    `{"confirm": true}` is `invalid_request` and deletes nothing. With it, all
    of `a`'s conversations, messages, attachments, reactions, contacts,
    operations and `backfill_state` are gone, its cached media files are
    unlinked after the commit, account `b` is untouched, the response carries
    the row counts and the effect sentence, and the `account.removed` audit
    row **survives** carrying `account_id`.
34. **Ambiguity is an error that can be acted on.** With two accounts, a write
    omitting `account_id` is `invalid_request` with
    `details.field = "account_id"` and `details.accounts` listing both as
    `{id, google_account, state}`; retrying with one of those IDs succeeds.
    With exactly one account, the same call omitting it **succeeds**. A read
    omitting it returns both accounts' rows. A `conv_` ID paired with the
    wrong `account_id` is `invalid_request` naming both, never `not_found`.
35. **Idempotency is per account.** The same `client_request_id` sent to two
    accounts creates two operations and calls each backend once.
36. **`agm accounts remove` is the *only* purge.** Each of signing out,
    a `RevokePairData` from the phone, cookie expiry
    (`GaiaLoggedOut`), `account_changed`, an abandoned re-pair and
    `accounts.max_concurrent` parking deletes **zero** conversation, message,
    attachment, reaction, contact and operation rows — asserted by row counts
    before and after each. This is the negative half of D30 and test 33 is the
    positive half.
37. **`accounts.max_concurrent` bounds concurrency and is diagnosable.** With
    it set to 1 and two accounts paired, exactly one is `connected` and the
    other is **`parked`** with `state_reason: "capacity"` — not `degraded` and
    not `error`; the parked account is fully readable and its writes are
    `not_signed_in`; raising the setting connects it without a restart.
38. **A pairing that never completes leaves nothing behind.** Start a pairing,
    then cancel it, let it time out, and kill the process mid-pairing. In each
    case no `acct_` row survives, no session file is written, and — critically
    — a subsequent write with one real account still succeeds **without**
    `account_id`, proving the abandoned row never tripped the §7.3 ambiguity
    rule. A pairing whose `Mobile.SourceID` is empty is `pairing_no_account`
    and likewise creates nothing.
39. **Every advertised listing is indexed in both forms.** `EXPLAIN QUERY PLAN`
    over each list and search route, with and without `account_id`, shows an
    index for every filter combination the route advertises — no `SCAN` of
    `conversations`, `messages`, `participants` or `contacts`.
40. **Rate limits do not multiply with accounts.** One authorization sending to
    two accounts shares one mutation bucket (§12.3).
41. Sentinel secrets — including each of the seven Google cookie values, **for
    both accounts**, and both session files, by
    name — appear in no log line, no audit payload, and nowhere in
    `agent-gm.sqlite3`, `-wal` or `-shm`, proven by `strings | grep`.
42. `PRAGMA foreign_key_check` is empty after every migration; a database at a
    higher `user_version` refuses to open, naming both numbers.
43. `POST /v1/admin/backup` produces a file that opens standalone, and pruning
    keeps exactly `backup.keep` and audits each removal.
44. `no-real-numbers` passes over the whole tree.
45. **Live gate.** `agm pair` from a clean data directory, then
    `agm conversations list`, then `agm messages send` one text to
    `<APPROVED_DIRECT_NUMBER>` with `--wait --wait-for sent`, then
    `agm messages send --file` of a small JPEG to the same number, then the
    owner replies and `agm messages list` shows it. Then `agm messages
    add-reaction` and `remove-reaction`. Then `agm messages delete` on Agent
    GM's own test message, with the owner confirming the effect sentence
    first. Then `agm conversations archive` and `unarchive`.
46. **Live gate.** The fallback path works from a clean data directory:
    `agm pair --paste`, fed a cURL command copied from devtools on stdin,
    reaches `connected`; and a paste missing `OSID` is refused with a message
    naming `OSID` and `messages.google.com` rather than a generic failure.
47. **Live gate.** `agm pair --refresh-cookies` re-authenticates the existing
    pairing **without** a re-pair and without a new emoji; a capture from a
    different Google account is `pairing_wrong_account` and changes nothing.
    In both cases the short-lived Chrome profile directory is gone after the
    command returns (D33), asserted on the filesystem.
48. **Live gate.** `agm conversations start <APPROVED_GROUP_NUMBER_1>
    <APPROVED_GROUP_NUMBER_2> --name "agent-gm test"` creates a group. If it
    fails, the failure is diagnosed against the §15.4 runbook rows
    (group-MMS setting, `config_version_stale`, `google_undocumented_status`)
    before anything is changed.

### Slice 3 — OAuth and MCP

**Deliverables.** `internal/oauth` in full (§9); `internal/mcp` in full (§8);
the instructions block; `devbox run conformance` and its baseline; the name
lint and its meta-test; `docs/mcp.md`, `docs/oauth.md`.

**Acceptance tests.**

1. `issuer` equals `https://gm.agent-wx.app` **byte for byte** and `resource`
   equals it plus `/mcp` with no trailing-slash drift, asserted as string
   equality on both discovery documents.
2. An unauthenticated `/mcp` request answers `401` with the exact
   `WWW-Authenticate` of §9.2; a valid token carrying no messaging scope
   answers `403 insufficient_scope` with the same challenge.
3. DCR: `token_endpoint_auth_method` other than `none` is refused; a
   client-chosen `client_id` is refused; 11 redirect URIs are refused; a
   501-character URI is refused; `http://localhost:1/cb` is **accepted** and
   `http://localhost.evil.example/cb`, `http://notlocalhost/cb`,
   `http://local.host/cb`, `http://localhost@evil.example/cb` are each
   refused; a registered `http://127.0.0.1/cb` matches
   `http://127.0.0.1:53211/cb` but not `http://127.0.0.1:53211/cb2` and not
   `http://127.0.0.2/cb`.
4. Authorize: an unknown client and an unregistered redirect answer `4xx` and
   **do not redirect**; every later failure redirects with `error`, `state`
   and a byte-exact `iss`. Omitting `code_challenge_method` is
   `invalid_request`, never defaulted to `plain`. A `resource` other than the
   canonical one is `invalid_target`. `scope=admin` is `invalid_scope`.
5. An invalid enrollment code re-renders the form with **one generic message,
   byte-identical** for unknown, expired, revoked and consumed codes, creates
   no pending request, and consumes nothing.
6. The eleventh failed code attempt in 15 minutes is `429` with `Retry-After`,
   per signed context **and** per source, and loading a fresh authorization
   page does not reset the source bucket.
7. `/oauth/requests/{id}` and `/status` answer `404` without the context
   cookie.
8. A replayed authorization code is `invalid_grant` **and revokes the tokens
   the first exchange produced**. A wrong PKCE verifier is `invalid_grant`
   **and consumes the code**.
9. Refresh rotates; reusing a spent refresh token revokes the family and the
   authorization in one transaction, with its audit row; a widening `scope` is
   `invalid_scope` and **does not spend** the presented token.
10. `/oauth/revoke` always answers `200`, including for a token belonging to
    nobody, and never confirms existence.
11. Unauthenticated budgets: 30 invalid presented tokens in 15 minutes trips a
    durable cooldown that survives a restart; a successful presentation does
    not clear the counter; the token is looked up read-only before any write
    transaction opens.
12. The enrollment code's plaintext appears nowhere in the database file, the
    WAL, the shm, or a log; the stored hash equals SHA-256 of the canonical
    form **computed outside the codebase**.
13. An OAuth authorization can never hold `admin`, and an admin session can
    never be minted through `/oauth/token`; the two credential paths do not
    cross (§9.6).
14. `tools/list` under `messages:read` returns exactly the **eleven** read tools and
    **no** write or delete tool; under `messages:write` exactly the eight
    more; under `messages:delete` exactly the two. `tools/call` on a
    non-visible name is refused again at call time.
15. Every tool has a description; **every argument has a description**; every
    schema is `additionalProperties: false`; every write tool requires
    `client_request_id`; every write tool's description ends with the
    fresh-key sentence of §8.2. Asserted by fetching `tools/list` from a
    running server and counting, so a stale description fails the claim.
16. A two-way table test over the route inventory and the MCP surface asserts
    §8.2's rule exactly, with **three** categories: every `/v1` route whose
    scope is `messages:read`, `messages:write` or `messages:delete` is served
    as a **tool**, as a **resource**, or as a **named exclusion**; every
    `admin`-scoped route and every credential route (`/v1/auth/*`) is served as
    none of the three. A tool with no route, or a messaging route in none of
    the three categories, fails it.
17. **Multi-account MCP.** With two accounts: `list_accounts` returns both with
    their states; a read tool omitting `account_id` covers both and every row
    carries its `account_id`; a **write** tool omitting it returns
    `isError: true` with `details.accounts` naming both, and retrying with one
    of those IDs succeeds; `get_session` works with and without `account_id`
    under the §7.3 rule; `get_health` returns two account rows and an
    `accounts_summary`. With **one** account, every one of those calls
    succeeds with `account_id` omitted.
18. The §9.4 authorization screen renders the global-scope disclosure line
    verbatim, above the scope checkboxes, followed by the current accounts —
    asserted as a string, which is what makes §9.7's honesty claim testable.
19. A domain failure is a **result** with `isError: true` and
    `structuredContent.error`, not a JSON-RPC error; an unknown tool name
    **is** a JSON-RPC error. Both directions asserted.
20. The `initialize` instructions block is byte-identical to the "First five
    minutes" section of `docs/mcp.md` modulo markdown quoting.
21. `Origin: https://evil.example` on `/mcp` is `403` before parsing; no
    `Origin` is accepted; two `Authorization` headers are refused rather than
    resolved.
22. `get_attachment` returns inline image content under
    `media.inline_mcp_image_max_bytes` and a resource link above it, **and the
    download ticket either way**; the first content block is the text summary.
23. `devbox run conformance` runs against a token minted through the **whole**
    OAuth flow, passes its baseline in both directions, and fails if the
    chosen spec revision runs zero scenarios.
24. `devbox run lint-names` fails on each banned word in a served tool
    description and in a `docs/` page, and its meta-test proves the exempt
    cases — §18.3, a fenced block quoting upstream, `send_mode`,
    `search.mode` — still pass.
25. **Live gate.** A real MCP client completes discovery, dynamic
    registration, the authorization screen with an enrollment code, owner
    approval and the token exchange against a **temporary public URL**
    (`cloudflared tunnel --url` against the locally built binary — this needs
    no image and no deployment, which is why the connector gate is Slice 4).
    It then calls `list_conversations`, `get_message` and `get_health` against
    the owner's real account, and `send_message` one text to
    `<APPROVED_DIRECT_NUMBER>`. The owner then revokes the authorization and
    the next call is `401`.

### Slice 4 — packaging and the connectors

**Deliverables.** The Dockerfile, `compose.example.yml`, the release workflow,
the GHCR push, the npm wrapper, `docs/deploy.md`, `CHANGELOG.md`.

**Acceptance tests.**

1. The image builds for `linux/amd64` and `linux/arm64`, runs as `nonroot`,
   and `agent-gm healthcheck` succeeds inside it with no shell and no `curl`.
2. `docker compose -f compose.example.yml up` on a clean machine with only the
   three required environment variables reaches `GET /healthz` = 200.
3. `GET /v1/health` and MCP `serverInfo` report a `commit` equal to the built
   commit and a `source_url` that resolves — the AGPL §13 obligation of §1.4.
4. CI cross-compiles all six release binaries and asserts each runs
   `agm version` under `qemu` where the architecture allows; the two darwin
   archives are checked for architecture and dynamic-link correctness with
   `file` and `otool -L` equivalents, since there is no macOS runner. Darwin
   execution is covered by test 9.
5. The wrapper verifies the downloaded binary against the `checksums.txt`
   **pinned inside the npm tarball**; a tampered download fails install with a
   message naming the file; `AGENT_GM_CLI_SKIP_DOWNLOAD=1` skips cleanly and
   `AGENT_GM_CLI_BINARY` points at an existing binary.
6. An unsupported platform fails `postinstall` with a message naming the
   platform, rather than installing something that cannot run.
7. Exit codes survive the npm shim: `agm --nonsense` exits `2` through npm as
   it does natively; a `not_found` exits `5`.
8. The GitHub release carries all six archives, `checksums.txt` and a valid
   cosign signature; the GHCR digest is recorded in the release notes together
   with the `libgm` pin.
9. **Live gate.** The owner installs the CLI on their Mac, runs
   `agm auth login` against `https://gm.agent-wx.app`,
   `agm conversations list`, and `agm messages send` one text to
   `<APPROVED_DIRECT_NUMBER>`.
10. **Live gate.** claude.ai and ChatGPT each add
    `https://gm.agent-wx.app/mcp` as a connector against the **deployed**
    container, complete enrollment and approval, list conversations, read a
    message, and send one text to `<APPROVED_DIRECT_NUMBER>`. Each is then
    revoked from `/v1/admin/authorizations` and the connector reports a
    failure rather than silently continuing.

## 17. The agent team

| Slice | Implementer | Reviewer | Why |
|---|---|---|---|
| 1 spike | Opus | Opus | Reading a pinned library correctly; small and concrete |
| 2 store, REST, CLI, media | Opus | Opus | Large but fully specified. Escalate to Fable if the crash-recovery or media-ticket work reports a contract conflict |
| 3 OAuth and MCP | Opus | **Fable** | Security invariants that fail silently. The reviewer must be able to reason about the replay, rotation and audience rules |
| 4 packaging and the connectors | Opus | Opus | Mechanical, but it owns the claude.ai/ChatGPT live gate against the real deployment |

Roles are `.claude/agents/implementer.md` and `.claude/agents/reviewer.md`.
Both were rewritten for Agent GM alongside this spec: they name this file as
the contract, carry the pin rule, the never-send-live rule, the plant
discipline, and the standing prohibitions below.

**Protocol.** The implementer commits small reviewable slices on a working
branch and sends the reviewer a message with the commit SHA, what it covers,
and how to run its tests. The reviewer syncs its own worktree to that SHA with
`git fetch . <branch>` and `git reset --hard <sha>`, runs everything itself,
and replies with findings. The implementer acts on findings directly. Both
message the coordinator only for spec conflicts, blocked decisions, and slice
completion.

**Live gates involving a second Google account** need the owner to sign into
it. An implementer never adds an account to the owner's deployment; the
two-account tests of §16 run against two **fakes**, which is what §13.1's
one-fake-per-account rule exists to make possible.

**Live sends are the coordinator's alone.** Neither the implementer nor the
reviewer sends a message to a real phone number, ever, for any reason. An
agent that believes it needs a live send says so and stops. The approved
numbers of §13.3 exist so that when the coordinator does send, the target is
never in question.

**No agent writes a real phone number into this repository.** Not in a test,
not in a fixture, not in a doc, not in a commit message, not in a comment. The
placeholders `<APPROVED_DIRECT_NUMBER>`, `<APPROVED_GROUP_NUMBER_1>` and
`<APPROVED_GROUP_NUMBER_2>` are used everywhere; the real values live in the
operator's private notes and reach a live test only through
`AGENT_GM_LIVE_NUMBERS` or the untracked `testdata/live-numbers.local`. This
repository is public. The `no-real-numbers` CI job enforces it, and a commit
that trips it is reverted rather than amended.

**Standing prohibitions**, inherited and non-negotiable: never touch
`/home/nick/code/agent-mx-trial`, port `8787`, `127.0.0.1:8008`,
`/home/nick/code/openclaw-custom/.env`, or `openclaw-custom/matrix`. Never
read the owner's `.env`, 1Password, or any credential store. Never kill a
process by name pattern — use `pkill -x agent-gm` or a PID from `pgrep -x`,
never `pkill -f`, which matches the calling shell. Work only through Devbox;
add missing tools to `devbox.json` rather than installing on the host.

---

## 18. Decisions and open questions
### 18.1 Decisions

| # | Decision | Because |
|---|---|---|
| D1 | Import `libgm` directly; no bridge, no Matrix | The owner talks to nobody else on the server and wants no human UI. Everything Matrix bought — federation, other users, E2EE between clients — is cost with no benefit here |
| D2 | AGPL-3.0-or-later, public repository | `libgm` is AGPL and no upstream exception covers Agent GM. §1.4 |
| D3 | Pin `libgm` by commit `be48a58`; bump only as a deliberate slice; surface both ConfigVersions in health | See the field observation below |
| D4 | No outbox; sends are synchronous | `SendMessage` returns the phone's own answer within 60s. One system of record needs no reconciliation |
| D5 | `ErrPhoneNotResponding` → operation `pending`, HTTP 504, never `failed` | Upstream says the server accepted it and the phone may still act (`session_handler.go:20-24`). Calling it a failure invites a duplicate text to a real person |
| D6 | `account_id` is in every **root** ID derivation | A re-pair of the same Google account — even onto a different phone — keeps every ID; a different account can never collide. `att_`, `react_` and `part_` inherit it through their parent |
| D7 | Idempotency key **required** on every mutation | Retries are the normal case for an agent, and a duplicate SMS is not recoverable |
| D8 | Session in an encrypted file, not in SQLite | Different write cadence, must survive a database restore-from-snapshot, must never be reachable by a query |
| D9 | The data key is not rotatable | Rotation means re-encrypting the session and every attachment key atomically across a restart. Not worth it for one user; documented instead |
| D10 | One `gm.Backend` interface with exactly two implementations, and **everything the API promises reachable through it** | Testability, not extensibility. A second *production* implementation is forbidden by N3. If a field cannot be served through the interface it cannot be served at all |
| D11 | OAuth 2.1 even for one user | claude.ai and ChatGPT connectors require it |
| D12 | `http://localhost` redirect URIs accepted, against RFC 8252 §8.3 | Widely used MCP clients register the name. Exact case-insensitive host match only |
| D13 | Media by ticket and `curl`, never base64 through the model | An MCP client cannot attach a file, and a megabyte of base64 in a prompt is not acceptable |
| D14 | Only Google's delete-for-me | It is the only delete `libgm` exposes. Every surface says so in the same words |
| D15 | Typing is REST-only, no MCP tool | No lasting effect, no result an agent can act on. The two other tool-less routes are named in §8.2 with their reasons |
| D16 | Deployment integration (Compose service, tunnel config) stays in `openclaw-custom` | This repository must be usable by any host |
| D17 | The npm package is a downloading wrapper, not a bundle | Six platform binaries in one tarball is 100 MB+ for a CLI. Checksums are pinned into the tarball so a compromised release page cannot re-target an old version |
| D18 | The interface-layering rubric is reduced to one axis | With no Matrix and no second provider, axes A and B are meaningless |
| **D19** | **The Google-account (gaia) flow is the only pairing path.** `agm pair` launches system Chrome with a dedicated profile, reads the seven cookies over CDP, starts gaia pairing and prints the emoji; `agm pair --paste` is the fallback. **QR pairing is removed: Google Messages no longer supports it.** Owner decision, 2026-09-06 | Google withdrew the QR device-pairing option, so there is nothing to choose between. Corroborating the withdrawal from the pinned tree: `events.QR` exists at `events/qr.go:7` but has **zero emitters** at `be48a58`, and the QR payload was only ever a return value of `StartLogin`/`RefreshPhoneRelay` — the reviewer's F-3, which flagged that Agent GM would have waited on an event that never fires. Consequence, stated wherever it matters (§3.2, §11.4, §12.1, §15.2): each account's session file holds **live Google account cookies** for the life of the pairing (`http.go:58`, `client.go:73-81,407`) — the whole Google account, not a scoped token. There is no lower-privilege alternative to plan for, only handling rules to follow. Cookie expiry does **not** force a re-pair (`connector/login.go:246-289`), which is what makes a single flow sustainable |
| **D20** | **OAuth client resolution is DCR-only** | Agent MX had three tiers — preregistered, Client ID Metadata Document, DCR — and an SSRF-safe outbound fetcher to support the middle one. Dropping CIMD removes all outbound HTTP from the OAuth layer, which eliminates the SSRF surface rather than defending it. Every client this server will see registers itself |
| **D21** | **`conv_` is the conversation prefix** | Agent MX retired `conv_` for `room_` because a conversation there was a *facet of a Matrix room*. Here there is no room and a conversation is the primary noun, so `conv_` is simply correct. This is a reversal of an Agent MX decision, recorded so it is not read as an oversight |
| **D22** | **`tmp_id` is a bare UUID, not the operation ID** | `util.GenerateTmpID()` is `uuid.NewString()` with the comment *"Matches what the native app does"* (`util/func.go:9-12`). A prefixed opaque ID would diverge from every other client of this protocol for no benefit |
| **D23** | **One reaction per person per message** | Google's picker is single-select and `SendReactionRequest_SWITCH` exists precisely to replace (`gmproto/client.proto:305-317`). A schema permitting several would be modelling something Google does not do |
| **D24** | **Download tickets are stateful rows** | A five-redemption cap cannot be enforced by a signed blob. The token value is still signed and only hashed at rest |
| **D25** | **The admin bootstrap session carries the three messaging scopes** | The owner presenting `AGENT_GM_ADMIN_SECRET` is the person the service belongs to. A credential that could administer the server but not read a message would be useless, and Slice 2 could not test itself |
| **D26** | **The live event stream is treated as lossy; a reconciliation sweep is mandatory** | `deduplicateUpdate`'s callers `return` out of the batch loop on a hit (`event_handler.go:263-266,272-275`), abandoning every remaining part. That is message *loss*, and no local dedup recovers it |
| **D27** | **Agent GM is multi-account.** One owner, N Google accounts, each with its own `libgm` client, session file, event stream, backfill and sweep, all concurrent. **Owner decision, 2026-09-06** | The premise that one owner means one account was never argued, only assumed — an owner with a personal and a work Google account has two phones and wants both here. Nothing in `libgm` is a singleton: a `Client` is constructed per `AuthData` (`client.go:163`), so N clients is the library's own shape rather than a workaround. The cost is a column, a selection rule (§7.3) and a supervisor (§4.7); the alternative was N servers, N tunnels, N OAuth registrations, and an agent that cannot see across them |
| **D28** | **The account identifier is `AuthData.Mobile.SourceID`**, lowercased — the Google account address — hashed into `acct_` (§4.1) | It is the only field at `be48a58` that identifies an *account* rather than a device or a session. Upstream compares it against `Config.GetDeviceInfo().GetEmail()` to decide whether a re-authentication is the same account (`connector/login.go:267-270`) and lowercases it at sign-in (`pair_google.go:102-105`). `DestRegID`, `SessionID`, `PairingID`, `Browser.SourceID` and `FinishGaiaPairing`'s return are all device- or session-scoped; §3.2 tabulates why each is unusable. Hashing keeps the address out of IDs and URLs |
| **D29** | **OAuth scopes are global across accounts; per-account scoping is deferred** | A scope grammar naming accounts needs accounts to exist before a token is issued, an account picker on the authorization screen, and enrollment ceilings that can name accounts that do not exist yet. None of that is worth building before the owner has met a case for it. The honest statement is made where it matters (§9.7, and the authorization screen): a token reads and sends as **any** account. An owner needing real separation runs a second Agent GM |
| **D30** | **Signing out keeps history; only `agm accounts remove` deletes** | Losing access to an account is common — cookies expire, a phone is replaced — and losing years of searchable history because of it would be a disaster with no upside. Splitting the two makes deletion an explicit, confirmed, audited act, and makes re-pairing free: IDs derive from the account and Google's own stable IDs, so resuming reconciles rather than duplicating (§4.7) |
| **D31** | **`agm pair` prints the chosen device's `dest_reg_uuid` and nothing more: no last-seen timestamp, and no `details.device_count` on `pairing_init_timeout`.** Recorded 2026-09-06, from the Slice 1 review | §3.1 mandates `DoGaiaPairing`, which runs `StartGaiaPairing` and `FinishGaiaPairing` back to back and returns neither the `*PairingSession` nor the candidate list. The chosen device's `LastSeen` and the number of candidates reach only an upstream log line (`pair_google.go:352-370`), and `ErrHadMultipleDevices` is wrapped with `fmt.Errorf("%w (%w)", …)` carrying no count (`pair_google.go:391-397`, fixture assertion 16). `AuthData.DestRegID` *is* set before the emoji callback fires, so the UUID is available and is what §3.2 asks to be recorded. The alternative — driving `StartGaiaPairing`/`FinishGaiaPairing` directly — would forfeit the "`DoGaiaPairing` reconnects in its own goroutine" behaviour §3.1 says Agent GM depends on and must not re-implement, for two diagnostic fields |
| **D32** | **`config_version_stale` is a diagnosis, not a fault.** `GET /v1/health` and `agm health` report it as a boolean fact about the account; it is an *error code* only when a conversation-creating call has actually failed (§3.7, §7.2). A stale version with everything working is reported and nothing more. Recorded 2026-09-06, from the Slice 1 live gate | The live gate saw `config_version_stale: true` (live `2026.9.3.4.6` against the pin's `2026.9.2.4.6`) while pairing, listing, sending and the echo all worked. Treating the version difference as an error would have failed a healthy deployment; treating it as invisible would have hidden the one diagnosis §15.4 gives for a conversation-creating failure. So it is surfaced, it does not change `status`, and §15.5 says what an operator does about it: nothing, until a create fails, and then a pin bump (§3.6) |
| **D33** | **The pairing Chrome profile is short-lived.** It is created fresh under the platform's temporary directory for one capture and deleted the moment Chrome closes — on success, on failure, on timeout and on Ctrl-C. There is no kept profile, nothing under `$XDG_STATE_HOME/agent-gm/`, and no `--forget-browser`. **Owner decision, 2026-09-06** | A kept profile is a directory holding a live, logged-in Google session sitting on the client machine indefinitely, protected by nothing but its mode — the same blast radius as `sessions/*.enc` but with no data key in front of it, and easy to forget about. The benefit it bought was a `--refresh-cookies` that usually needed no sign-in; the owner judged a sign-in prompt per refresh to be cheap next to a permanent credential on disk. Keying it by account, and the `--forget-browser` command that existed to clean it up, both go with it |

#### Field observation behind D3 — the ConfigVersion, and status 4

This is **not** a claim about the pinned library, and §3 does not make it one.
It is what happened to the owner's deployment, recorded so D3 is not a rule
without a reason:

> On **2026-09-05**, running the mautrix-gmessages `v26.08` image — whose
> `pkg/libgm/util/config.go` hardcoded `ConfigVersion` **2026-03-18**, about
> six months stale — every attempt to create a group conversation came back
> from Google as `GetOrCreateConversationResponse.status = 4` with no
> conversation body, while existing conversations kept working. The real
> Google Messages web client, captured at the same time, was sending
> `2026-09-03`. Replacing the image with a build of upstream at `be48a58`
> (`ConfigVersion` 2026-09-02), with no other change, made the same group
> creation succeed end to end.

The proto declares no name for `4` at any commit, and nothing upstream
documents it, so:

- **§3.7 does not say "status 4 means a stale ConfigVersion."** Any
  non-`SUCCESS`, non-`CREATE_RCS` value is `google_undocumented_status`,
  reporting the bare integer and claiming nothing.
- **`config_version_stale` is detected by the version diff alone** — a
  conversation-creating call failed *and* compiled ≠ live. It never keys on a
  particular status number.
- `GET /v1/health` reports both versions unconditionally, so the diff is
  visible before anything fails.

#### The single acceptance axis

A reviewer scores each public surface 0, 1 or 2, and a slice is not accepted
below 2:

> **Every noun and verb on the surface is a Google Messages term used with its
> Google Messages meaning.** A *conversation* is a thread; a *contact* is
> somebody in the phone's contacts; *RCS* and *SMS/MMS* mean what Google means
> by them; *delete* means delete-for-me and the surface says so in the same
> words everywhere; *sending*, *sent*, *delivered* and *read* are the states
> Google reports and nothing is claimed that Google did not say. No Matrix term
> survives; no SQLite or `gmproto` internal reaches a public surface; and no
> name asserts a Google behaviour that does not exist. A reviewer scoring below
> 2 names the exact route, tool or command that fails and the smallest change
> that would fix it.

### 18.2 Open questions for the owner

**Owner answers, 2026-09-06 ("lgtm").** Every question below is now decided;
the table is kept as the record of what was asked.

| # | Decision |
|---|---|
| OQ-1 backfill depth | Keep the defaults: 2000 messages per conversation, 365-day horizon, applied per account |
| OQ-2 pruning | None. Revisit when the database is measurably large |
| OQ-3 OAuth approval | CLI only (`agm admin authorization-requests approve`). No approval page in v1 |
| OQ-4 npm | Publish as `@agent-gm/cli` under the `agent-gm` org; publish from CI only |
| OQ-5 GHCR image | Public |
| OQ-6 concurrent accounts | Default `accounts.max_concurrent = 8`; extra accounts `parked` |
| OQ-7 outage notification | No notification channel; state is visible in `agm health` and `GET /v1/health` |
| test numbers / group | The three approved numbers stand (private notes); the live-gate group is kept as a standing test thread |
| phone Group messaging | Owner confirms it is set to MMS |

The owner also asked that work be published to GitHub continuously: every
accepted slice is pushed, and implementers push their branch at the end of
each working session rather than only at review time.


Seven questions remain. Everything the first review could answer from the
pinned source has been answered and moved into §18.1 or §18.3.

The multi-account decision of 2026-09-06 (D27) resolved the largest open
premise in the spec, and narrowed OQ-1: backfill depth is now a per-server
setting applied to **each** account, so the cost of a generous horizon is
multiplied by the number of accounts.

| # | Question | Blocks | Why it matters |
|---|---|---|---|
| **OQ-1** | **How much history should the initial backfill pull?** `backfill.max_messages_per_conversation` defaults to 2000 and `backfill.horizon` to 365 days. Everything the phone will give, or a bounded window? | Slice 2 | Changes backfill from minutes to hours and the database from tens of MB to hundreds |
| **OQ-2** | **Should anything ever be pruned?** Today nothing is: messages, attachments metadata and audit rows accumulate for ever. Is a retention policy wanted, and over what? | Slice 2 | Cheap to leave alone, but it is a decision rather than an oversight only if it is made |
| **OQ-3** | **Who approves an OAuth authorization request, and how?** The design has the owner running `agm admin authorization-requests approve` on a terminal. Is that acceptable, or is an owner-facing approval page wanted — one that could be opened on a phone? | Slice 3 | An approval page is a human UI and contradicts non-goal N2. It is buildable, but it needs an explicit exception recorded here rather than an implementer deciding it |
| **OQ-4** | **Publish the CLI to npm, and under what name?** `@agent-gm/cli` is assumed throughout §14.3. Is the scope available, and is public publication wanted at all? | Slice 4 | If the scope is taken the name changes in the wrapper, the docs and the README. If publication is unwanted, the whole wrapper is dead work and the CLI ships as a release archive only |
| **OQ-5** | **Should the GHCR image be public?** AGPL §13 is satisfied by serving the source URL (§1.4), not by a public image, so this is preference, not obligation | Slice 4 | A private image means the deployment needs a pull secret |
| **OQ-6** | **Should `accounts.max_concurrent` (default 8) ever be lower?** Every `connected` account holds a long poll, an ingest goroutine and a sweep timer. How many accounts does the owner actually expect? | Slice 2 | The default is a guess; if it is two, the setting is noise, and if it is twenty, the backfill scheduling in §4.7 needs more thought |
| **OQ-7** | **What should happen when a phone is unreachable for a long time?** `pending` operations become `unknown` after 24 hours and nothing tells anyone. Is an alert wanted, and through what channel? | Slice 2 | There is deliberately no notification channel in this design; adding one is a new dependency |

| **OQ-7** | **Should `backfill.horizon` and `media.cache_max_bytes` become per-account?** Both are server-wide today (§15.1): the horizon is *applied to* each account, so its cost multiplies, and the media cache is one cross-account LRU, so a busy account can evict a quiet one's thumbnails. Per-account values would be more predictable and more configuration to keep straight | Slice 2 | Only matters once there are several accounts of very different sizes, which the owner knows and the spec does not |

Two smaller confirmations, not blockers:

- **The approved test numbers.** Confirm the three values, and whether the
  group created by the Slice 2 live gate should be deleted afterwards or kept
  as a standing test thread. The values themselves never enter this repository
  (§13.3).
- **The phone's *Group messaging* setting.** Confirm it is set to MMS. Note
  the claim has been softened: nothing in the pinned tree says a phone set to
  "send an SMS reply to all recipients" *cannot* create a group —
  `CreateGroup` fails only on fewer than two participants or a missing
  conversation body (`connector/startchat.go:181-233`) — so §11.4 now says a
  group send may fan out as separate SMS threads, which is what the setting
  actually controls.

#### Answered by the first spec review, from the pinned source

Recorded so they are not re-asked:

- *Which pairing flow is primary?* There is only one — the Google-account
  flow; the alternative was withdrawn upstream (D19, owner decision
  2026-09-06). The premise of the original question was wrong in any case:
  cookie refresh **does not** require a re-pair.
- *Must the owner give up Google Messages for web?* **No.** Nothing at the pin
  describes a paired-device slot or eviction. Concurrent use causes
  `BROWSER_INACTIVE`/`BROWSER_ACTIVE` flapping and extra resyncs — bandwidth,
  not the pairing (§3.2).

### 18.3 What was dropped from Agent MX, and why

Recorded so nobody re-derives it. These names may appear in this section as
history; the name lint of §13.5 exempts this file for exactly that reason.

| Dropped | Reason |
|---|---|
| Rooms, portals, portal generations, stale-portal detection, spaces | No Matrix. Google conversations are the only container, and they are `conv_` again (D21) |
| Invitations and `auto_accept` policy | Nothing invites anybody |
| E2EE, cross-signing, key backup, room-key import, `decryption_pending` | No Matrix crypto. Google's transport crypto is `libgm`'s business |
| Sync tokens and the two-database checkpoint | One event stream, one database |
| **`GET /v1/events` (SSE) for messages, its replay ring and cursor** | Agents poll; nothing here needs a durable event stream, and a replay ring is a second system of record. `GET /v1/accounts/{account_id}/events` survives as a **state**-only stream with no replay and no cursor, because `agm session --watch` needs it and it carries no message data |
| **Client ID Metadata Documents and three-tier client resolution** | D20 |
| Group-start choreography and `conversation_start_progress` | `GetOrCreateConversation` is one call |
| The provider layer, `provider=none`, the base/provider capability split | One network, forever |
| The outbox, its retry schedule, its deferral counter, its reconciliation | D4. Note the *reconciliation sweep* of §5.4 is unrelated: it repairs a lossy read stream, not a write queue |
| `redact` vs `delete_remote_message`, and every delete `mode` | D14 |
| `matrix_unavailable`, `/readyz` blockers, `/v1/admin/matrix/*`, `/v1/admin/sync/*` | Nothing Matrix to be unavailable |
| **Error codes `authentication_required`, `operation_failed`, `history_incomplete`** | `authentication_required` folded into `invalid_token` (one answer to a bad credential); `operation_failed` is redundant now that an operation carries the real error object (§6.5); `history_incomplete` survives as a **warning** on list and search responses, not a code |
| **`--wait-for accepted`** | There is no `accepted` state, because there is no queue (D4). The default moved from `accepted` to `sent`, and `read` was added, because those are states Google actually reports |
| **QR device pairing** | Google Messages withdrew it (D19). Agent GM never shipped it; `libgm`'s `StartLogin`, `RefreshPhoneRelay` and `GenerateQRCodeData` are out of contract, and `events.QR` had no emitters at the pin in any case |
| **The single-account premise** | Never argued, only inherited from Agent MX's "one owner, one bridge login". D27 replaced it. What Agent MX called `account_id` — a column kept so a *later* multi-account service could exist without rewriting storage — is now load-bearing rather than speculative |
| Interface-layering axes A and B | D18 |
