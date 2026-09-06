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
access to **one Google Messages account**, paired directly with **one phone**.

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

- **G1 — One account, one phone.** Pairing is a first-class, documented, and
  recoverable operation (§3.2, §11.4). The binary refuses to hold more than one
  Google Messages session.
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
- **N4 — No multi-tenancy.** One owner. There is no user table, no per-user
  scoping in IDs, no tenant column. Authorization is a single boolean question
  ("is this token the owner's?") plus scopes.
- **N5 — No outbox, no reconciliation loop.** Agent MX needed an outbox because
  it had to reconcile two systems of record (Matrix and Google). Agent GM has
  one. Sends are synchronous (§6.1).
- **N6 — No delete modes other than Google's.** `messages.delete` maps to
  `libgm.Client.DeleteMessage`, which is Google Messages' own delete. There is
  no "delete for everyone", no local-only tombstone, no redaction vocabulary.
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
| `internal/gm` | the Google Messages adapter interface, its `libgm` implementation, and the fake | know about HTTP, SQLite, MCP or scopes |
| `internal/store` | SQLite: schema, migrations, all queries, the single writer | make network calls |
| `internal/core` | the operations the surfaces share: send, list, backfill, ingest, idempotency, delivery-status transitions | speak JSON or MCP |
| `internal/api` | REST handlers, DTOs, error envelope, strict parameter rejection, pagination | contain business logic |
| `internal/mcp` | MCP server, tool registry, schemas, instructions block, isError mapping | duplicate core logic |
| `internal/oauth` | OAuth 2.1 AS + protected-resource metadata, DCR, PKCE, enrolment, tokens | be reachable without TLS in production |
| `internal/media` | upload/download tickets, the media cache on disk, content sniffing, size limits | hold the store's write lock |
| `internal/cli` | `agm` subcommands, output formatting, exit codes, QR rendering | talk to `gm` or `store` directly — it goes through REST |
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

    // pairing
    StartQRPairing(ctx context.Context) (qrURL string, err error)
    RefreshQRPairing(ctx context.Context) (qrURL string, err error)
    StartGooglePairing(ctx context.Context, emoji func(string)) error
    Unpair(ctx context.Context) error

    // reads
    ListConversations(ctx context.Context, folder Folder, count int) ([]Conversation, error)
    GetConversation(ctx context.Context, convID string) (*Conversation, error)
    ListMessages(ctx context.Context, convID string, count int, cursor *Cursor) ([]Message, *Cursor, error)
    ListContacts(ctx context.Context) ([]Contact, error)
    ResolveConversation(ctx context.Context, numbers []string, groupName string) (*Conversation, error)

    // writes
    SendText(ctx context.Context, req SendTextRequest) (SendResult, error)
    SendMedia(ctx context.Context, req SendMediaRequest) (SendResult, error)
    React(ctx context.Context, msgID string, emoji string, action ReactionAction) error
    DeleteMessage(ctx context.Context, msgID string) error
    MarkRead(ctx context.Context, convID, msgID string) error
    DeleteConversation(ctx context.Context, convID, phone string) error

    // media
    Upload(ctx context.Context, data []byte, filename, mime string) (MediaRef, error)
    Download(ctx context.Context, mediaID string, key []byte) ([]byte, error)

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

The fake implementation (`internal/gm/fake`) is a deterministic in-memory
Google Messages: it accepts sends, generates message IDs, emits the same
event sequence a real phone would (§5.5), and can be scripted to fail with any
error in §3.5.

### 2.4 Concurrency model

- **One goroutine writes to SQLite.** All writes go through
  `store.Writer`, a single goroutine consuming a channel of write closures.
  SQLite is opened with `journal_mode=WAL`, `busy_timeout=5000`,
  `foreign_keys=on`, `synchronous=NORMAL`. Reads use a separate read-only pool.
- **One goroutine drains `gm.Events()`** and applies each event as a store
  write.
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
| `(*Client).FetchConfig` | `(ctx) error` | fetches Google's live `Config`, stores it on `c.Config`, and parses `DeviceInfo.DeviceID` into `AuthData.SessionID`. Logs (at trace) when the compiled-in `util.ConfigMessage` differs from live. Agent GM calls this once before `Connect` and surfaces the compiled/live version pair in `GET /v1/health` (§3.7). |
| `(*Client).Connect` | `() error` | refreshes the tachyon token synchronously so bad credentials surface immediately, then starts long polling. **This is the only connect path Agent GM uses.** |
| `(*Client).ConnectBackground` | `() error` | one-shot poll for push-woken processes. Agent GM does **not** use it. |
| `(*Client).Disconnect` | `()` | closes long polling and fails every in-flight response waiter with `ErrConnectionClosed`. |
| `(*Client).Reconnect` | `() error` | close + re-check login + restart polling. Agent GM calls this only from its own supervisor after `ListenFatalError` that is not a credential error. |
| `(*Client).IsConnected` | `() bool` | long-poll connection is non-nil. Upstream notes it is imprecise during reconnects; Agent GM treats it as advisory only. |
| `(*Client).IsLoggedIn` | `() bool` | `AuthData != nil && AuthData.Browser != nil && AuthData.HasCookies()`. This is the authoritative "are we paired" check. |
| `(*Client).CurrentSessionID` | `() string` | the session handler's current UUID; changes on every `SetActiveSession`. Recorded in the audit log on connect. |
| `(*Client).SetProxy` | `(string) error` | unused; Agent GM has no proxy support. |
| `(*Client).SetPingInterval` | `(time.Duration)` | clamped to `[1m, 4h)`. Agent GM leaves the default 1m. |
| `(*Client).SetDataReceiveCheckInterval` | `(time.Duration)` | intervals under 5m ignored. Agent GM leaves the default `DefaultBugleDefaultCheckInterval` = 2h55m. |

`libgm.AuthData` is the entire session. Its JSON tags are the on-disk format
Agent GM persists (§3.3). Fields: `RequestCrypto` (`*crypto.AESCTRHelper`),
`RefreshKey` (`*crypto.JWK`), `Browser` and `Mobile` (`*gmproto.Device`),
`TachyonAuthToken []byte`, `TachyonExpiry time.Time`, `TachyonTTL int64`,
`WebEncryptionKey []byte`, `SessionID`/`DestRegID`/`PairingID` (`uuid.UUID`),
`Cookies map[string]string`. `AuthData.IsGoogleAccount()` is
`DestRegID != uuid.Nil` and selects the `GDitto` network; QR pairing leaves it
`Nil` and uses the `Bugle` network (`util.QRNetwork` / `util.GoogleNetwork`).

#### Pairing — `pkg/libgm/pair.go`, `pkg/libgm/pair_google.go`

| Symbol | Signature | Returns / meaning |
|---|---|---|
| `(*Client).StartLogin` | `() (string, error)` | QR flow. Registers a phone relay, adopts the returned tachyon token, **starts a not-logged-in long poll goroutine**, and returns the QR payload string (`util.QRCodeURLBase` + base64 of a marshalled `gmproto.URLData` carrying the pairing key, AES key and HMAC key). |
| `(*Client).RefreshPhoneRelay` | `() (string, error)` | re-mints an expiring QR without restarting the flow; returns a new QR string. |
| `(*Client).GenerateQRCodeData` | `(pairingKey []byte) (string, error)` | the encoder behind both of the above. Not called directly. |
| `(*Client).DoGaiaPairing` | `(ctx, emojiCallback func(string)) error` | Google-account flow, start to finish: `StartGaiaPairing`, hand the emoji to the callback, `FinishGaiaPairing`, emit `events.PairSuccessful`, reconnect in a goroutine. **This is the call Agent GM uses for the Google-account flow.** |
| `(*Client).StartGaiaPairing` | `(ctx) (string, *PairingSession, error)` | requires cookies (`ErrNoCookies` otherwise); signs in, enumerates the account's devices, picks the single primary, returns the emoji to display. Exposed for the CLI when it wants to render the emoji before blocking. |
| `(*Client).FinishGaiaPairing` | `(ctx, *PairingSession) (string, error)` | completes UKEY2, derives the request-crypto keys, sets `AuthData.PairingID`, returns `"<mobile sourceID>/<destRegDevice int>"` as the phone ID. |
| `(*Client).Unpair` | `(ctx) error` | dispatches to `UnpairGaia` if cookies are present, else `UnpairBugle`. **The only unpair call Agent GM makes.** |
| `(*Client).PairCallback` | `atomic.Pointer[func(*gmproto.PairedData)]` | if set, `completePairing` calls it *instead of* emitting `PairSuccessful` and auto-reconnecting. Agent GM leaves it **nil** and consumes the event, so that the library's built-in 2-second settle-then-reconnect (see below) is used. |

Upstream behaviours Agent GM depends on and must not re-implement:

- After QR pairing, `completePairing` **sleeps 2 seconds before reconnecting**,
  with the comment that reconnecting too quickly makes the phone fail to
  recognise the session and unpairs the bridge (`pair.go:completePairing`).
  Agent GM does not reconnect on its own after `PairSuccessful`.
- `DoGaiaPairing` reconnects in its own goroutine on success.

Google-account pairing errors, all from `pair_google.go`, all mapped in §3.5:
`ErrNoCookies`, `ErrNoDevicesFound`, `ErrIncorrectEmoji`, `ErrPairingCancelled`,
`ErrPairingTimeout`, `ErrPairingInitTimeout`, `ErrHadMultipleDevices`.
`GaiaInitTimeout` is 20s.

#### Reads — `pkg/libgm/methods.go`

| Symbol | Signature | Returns |
|---|---|---|
| `(*Client).ListConversations` | `(ctx, count int, folder gmproto.ListConversationsRequest_Folder) (*gmproto.ListConversationsResponse, error)` | `.Conversations []*conversations.Conversation`, `.Cursor *Cursor`. **First call in a process sends `MessageType_BUGLE_ANNOTATION`, every later call `BUGLE_MESSAGE`** — the library tracks this with `conversationsFetchedOnce`. Agent GM must therefore call `ListConversations` at least once per process before relying on live conversation events. |
| `(*Client).GetConversation` | `(ctx, conversationID string) (*gmproto.Conversation, error)` | one conversation, already unwrapped from the response. |
| `(*Client).GetConversationType` | `(ctx, conversationID string) (*gmproto.GetConversationTypeResponse, error)` | used only to disambiguate SMS vs RCS when a conversation record is incomplete. |
| `(*Client).FetchMessages` | `(ctx, conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error)` | `.Messages []*Message`, `.TotalMessages int64`, `.Cursor *Cursor`. This is the backfill primitive (§5.2). |
| `(*Client).ListContacts` | `(ctx) (*gmproto.ListContactsResponse, error)` | the phone's contact list. Request constants are fixed upstream (`i1=1, i2=350, i3=50`). |
| `(*Client).ListTopContacts` | `(ctx) (*gmproto.ListTopContactsResponse, error)` | 8 most-contacted. Agent GM uses it for `contacts.list?top=true`. |
| `(*Client).IsBugleDefault` | `(ctx) (*gmproto.IsBugleDefaultResponse, error)` | `.Success` — whether Google Messages is the phone's default SMS app. Surfaced in health; a `false` here explains most send failures. |

`gmproto.ListConversationsRequest_Folder`: `UNKNOWN=0`, `INBOX=1`,
`ARCHIVE=2`, `SPAM_BLOCKED=5`. `gmproto.Cursor` is
`{lastItemID string, lastItemTimestamp int64}`.

#### Writes — `pkg/libgm/methods.go`

| Symbol | Signature | Returns |
|---|---|---|
| `(*Client).SendMessage` | `(ctx, *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error)` | `.Status` is `UNKNOWN=0, SUCCESS=1, FAILURE_2=2, FAILURE_3=3, FAILURE_4=4`. Upstream comments `FAILURE_4` as "not default sms app?". `.GoogleAccountSwitch` is set when the phone's active Google account changed underneath us. |
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
        TmpID:          txnID,   // Agent GM's operation ID, see §4.2
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
  set to the same value**, mirroring `connector/handlematrix.go:ConvertMatrixMessage`.
  Agent GM sets them to its operation ID, which is how the remote echo is
  correlated back to the operation (§6.3).
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

### 3.2 Pairing flows and what the owner does on the phone

Agent GM supports both upstream flows. The owner picks one at pair time; there
is no automatic fallback, because the two produce different `AuthData` shapes
(cookies vs no cookies) and different networks.

#### Flow A — QR pairing (`Bugle` network, no Google cookies)

1. Owner runs `agm pair --qr` (or `POST /v1/pairing/start {"method":"qr"}`).
2. Agent GM constructs a client with `NewAuthData()`, calls `FetchConfig`, then
   `StartLogin()`, which returns a QR payload string.
3. The CLI renders that string as a QR code in the terminal (§11.4). The REST
   response returns the string plus a PNG data URL.
4. **On the phone:** open Google Messages → profile/overflow menu → *Device
   pairing* → *QR code scanner* → scan the terminal. Google Messages must be
   the default SMS app for sends to work (`IsBugleDefault`, §3.1).
5. The library's already-running not-logged-in long poll receives a
   `BugleRoute_PairEvent` carrying `RPCPairData_Paired`; `completePairing`
   adopts the token, fills `AuthData.Mobile`/`Browser`, emits
   `events.PairSuccessful{PhoneID, QRData}`, sleeps 2s, and reconnects.
6. Agent GM persists `AuthData` on `PairSuccessful` (§3.3) and on every
   `events.AuthTokenRefreshed`.
7. QR codes expire. If the owner is slow, Agent GM calls `RefreshPhoneRelay()`
   every 60 seconds and pushes the new QR to the CLI, which redraws it.

The pairing session is **abandoned** if no `PairSuccessful` arrives within
`AGENT_GM_PAIRING_TIMEOUT` (default 5 minutes); Agent GM then discards the
half-built `AuthData` and returns `pairing_timeout`.

#### Flow B — Google-account pairing (`GDitto` network, requires cookies)

1. Owner supplies Google cookies for `messages.google.com` — at minimum
   `SAPISID`, plus the standard auth cookies — via
   `agm pair --google --cookies-file <path>`. The file is a JSON object of
   cookie name → value. Agent GM calls `AuthData.SetCookies` and **never logs
   or echoes it**; the file is read once and not copied.
2. Agent GM calls `DoGaiaPairing(ctx, emojiCallback)`.
3. `StartGaiaPairing` signs in, lists the account's devices, and selects the
   single device with `UnknownInt4 == 1`. If there is none, `ErrNoDevicesFound`;
   if there is more than one, `ErrHadMultipleDevices`.
4. The emoji callback fires with **one emoji character**. The CLI prints it
   very large and says: *"Your phone will show three emoji. Tap this one."*
5. **On the phone:** a Google Messages notification/dialog appears offering a
   choice of emoji plus a "this is not me" option. The owner taps the matching
   emoji within the timeout.
6. `FinishGaiaPairing` completes UKEY2, derives the AES/HMAC request-crypto keys
   (key-derivation version 0 or 1; version 1 concatenates the two UKEY2 keys in
   hash order and HKDFs them with the `Ditto salt/info` constants), sets
   `AuthData.PairingID`, and returns the phone ID.
7. Wrong emoji → `ErrIncorrectEmoji`; "this is not me" or dismissal →
   `ErrPairingCancelled`; expiry → `ErrPairingTimeout`.

Cookies **expire**. When they do, the long poll emits `events.GaiaLoggedOut`
(delivered as a `GET_UPDATES` data event whose unencrypted payload is exactly
`0x72 0x00` — upstream's `hackyLoggedOutBytes`). Agent GM marks the session
`logged_out`, stops polling, and every write returns `not_paired` until the
owner re-pairs. Refreshing cookies without a full re-pair is **not supported**
(§18 OQ-4).

#### Unpairing and the paired-device slot limit

Google Messages allows a **small, undocumented number of simultaneously paired
web devices** (observed at 1 active browser session per pairing plus a bounded
list of remembered devices; Google's UI presents this as a device list with a
"Remove"/"Unpair all devices" action). Two consequences the owner must know:

- Pairing Agent GM **may evict** an existing paired device, including the
  Google Messages Web tab in a browser, and pairing that tab may evict Agent GM.
  The owner should treat Agent GM as *the* paired device for this phone.
- If Agent GM is unpaired from the phone, the phone stops responding and the
  long poll produces `events.PingFailed` wrapping
  `events.ErrRequestedEntityNotFound`, or a `gmproto.RevokePairData` event.
  Agent GM treats both as **session invalidation**: it marks the session
  `unpaired`, stops the poller, and requires a fresh pair.

Agent GM never pairs implicitly. `agm pair` is always explicit, always
interactive or `--yes`-gated, and always writes an audit record.

### 3.3 Session persistence and refresh

The session is `libgm.AuthData` marshalled to JSON. It is stored **outside** the
SQLite database, at `$AGENT_GM_DATA_DIR/session.enc`, encrypted with the data
key (§4.5) using XChaCha20-Poly1305 with a random 24-byte nonce prefix.

```
session.enc := "AGMS1" || nonce[24] || AEAD(datakey, nonce, json(AuthData), aad="agent-gm/session/v1")
```

Rules:

- Written **atomically** (`session.enc.tmp` + `fsync` + `rename`), and only by
  the store writer goroutine, so a crash mid-write cannot corrupt it.
- Persisted on: `events.PairSuccessful`, `events.AuthTokenRefreshed`, every
  successful `Connect`, and on graceful shutdown. Also on a 5-minute timer if
  the in-memory `AuthData` differs from what was last written (cookies mutate
  in place via `AuthData.UpdateCookiesFromResponse`, without an event).
- File mode `0600`. Never logged, never included in a support bundle, never
  returned by any API.
- **Token refresh is the library's job.** `refreshAuthToken` runs inside
  `Connect` and inside the long-poll loop, refreshing when
  `time.Until(TachyonExpiry) <= libgm.RefreshTachyonBuffer` (1 hour).
  On success it emits `events.AuthTokenRefreshed`. Agent GM must not call any
  refresh path itself; it only persists.
- A refresh error is **fatal** if it is `events.ErrInvalidCredentials`,
  `events.ErrRequestedEntityNotFound`, or an HTTP 401/403/404
  (`client.go:isFatalRefreshError`). Fatal → session invalidated. Anything else
  is transient and the long poll retries.

### 3.4 The event stream

`SetEventHandler` receives `any`. This is the complete catalogue Agent GM
handles; anything else is logged at debug and dropped, and an unknown type
increments `unknown_events` in health.

**Connection lifecycle** (`pkg/libgm/events/ready.go`)

| Event | Meaning | Agent GM's reaction |
|---|---|---|
| `*events.ClientReady{SessionID, Conversations}` | long poll established, with the initial conversation list | mark session `connected`; upsert those conversations; kick backfill (§5.2) |
| `*events.AuthTokenRefreshed{}` | tachyon token rotated | persist `AuthData` |
| `*events.ListenTemporaryError{Error}` | poll dropped, will retry | session → `degraded`; record in health; no user-visible failure |
| `*events.ListenRecovered{}` | poll back | session → `connected` |
| `*events.ListenFatalError{Error}` | poll cannot continue | if `ErrInvalidCredentials` or the error string is `"http 401 while polling"` → invalidate session (`bad_credentials`); otherwise session → `error` and the supervisor retries `Reconnect` with backoff |
| `*events.PingFailed{Error, ErrorCount}` | ditto ping failed | if `ErrRequestedEntityNotFound` → invalidate session as `unpaired`; else if `ErrorCount > 1` → session → `error` (upstream deliberately ignores the first failure) |
| `*events.PhoneNotResponding{}` | phone unreachable | health flag `phone_responding=false`; sends still allowed but will likely fail |
| `*events.PhoneRespondingAgain{}` | recovered | `phone_responding=true` |
| `*events.NoDataReceived{}` | nothing received for `dataReceiveCheckInterval` | health counter; triggers a `ListConversations` poke |
| `*events.HackySetActiveMayFail{}` | skip-count non-zero at connect | re-issue `SetActiveSession` after a delay |
| `*events.BrowserActive{SessionID}` | a browser session became active | if `SessionID != CurrentSessionID()`, **another device has taken over**; log a warning and record it — this is the signal that the owner opened Google Messages Web (§3.2 slot limit) |

**Pairing** (`pkg/libgm/events/qr.go`, `pair.go`)

| Event | Meaning |
|---|---|
| `*events.QR{URL}` | a QR payload to display (emitted by the poller during QR pairing) |
| `*events.PairSuccessful{PhoneID, QRData}` | pairing done; `QRData` is `*gmproto.PairedData` |
| `*gmproto.RevokePairData` | the phone revoked this pairing → invalidate session |
| `*events.GaiaLoggedOut{}` | Google cookies dead → invalidate session |
| `*events.AccountChange{*gmproto.AccountChangeOrSomethingEvent, IsFake bool}` | the phone's active Google account changed. `IsFake=true` means it was synthesised at startup from `EncryptedData2`, not a real change. Agent GM records the account and, if it differs from the paired one, marks the session `account_changed` and blocks writes. |

**Data** (`pkg/libgm/event_handler.go:handleUpdatesEvent`)

All data events arrive as `ActionType_GET_UPDATES` and are demultiplexed by
`gmproto.UpdateEvents`:

| Delivered type | Source | Agent GM's reaction |
|---|---|---|
| `*libgm.WrappedMessage{*gmproto.Message, IsOld bool, Data []byte}` | `UpdateEvents_MessageEvent` | the message ingest path (§5.3). `IsOld` means the event was replayed from the server's backlog after reconnect. |
| `*gmproto.Conversation` | `UpdateEvents_ConversationEvent` | conversation upsert. **Old conversation events are skipped by the library**, so Agent GM always sees fresh ones. |
| `*gmproto.UserAlertEvent` | `UpdateEvents_UserAlertEvent` | see the alert table below. Old alerts are dropped by the library. |
| `*gmproto.Settings` | `UpdateEvents_SettingsEvent` | store the SIM list, RCS enablement, and whether Google Messages is the default SMS app |
| `*gmproto.TypingData` | `UpdateEvents_TypingEvent` | ephemeral; surfaced in `GET /v1/conversations/{id}` as `peer_typing_until`, never persisted |

`UpdateEvents_BrowserPresenceCheckEvent` is handled entirely inside the library
(it auto-acks) and never reaches Agent GM.

`gmproto.AlertType` values Agent GM acts on: `BROWSER_INACTIVE=1`,
`BROWSER_ACTIVE=2`, `BROWSER_INACTIVE_FROM_TIMEOUT=7`,
`BROWSER_INACTIVE_FROM_INACTIVITY=8` (connection health);
`MOBILE_BATTERY_LOW=5`/`MOBILE_BATTERY_RESTORED=6`,
`MOBILE_DATA_CONNECTION=3`/`MOBILE_WIFI_CONNECTION=4` (phone health, surfaced in
`GET /v1/health`); `RCS_CONNECTION=9`;
`MOBILE_DATABASE_SYNC_STARTED=13`/`MOBILE_DATABASE_SYNCING=11`/`MOBILE_DATABASE_SYNC_COMPLETE=12`
(a sync in progress means backfill results are unstable — Agent GM defers
backfill until complete). The full enum has 27 values; the rest are recorded in
the audit log and otherwise ignored.

**Library-level deduplication.** Before Agent GM sees anything,
`deduplicateUpdate` drops any message or conversation update whose
(id, SHA-256 of decrypted payload) pair matches one of the **last 8** updates
(`client.go:recentUpdates [8]updateDedupItem`). This window is small; Agent GM
must not rely on it and does its own dedup (§5.4).

### 3.5 Error taxonomy from the library

| Library error | Where | Agent GM code (§7.2) |
|---|---|---|
| `libgm.ErrPhoneNotResponding` | `session_handler.go`; the phone did not answer within `responseHardTimeout` = **60s**. Upstream notes *the server already accepted the request, so the phone may still process it later.* | `phone_not_responding` (HTTP 504) — and the operation stays `pending`, not `failed` (§6.4) |
| `libgm.ErrConnectionClosed` | request in flight when `Disconnect` ran | `disconnected` (503) |
| `events.ErrInvalidCredentials` | tachyon type 16 | `not_paired` (401 to the caller? **no** — 409, see §7.2) |
| `events.ErrRequestedEntityNotFound` | tachyon type 5 | `not_paired` (409) |
| `events.ErrCallerNoPermission` | tachyon type 7 | `google_permission_denied` (502) |
| `events.RequestError{Data *gmproto.ErrorResponse, HTTP *HTTPError}` | any non-OK tachyon response | `google_error` (502), with `google.type` and `google.message` in `details` |
| `events.HTTPError{Action, Resp, Body}` | transport-level | `google_http_error` (502), with `details.status` |
| `pair_google.Err*` (7 values, §3.1) | pairing | `pairing_no_cookies`, `pairing_no_devices`, `pairing_wrong_emoji`, `pairing_cancelled`, `pairing_timeout`, `pairing_init_timeout`, `pairing_multiple_devices` (all 409) |

`events.RequestError.Is` compares `Type` and `Message` only, not the error
class — so `errors.Is` against the three sentinel values is reliable and is
what Agent GM uses.

### 3.6 The pin, and the policy for changing it

```
module:  go.mau.fi/mautrix-gmessages
commit:  be48a58
subject: libgm/config: bump version
ConfigVersion (util.ConfigMessage): Year=2026 Month=9 Day=2 V1=4 V2=6
go directive: 1.26.0 (toolchain go1.27.0)
```

`go.mod` pins by pseudo-version resolving to `be48a58`. A vendored checkout of
the upstream tree at that commit lives at `/home/nick/code/mautrix-gmessages`
on the owner's machine and is **not** committed here; CI re-clones it for the
fixture-validation job (§13.4).

**Pin-and-bump policy.**

- The pin is a **fact recorded in three places that must agree**: `go.mod`, the
  constant `gm.PinnedUpstreamCommit` in `internal/gm/pin.go`, and this section.
  A CI job (`pin-consistency`) fails the build if they diverge.
- Agent GM **never** floats the dependency. `GOFLAGS=-mod=readonly` is set in
  `devbox.json` so an accidental `go get` cannot silently move it.
- **Bumping the pin is a deliberate slice**, never a drive-by commit. The slice
  must: (a) update all three places; (b) diff `pkg/libgm` between old and new
  commit and record, in `docs/upstream-pin.md`, every change to a symbol in
  §3.1, every change to `util.ConfigMessage`, and every added/removed/renamed
  enum value in §3.7 or §5.5; (c) re-run the fixture validation job; (d) pass a
  **live gate** (§13.3) — pair, list, send one text to the approved number,
  receive a reply — run by the coordinator, not by an implementer.
- A bump that changes `util.ConfigMessage` is **expected to be urgent**: a stale
  ConfigVersion is the known cause of the undocumented `GetOrCreateConversation`
  status in §3.7. Agent GM surfaces the compiled-in and the live-fetched
  versions side by side in `GET /v1/health` precisely so this is diagnosable
  without reading logs (§3.7).

### 3.7 Undocumented statuses and other sharp edges

**`GetOrCreateConversationResponse.Status`.** The proto at the pinned commit
declares only:

```proto
enum Status {
    UNKNOWN = 0;
    SUCCESS = 1;
    CREATE_RCS = 3;
}
```

Observed values and what they mean:

| Value | Name | Meaning | Agent GM |
|---|---|---|---|
| 0 | `UNKNOWN` | phone did not classify the request | `google_error`, retry once |
| 1 | `SUCCESS` | conversation returned in `.Conversation` | proceed |
| 2 | *(unnamed)* | not observed | `google_error`, `details.status=2` |
| 3 | `CREATE_RCS` | the phone wants the caller to retry with `CreateRCSGroup=true` (and a non-nil `RCSGroupName`, empty string is acceptable). Upstream retries once, exactly this way. | retry once with `CreateRCSGroup=true`; a second `CREATE_RCS` is `google_error` |
| **4** | *(unnamed — no enum entry)* | **The stale-ConfigVersion symptom.** When `util.ConfigMessage` in the compiled binary is older than what Google currently serves, `GetOrCreateConversation` returns status 4 with **no conversation body**, and every attempt to start a new chat fails while existing conversations keep working. It is not a per-request error and retrying does not help. | `config_version_stale` (HTTP 502), with a message that names the compiled and live ConfigVersions and says the fix is a library pin bump (§3.6). Agent GM must **not** report this as a generic Google error, because that has historically cost hours of misdirected debugging. |

Because the proto has no name for 2 or 4, `resp.GetStatus().String()` renders
them as the bare number; Agent GM logs the numeric value, never a name it made
up.

Detection rule for `config_version_stale`: status is 4, **or** status is not
`SUCCESS` and `FetchConfig`'s live `ConfigVersion` differs from
`util.ConfigMessage` in year, month or day. `GET /v1/health` always reports:

```json
"google": {
  "config_version_compiled": "2026.9.2",
  "config_version_live": "2026.9.2",
  "config_version_stale": false,
  "is_default_sms_app": true,
  "phone_responding": true,
  "upstream_commit": "be48a58"
}
```

**Other sharp edges, all load-bearing:**

- **`ListConversations` must be called once per process before live
  conversation events are trustworthy.** The first call uses
  `MessageType_BUGLE_ANNOTATION` and subsequent calls `BUGLE_MESSAGE`
  (`methods.go`, `conversationsFetchedOnce`). Agent GM always issues one on
  connect.
- **`SendMessageResponse` transient statuses.** Upstream retries `FAILURE_2`
  and `FAILURE_3` with backoff `[3s, 8s, 20s]`
  (`connector/handlematrix.go:isTransientSendFailure`, `sendRetryBackoff`).
  Agent GM uses the same set and the same backoff. `FAILURE_4` is **not**
  retried and is reported as `not_default_sms_app` when `IsBugleDefault` also
  says false.
- **`ErrPhoneNotResponding` does not mean the send failed.** The server accepted
  it; the phone may still deliver it when it wakes. Agent GM keeps the operation
  `pending` (§6.4) and lets the remote echo resolve it.
- **`SendMessageResponse.GoogleAccountSwitch`** being non-empty means the phone
  switched Google accounts; Agent GM records it and marks the session
  `account_changed`.
- **Tombstone statuses are not messages.** `MessageStatusType` values 200–299
  are protocol/system events. Agent GM stores them with `kind="tombstone"` and
  excludes them from `messages.list` unless `include_tombstones=true`
  (§7.5). The set upstream ignores outright in group chats
  (`handlegmessages.go:shouldIgnoreStatus`) is carried over verbatim.
- **`Conversation.LatestMessage` is large and duplicative.** Upstream clones and
  nils it before logging. Agent GM never persists it as part of the conversation
  row; it goes through the message ingest path or nowhere.
- **Timestamps are microseconds.** `gmproto.Message.Timestamp` is a Unix
  timestamp in **microseconds**, as is `Conversation.LastMessageTimestamp`.
  `TachyonTTL` is likewise microseconds. Agent GM converts once, at the `gm`
  boundary, into `time.Time`, and stores milliseconds (§4.3).
- **`libgm` writes to the process's zerolog logger** and can log at trace level
  the base64 of decrypted payloads (`logContent`). Agent GM configures the
  library logger at `info` in production and forbids `trace` unless
  `AGENT_GM_UNSAFE_TRACE=1` is set, which also stamps every log line with
  `unsafe_trace=true` (§12.2).

---
