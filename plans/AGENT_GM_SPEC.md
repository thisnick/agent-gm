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

## 4. Data model and identifiers

### 4.1 Identifier scheme

Public IDs are opaque, URL-safe, stable strings with a typed prefix. A caller
never sees a raw Google conversation ID, message ID or participant ID on a
public surface; those live in internal `source_*` columns and in
`admin`-gated diagnostics.

| Prefix | Object | Derivation |
|---|---|---|
| `conv_` | conversation | UUIDv5(ns, `account_key` ‖ `"conversation"` ‖ Google conversation ID) |
| `msg_` | message | UUIDv5(ns, `account_key` ‖ `"message"` ‖ Google conversation ID ‖ Google message ID) |
| `att_` | attachment (one media part of a message) | UUIDv5(ns, message ID ‖ part index ‖ Google media ID) |
| `react_` | reaction | UUIDv5(ns, message ID ‖ participant ID ‖ fully-qualified emoji) |
| `contact_` | contact | UUIDv5(ns, `account_key` ‖ `"contact"` ‖ Google participant ID) |
| `part_` | participant of a conversation | UUIDv5(ns, conversation ID ‖ Google participant ID) |
| `op_` | operation | UUIDv7 (locally created) |
| `upl_` | upload reservation | UUIDv7 |
| `authreq_` | pending OAuth authorization request | UUIDv7 |
| `auth_` | active authorization (grant) | UUIDv7 |
| `client_` | registered OAuth client | UUIDv7 |
| `enroll_` | enrollment code record | UUIDv7 |
| `req_` | request ID, in every response envelope | UUIDv7, not stored |

Rules:

- **One frozen namespace UUID**, declared once in `internal/store/ids.go` as
  `IDNamespace`, never changed. Changing it renames every object in the world.
- **`account_key` is in every derivation**, even though there is exactly one
  account. It is `SHA-256(AuthData.Mobile.SourceID)` truncated to 16 bytes,
  captured at pair time and stored in `server_meta`. This is carried over from
  Agent MX deliberately: it means a database rebuilt from Google reproduces the
  same IDs, and it means a *different* phone can never produce a colliding ID.
  **Re-pairing the same phone reuses the same `account_key`, so conversations
  and messages keep their IDs across a re-pair.** Pairing a different phone
  produces a completely different ID space, which is the correct outcome.
- Deterministic (UUIDv5) IDs are used for everything derived from Google, so a
  wiped database that is re-backfilled hands agents the same IDs they had
  before. UUIDv7 is used for everything Agent GM itself creates.
- An ID with the wrong prefix for the parameter is `invalid_request` naming the
  parameter and the expected prefix — **never `not_found`**, which would read
  as "that thing is gone".
- A raw Google ID presented where an Agent GM ID is expected is
  `invalid_request` with the same message. There are no aliases and no
  fallbacks.

### 4.2 SQLite schema

One database file, `$AGENT_GM_DATA_DIR/agent-gm.sqlite3`. Opened with
`journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`,
`synchronous=NORMAL`. One writer goroutine; a read-only pool for queries.

```sql
-- identity and process state -------------------------------------------------
CREATE TABLE server_meta (
    key            TEXT PRIMARY KEY,
    value          TEXT NOT NULL
);
-- keys: account_key, phone_id, schema_version_note, pending_reprocess,
--       upstream_commit, config_version_compiled, session_state,
--       backfill_complete_at

-- conversations --------------------------------------------------------------
CREATE TABLE conversations (
    id                     TEXT PRIMARY KEY,          -- conv_...
    source_id              TEXT NOT NULL UNIQUE,      -- Google conversationID
    name                   TEXT,                      -- may be empty for DMs
    is_group               INTEGER NOT NULL DEFAULT 0,
    conversation_type      TEXT NOT NULL,             -- unknown|sms|rcs
    send_mode              TEXT NOT NULL,             -- auto|xms|xms_latch
    folder                 TEXT NOT NULL DEFAULT 'inbox', -- inbox|archive|spam_blocked
    unread                 INTEGER NOT NULL DEFAULT 0,
    pinned                 INTEGER NOT NULL DEFAULT 0,
    read_only              INTEGER NOT NULL DEFAULT 0,
    default_outgoing_id    TEXT,                      -- participantID to send as
    latest_message_id      TEXT,                      -- msg_... , nullable
    last_activity_ms       INTEGER NOT NULL,          -- ms since epoch
    group_avatar_url       TEXT,
    sim_payload_json       TEXT,                      -- opaque, re-sent verbatim
    deleted_at_ms          INTEGER,                   -- set by delete-for-me
    created_at_ms          INTEGER NOT NULL,
    updated_at_ms          INTEGER NOT NULL
);
CREATE INDEX conversations_activity ON conversations(last_activity_ms DESC, id);
CREATE INDEX conversations_folder   ON conversations(folder, last_activity_ms DESC);

-- participants ---------------------------------------------------------------
CREATE TABLE participants (
    id                TEXT PRIMARY KEY,               -- part_...
    conversation_id   TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    source_id         TEXT NOT NULL,                  -- Google participantID
    contact_id        TEXT REFERENCES contacts(id),
    display_name      TEXT,
    first_name        TEXT,
    phone_e164        TEXT,
    formatted_number  TEXT,
    identifier_type   TEXT,                           -- phone|email|unknown|...
    is_me             INTEGER NOT NULL DEFAULT 0,
    is_visible        INTEGER NOT NULL DEFAULT 1,
    UNIQUE (conversation_id, source_id)
);
CREATE INDEX participants_phone ON participants(phone_e164);

CREATE TABLE contacts (
    id            TEXT PRIMARY KEY,                   -- contact_...
    source_id     TEXT NOT NULL UNIQUE,
    display_name  TEXT,
    phone_e164    TEXT,
    avatar_hash   TEXT,
    is_top        INTEGER NOT NULL DEFAULT 0,
    updated_at_ms INTEGER NOT NULL
);
CREATE INDEX contacts_phone ON contacts(phone_e164);

-- messages -------------------------------------------------------------------
CREATE TABLE messages (
    id                  TEXT PRIMARY KEY,             -- msg_...
    conversation_id     TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    source_id           TEXT NOT NULL,                -- Google messageID
    kind                TEXT NOT NULL,                -- message|tombstone
    direction           TEXT NOT NULL,                -- incoming|outgoing
    sender_participant  TEXT,                         -- part_... , null for system
    text                TEXT,                         -- concatenated text parts
    subject             TEXT,
    delivery_state      TEXT NOT NULL,                -- see 4.4
    delivery_state_raw  INTEGER NOT NULL,             -- the numeric MessageStatusType
    delivery_error      TEXT,                         -- Message.MessageStatus.errMsg
    reply_to_message_id TEXT,                         -- msg_... , nullable
    operation_id        TEXT,                         -- op_... , outgoing only
    tmp_id              TEXT,                         -- the TmpID we sent, for echo match
    is_deleted          INTEGER NOT NULL DEFAULT 0,
    sent_at_ms          INTEGER NOT NULL,             -- Google timestamp, us -> ms
    ingested_at_ms      INTEGER NOT NULL,
    content_hash        TEXT NOT NULL,                -- sha256 of canonical content
    UNIQUE (conversation_id, source_id)
);
CREATE INDEX messages_conv_time ON messages(conversation_id, sent_at_ms DESC, id DESC);
CREATE INDEX messages_time      ON messages(sent_at_ms DESC, id DESC);
CREATE INDEX messages_tmp_id    ON messages(tmp_id) WHERE tmp_id IS NOT NULL;

CREATE VIRTUAL TABLE messages_fts USING fts5(
    text, subject, content='messages', content_rowid='rowid', tokenize='unicode61'
);

-- attachments ----------------------------------------------------------------
CREATE TABLE attachments (
    id                 TEXT PRIMARY KEY,              -- att_...
    message_id         TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    part_index         INTEGER NOT NULL,
    media_id           TEXT,                          -- Google mediaID, may be empty
    thumbnail_media_id TEXT,
    decryption_key     BLOB,                          -- encrypted at rest, see 4.5
    filename           TEXT,
    mime_type          TEXT,
    media_format       TEXT,                          -- gmproto MediaFormats name
    size_bytes         INTEGER,
    width              INTEGER,
    height             INTEGER,
    download_state     TEXT NOT NULL,                 -- available|pending|failed|unavailable
    cache_path         TEXT,                          -- relative to media-cache/objects
    sha256             TEXT,                          -- of decrypted bytes, nullable
    UNIQUE (message_id, part_index)
);

-- reactions ------------------------------------------------------------------
CREATE TABLE reactions (
    id              TEXT PRIMARY KEY,                 -- react_...
    message_id      TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    participant_id  TEXT NOT NULL,                    -- part_...
    emoji           TEXT NOT NULL,                    -- fully-qualified unicode
    emoji_type      TEXT NOT NULL,                    -- LIKE|LOVE|...|CUSTOM
    is_mine         INTEGER NOT NULL DEFAULT 0,
    updated_at_ms   INTEGER NOT NULL,
    UNIQUE (message_id, participant_id, emoji)
);

-- operations (idempotency + status, NOT an outbox) ---------------------------
CREATE TABLE operations (
    id                    TEXT PRIMARY KEY,           -- op_...
    kind                  TEXT NOT NULL,              -- send_text|send_media|react|...
    authorization_id      TEXT NOT NULL,
    idempotency_key       TEXT NOT NULL,
    request_fingerprint   TEXT NOT NULL,              -- sha256 of canonical body
    conversation_id       TEXT,
    message_id            TEXT,                       -- filled by the echo
    status                TEXT NOT NULL,              -- see 6.4
    terminal              INTEGER NOT NULL DEFAULT 0,
    terminal_at_ms        INTEGER,
    corrected_at_ms       INTEGER,
    error_code            TEXT,
    error_message         TEXT,
    error_retryable       INTEGER,
    google_status_raw     INTEGER,                    -- SendMessageResponse.Status
    request_payload_json  TEXT NOT NULL,              -- redacted: no bodies
    created_at_ms         INTEGER NOT NULL,
    updated_at_ms         INTEGER NOT NULL,
    UNIQUE (authorization_id, kind, idempotency_key)
);
CREATE INDEX operations_pending ON operations(status) WHERE terminal = 0;

-- uploads --------------------------------------------------------------------
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
    staged_path         TEXT,
    expires_at_ms       INTEGER NOT NULL,
    created_at_ms       INTEGER NOT NULL
);

-- media cache ----------------------------------------------------------------
CREATE TABLE media_cache_entries (
    attachment_id   TEXT PRIMARY KEY REFERENCES attachments(id) ON DELETE CASCADE,
    relative_path   TEXT NOT NULL,
    size_bytes      INTEGER NOT NULL,
    last_used_ms    INTEGER NOT NULL
);

-- auth -----------------------------------------------------------------------
CREATE TABLE oauth_clients (...);            -- client_...
CREATE TABLE enrollment_codes (...);         -- enroll_... , code_hash only
CREATE TABLE authorization_requests (...);   -- authreq_...
CREATE TABLE authorizations (...);           -- auth_...
CREATE TABLE tokens (...);                   -- hashes only, never values
CREATE TABLE oauth_attempts (...);           -- durable failure limiter

-- settings and audit ---------------------------------------------------------
CREATE TABLE settings (
    key         TEXT PRIMARY KEY,
    value_json  TEXT NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

CREATE TABLE audit_events (
    id                TEXT PRIMARY KEY,               -- UUIDv7
    kind              TEXT NOT NULL,
    authorization_id  TEXT,
    target_type       TEXT,
    target_id         TEXT,
    result            TEXT NOT NULL,                  -- ok|refused|failed
    source            TEXT,                           -- resolved client source
    payload_json      TEXT NOT NULL,                  -- redacted, see 12.4
    created_at_ms     INTEGER NOT NULL
);
CREATE INDEX audit_kind_time ON audit_events(kind, created_at_ms DESC);
CREATE INDEX audit_time      ON audit_events(created_at_ms DESC);
```

### 4.3 Migrations

- **Forward-only, numbered, never edited after they ship.** `0001_init.sql`,
  `0002_...`. `PRAGMA user_version` is the version. A database at a *higher*
  version than the binary knows refuses to open, with an error naming both
  numbers. There is no down-migration.
- Migrations run **inside one transaction each**, on the writer goroutine,
  before any HTTP listener binds.
- A migration that needs data recomputed sets
  `server_meta.pending_reprocess = <task name>`; the process runs that task
  once after startup and clears the key. This is how a schema change that
  affects derived data (e.g. a delivery-state remap) is applied without a
  hand-written data migration.
- `PRAGMA foreign_key_check` runs after every migration in tests, and its
  output must be empty (§13.2).
- **Audit rows are never rewritten by a migration.** An audit row records what
  happened when it happened; rewriting it would make the trail claim a value
  that did not exist then.

### 4.4 Timestamps and the delivery-state vocabulary

Google timestamps are **microseconds**; Agent GM converts at the `gm` boundary
and stores **milliseconds since the Unix epoch** in every `*_ms` column. Every
JSON surface renders them as RFC 3339 UTC with millisecond precision.

`messages.delivery_state` is Agent GM's own closed vocabulary, mapped from
`gmproto.MessageStatusType` (the numeric value is kept in
`delivery_state_raw` so nothing is lost):

| `delivery_state` | Meaning | `MessageStatusType` sources |
|---|---|---|
| `queued` | accepted locally, not yet at the carrier | `OUTGOING_YET_TO_SEND(4)`, `OUTGOING_SEND_AFTER_PROCESSING(10)`, `OUTGOING_SCHEDULED(16)`, `OUTGOING_DRAFT(3)` |
| `sending` | in flight | `OUTGOING_SENDING(5)`, `OUTGOING_RESENDING(6)`, `OUTGOING_AWAITING_RETRY(7)`, `OUTGOING_VALIDATING(20)` |
| `sent` | the carrier took it | `OUTGOING_COMPLETE(1)`, `OUTGOING_NOT_DELIVERED_YET(14)` |
| `delivered` | the recipient's device has it | `OUTGOING_DELIVERED(2)` |
| `read` | the recipient opened it | `OUTGOING_DISPLAYED(11)` |
| `failed` | terminal failure | every `OUTGOING_FAILED_*` (8, 9, 13, 17, 18, 19, 21, 22, 24, 25, 27), `OUTGOING_RESTRICTED(26)` |
| `canceled` | withdrawn | `OUTGOING_CANCELED(12)`, `OUTGOING_REVOCATION_PENDING(15)` |
| `deleted` | removed | `OUTGOING_DELETED(23)`, `INCOMING_DELETED(117)` |
| `received` | an incoming message that is complete | `INCOMING_COMPLETE(100)`, `INCOMING_DELIVERED(108)`, `INCOMING_DISPLAYED(109)` |
| `downloading` | incoming media not yet fetched | `INCOMING_*_DOWNLOADING`, `INCOMING_*_DOWNLOAD`, `INCOMING_AWAITING_AUTO_DOWNLOAD(115)` |
| `download_failed` | incoming media unavailable | `INCOMING_DOWNLOAD_FAILED*`, `INCOMING_EXPIRED_OR_NOT_AVAILABLE(107)`, `INCOMING_FAILED_TO_DECRYPT(113)`, `INCOMING_DECRYPTION_ABORTED(114)`, `INCOMING_DOWNLOAD_RESTRICTED(118)` |
| `unknown` | anything else, including a value the pinned proto has no name for | `STATUS_UNKNOWN(0)` and unmapped values |

Permitted transitions for an **outgoing** message, enforced in SQL by a
trigger, not only in Go:

```text
queued -> sending -> sent -> delivered -> read
queued|sending|sent      -> failed
queued|sending           -> canceled
any                      -> deleted
unknown                  -> any (a late authoritative status corrects it)
```

A transition that skips forward (`queued -> delivered`) is **accepted** — the
phone genuinely reports coarse jumps — but a *backward* move (`read -> sent`)
is refused, logged, and audited as `message.status_out_of_order`, and the
stored state is left alone. `delivery_state_raw` is always overwritten with
whatever Google last said, so a reviewer can always see the raw truth.

`read` is a state on the *message*, not on the operation (§6.4).

### 4.5 The data key

`AGENT_GM_DATA_KEY` is a 256-bit key supplied as 64 hex characters or standard
base64. It is used to derive, by HKDF-SHA256 with distinct `info` strings:

| Purpose | `info` |
|---|---|
| session file envelope (§3.3) | `agent-gm/session/v1` |
| `attachments.decryption_key` column encryption | `agent-gm/attachment-key/v1` |
| upload and download ticket signing (§10.3) | `agent-gm/ticket/v1` |
| pagination cursor signing (§7.3) | `agent-gm/cursor/v1` |

**The data key is not rotatable in place.** A database restored without the
key that sealed it cannot decrypt the session file or any attachment key. The
key and the data directory move together, always. If the key is lost the
recovery path is: delete `session.enc`, re-pair (§11.4), and re-backfill;
message text survives because it is not encrypted at rest, but cached media
and the session do not.

Refusing to start with `session envelope cannot be decrypted` means the key
differs from the one that sealed the session. Restore the original key; there
is no in-place rotation.

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

Triggered on `events.ClientReady`, and again on demand via
`POST /v1/admin/backfill`.

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
7. Set server_meta.backfill_complete_at.
```

Concurrency is `settings.backfill.concurrency` (default 2, bounds 1–8)
conversations at a time. Backfill is **paused** while a
`MOBILE_DATABASE_SYNC_STARTED`/`SYNCING` alert is outstanding and resumes on
`MOBILE_DATABASE_SYNC_COMPLETE` (§3.4), because results during a phone-side
sync are unstable.

Backfill never blocks reads. `GET /v1/health` reports
`backfill: {state, conversations_done, conversations_total}`, and every list
response carries a `history_incomplete` warning until
`backfill_complete_at` is set, so an empty result during backfill is not read
as an absent message.

### 5.3 Live ingestion

One goroutine, `core.ingestLoop`, drains `gm.Events()` and applies each event
as one store write transaction. It is the **only** writer of message rows.

For a `*libgm.WrappedMessage`:

```
1. If shouldIgnoreStatus(status, isDM) says ignore -> drop, count it, done.
   (The set is carried over verbatim from
   connector/handlegmessages.go:shouldIgnoreStatus.)
2. If status is in 200..299 -> kind="tombstone", store, exclude from
   messages.list unless include_tombstones=true.
3. Derive msg_ ID from (conversation source ID, message source ID).
4. Compute content_hash over the canonical content (text parts joined with
   \n, then each media part's mediaID and size, then the reaction set).
5. If a row with that ID exists:
     - if content_hash and delivery_state_raw are both unchanged -> no-op.
     - else update, honouring the transition rules of 4.4.
   Else insert.
6. Upsert attachments from MessageInfo entries.
7. Replace the reaction set from Message.Reactions (it is authoritative and
   complete, so this is a set-replace, not a merge).
8. If Message.TmpID matches an operation's tmp_id, resolve that operation
   (6.3).
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
     on; Agent GM assumes it does nothing.
  2. The primary key on `(conversation_id, source_id)` — a repeat is an
     upsert, never a second row.
  3. `content_hash` — an upsert whose content and raw status are both
     unchanged writes nothing at all, so a replay storm does not churn the WAL
     or bump `updated_at_ms`.
- **`last_activity_ms` is monotonic per conversation.** It is only ever moved
  forward, with `MAX(existing, new)`, so a replayed old message cannot make a
  conversation jump to the top of the list.

### 5.5 Delivery-status transitions in practice

The sequence an agent observes for its own outgoing text, in the normal case:

```
POST /v1/conversations/{id}/messages      -> 200, operation succeeded,
                                             message_id present
message.delivery_state: queued            (echo, MessageStatusType 4 or 5)
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
  inside the same request. Total worst-case latency for a send is therefore
  bounded at roughly 4 × 60s + 31s; the HTTP handler enforces a 240-second
  deadline and returns `phone_not_responding` if it is exceeded.

The `operations` table still exists, for two reasons only: **idempotency**
and **status**. It is a record of what happened, not a queue of what to do.

### 6.2 The order of a mutation

This order is part of the contract and is tested (§13.2):

```
1. Authenticate; check scope.  -> 401 / 403 at the transport
2. Parse strictly.             -> invalid_request naming the key
3. Resolve the conversation.   -> not_found (indistinguishable from unseen)
4. Check it is actionable.     -> unsupported_capability + details.reason,
                                  and NO operation row is created
5. Look up the idempotency key.
     same key + same fingerprint  -> return the existing operation, do nothing
     same key + different body    -> idempotency_conflict, do nothing
6. INSERT the operation row with status='running' and COMMIT.
7. Call libgm.
8. UPDATE the operation with the outcome, and COMMIT.
9. Respond.
```

Step 6 committing *before* step 7 is what makes the crash story honest: if the
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
- Uniqueness is scoped to **(authorization, operation kind, key)**. Two
  clients may use the same key value; one client may use one key for a send
  and for a mark-read.
- "The same request" is decided by a **SHA-256 over the canonically
  serialised body** (keys sorted, no insignificant whitespace), so reordered
  JSON keys are a replay and any changed value is not.
- A replay returns the existing operation **and its `message_id`**, and sends
  nothing. A fresh key is a different call, not a repeat — every tool
  description and CLI help text says so, because this is the mistake that
  sends a second text message to a real person.
- Keys are retained for **30 days** (`settings.operations.idempotency_ttl`,
  bounds 1d–365d), after which the row is swept. A replay of a swept key is a
  new operation; the CLI and the tool descriptions state the retention.

The operation's `tmp_id` is set to the operation ID and is what goes into
`SendMessageRequest.TmpID` / `MessagePayload.TmpID` / `TmpID2` (§3.1). When
the remote echo arrives carrying that `TmpID`, the ingest loop writes the
message's `msg_` ID onto the operation and, if the operation is `pending`,
settles it (§6.4).

### 6.4 Operation status

| Status | Meaning | Terminal |
|---|---|---|
| `running` | in flight, or the process died mid-call | no |
| `succeeded` | the phone accepted it (`SendMessageResponse_SUCCESS`, or a `Success: true` for reactions/deletes) | yes |
| `pending` | **`libgm.ErrPhoneNotResponding` only.** The server accepted the request; the phone may still act on it when it wakes. This is *not* a failure. | no |
| `failed` | the phone refused it, or a non-retryable error | yes |
| `unknown` | never settled within `settings.operations.pending_timeout` (default 24h, bounds 1h–7d), or recovered from a crash | yes |

Transitions:

```text
running -> succeeded | failed | pending
pending -> succeeded            (the remote echo arrived)
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

### 6.5 Crash recovery

At startup, before the listener binds:

```
UPDATE operations
   SET status='unknown', terminal=1, terminal_at_ms=?, error_code='crash_recovered'
 WHERE status='running';
```

Each row is audited as `operation.crash_recovered`. Nothing is retried. If the
send did reach Google, the remote echo will arrive on reconnect and correct
the operation to `succeeded` with a `message_id` — which is exactly why the
correction transition out of `unknown` exists.

A `pending` operation is left alone at startup; the reaper settles it at
`pending_timeout` measured from `created_at_ms`.

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
`filename_normalized` or `filename_truncated`.

### 7.2 Error codes

| Code | HTTP | Retryable | Meaning |
|---|---|---|---|
| `invalid_request` | 400 | no | malformed, unknown parameter/field, wrong ID prefix, contradictory idempotency key |
| `invalid_token` | 401 | no | absent, expired, unknown, or wrong-audience bearer |
| `insufficient_scope` | 403 | no | valid token, wrong scope |
| `not_found` | 404 | no | no such object. Byte-identical whether it never existed or the caller may not see it |
| `idempotency_conflict` | 409 | no | same key, different body |
| `not_paired` | 409 | no | no Google Messages session, or the session was invalidated |
| `pairing_no_cookies` | 409 | no | Google-account pairing without cookies |
| `pairing_no_devices` | 409 | no | the account has no primary device |
| `pairing_multiple_devices` | 409 | no | more than one primary-looking device |
| `pairing_wrong_emoji` | 409 | no | the owner tapped the wrong emoji |
| `pairing_cancelled` | 409 | no | the owner dismissed or chose "not me" |
| `pairing_timeout` | 409 | no | no response within the window |
| `pairing_init_timeout` | 409 | yes | `GaiaInitTimeout` (20s) elapsed |
| `unsupported_capability` | 409 | no | the action cannot apply here; `details.reason` from §7.7 |
| `payload_too_large` | 413 | no | body over 1 MiB, or media over `media.upload_max_bytes` |
| `media_unsupported_type` | 415 | no | mime not in `libgm.MimeToMediaType` |
| `rate_limited` | 429 | yes | with `Retry-After` |
| `internal_error` | 500 | yes | a bug |
| `not_default_sms_app` | 502 | no | `FAILURE_4` and `IsBugleDefault` is false |
| `config_version_stale` | 502 | no | §3.7. Names both ConfigVersions and says the fix is a pin bump |
| `google_error` | 502 | maybe | a tachyon error; `details.google_type`, `details.google_message` |
| `google_http_error` | 502 | yes | transport-level; `details.status` |
| `google_permission_denied` | 502 | no | `ErrCallerNoPermission` |
| `disconnected` | 503 | yes | `ErrConnectionClosed`; the long poll is down |
| `phone_not_responding` | 504 | yes | `ErrPhoneNotResponding`. **The operation is `pending`, not failed — do not resend.** |

A body over 1 MiB is `413` carrying `payload_too_large`; a body that fails to
read for any other reason is `400`, because "too large" would be a guess.

`401` and `403` carry `WWW-Authenticate` with `realm="agent-gm"`, an `error`
parameter, `resource_metadata` pointing at
`https://gm.agent-wx.app/.well-known/oauth-protected-resource/mcp`, and, for a
scope refusal, the `scope` the route requires.

### 7.3 Pagination

Cursors are **opaque, HMAC-signed (data key, `agent-gm/cursor/v1`), and bound
to the endpoint and the filter set as written**. Reusing a cursor with
different filters is `invalid_request`. The binding is to the query as
written, not to a normalised form: a cursor issued without `folder` is not
valid when replayed with `folder=inbox`, although the two select the same
rows. A client that walks a listing sends the same query string on every page
anyway.

`limit` defaults to 50 and caps at 100 on every listing.
`GET /v1/messages/{id}/context` takes `before`/`after`, each defaulting to 5
and capped at 100. Ordering is newest-first unless the route says otherwise.
The cursor encodes `(sent_at_ms, id)` so it is stable across equal timestamps.

### 7.4 Routes — health, session, pairing

| Method | Path | Scope | Notes |
|---|---|---|---|
| `GET` | `/healthz` | none | liveness. Never touches SQLite. `200 {"status":"ok"}` |
| `GET` | `/v1/health` | `messages:read` | the full picture: `google` (§3.7), `session`, `backfill`, `counters`, `version`, `source_url` (§1.4) |
| `GET` | `/v1/session` | `messages:read` | pairing state: `state`, `phone_id`, `paired_at`, `connected`, `phone_responding`, `last_event_at` |
| `POST` | `/v1/pairing/start` | `admin` | `{"method":"qr"}` or `{"method":"google","cookies":{...}}`. Returns `{"pairing_id","method","qr":{"payload","png_data_url"},"expires_at"}` for QR, or `{"pairing_id","method","emoji"}` for Google. |
| `GET` | `/v1/pairing/{pairing_id}` | `admin` | poll: `{"state":"waiting|paired|failed|expired","qr":{...},"emoji":"…","error":{...}}`. A refreshed QR appears here. |
| `DELETE` | `/v1/pairing/{pairing_id}` | `admin` | abandon an in-flight pairing |
| `POST` | `/v1/session/unpair` | `admin` | `libgm.Unpair`. Requires `{"confirm":true}`. Audited. |
| `POST` | `/v1/session/reconnect` | `admin` | force `Reconnect()`. For operator use after a network event. |

`session.state` vocabulary: `unpaired`, `pairing`, `connected`, `degraded`
(temporary listen error), `error`, `bad_credentials`, `account_changed`,
`logged_out`.

### 7.5 Routes — reads

`messages:read` on all of these.

| Method | Path | Parameters |
|---|---|---|
| `GET` | `/v1/conversations` | `query`, `participant`, `folder` (`inbox`\|`archive`\|`spam_blocked`), `type` (`sms`\|`rcs`), `unread_only`, `group_only`, `include_deleted`, `cursor`, `limit` |
| `GET` | `/v1/conversations/{conversation_id}` | — |
| `GET` | `/v1/conversations/{conversation_id}/messages` | `cursor`, `limit`, `direction` (`incoming`\|`outgoing`), `sender`, `after`, `before` (RFC 3339), `has_attachment`, `delivery_state`, `include_tombstones` |
| `GET` | `/v1/messages` | the same plus `conversation_id` |
| `GET` | `/v1/messages/{message_id}` | — |
| `GET` | `/v1/messages/{message_id}/context` | `before` (default 5, max 100), `after` |
| `GET` | `/v1/messages/{message_id}/attachments` | metadata only |
| `GET` | `/v1/search/messages` | `q` (required), `syntax` (`literal` default, `fts5`), `conversation_id`, `sender`, `after`, `before`, `has_attachment`, `cursor`, `limit`. Returns `results[{message, rank, snippet, conversation}]` plus `coverage` |
| `GET` | `/v1/contacts` | `query`, `top`, `cursor`, `limit` |
| `GET` | `/v1/attachments/{attachment_id}` | metadata + a download ticket (§10) |
| `GET` | `/v1/attachments/{attachment_id}/content` | bytes. Access token **or** download ticket |
| `GET` | `/v1/operations/{operation_id}` | `messages:write`. The caller's own operations; another authorization's is `not_found` |

`participant` accepts an E.164 number (`+1<APPROVED_DIRECT_NUMBER>`), the bare digits, a
national form, or a `part_`/`contact_` ID. `sender` accepts the same plus the
literal `me`.

Conversation DTO:

```json
{ "id": "conv_...", "name": "Alex", "is_group": false, "type": "rcs",
  "send_mode": "auto", "folder": "inbox", "unread": true, "pinned": false,
  "read_only": false,
  "participants": [ { "id": "part_...", "contact_id": "contact_...",
                      "display_name": "Alex", "phone": "+15105550123",
                      "is_me": false } ],
  "last_activity_at": "2026-09-06T09:41:02.115Z",
  "latest_message_id": "msg_...",
  "capabilities": { "send_text": true, "send_media": true, "reply": true,
                    "react": true, "mark_read": true, "delete_message": true,
                    "delete_conversation": true, "typing": true },
  "created_at": "…", "updated_at": "…" }
```

Message DTO:

```json
{ "id": "msg_...", "conversation_id": "conv_...", "kind": "message",
  "direction": "outgoing", "sender": { "id": "part_...", "is_me": true },
  "text": "on my way", "subject": null,
  "delivery": { "state": "delivered", "state_raw": 2, "error": null,
                "updated_at": "2026-09-06T09:41:07.900Z" },
  "reply_to_message_id": null, "operation_id": "op_...",
  "attachments": [ { "id": "att_...", "mime_type": "image/jpeg",
                     "filename": "IMG_0421.jpg", "size": 184320,
                     "width": 1024, "height": 768,
                     "download_state": "available" } ],
  "reactions": [ { "id": "react_...", "emoji": "👍",
                   "participant_id": "part_...", "is_mine": false } ],
  "is_deleted": false,
  "sent_at": "2026-09-06T09:41:02.115Z" }
```

### 7.6 Routes — writes

`messages:write` unless noted.

| Method | Path | Body | Answer |
|---|---|---|---|
| `POST` | `/v1/conversations` | `{"recipients":["+1…"], "name"?, "client_request_id"}` | `200` with the existing conversation, or `200` with a newly created one. `GetOrCreateConversation`; the `CREATE_RCS` retry of §3.7 is internal. `name` is accepted only for 2+ recipients. Zero recipients, or two that normalise to one number, is `invalid_request` **before** an operation row exists. |
| `POST` | `/v1/conversations/{id}/messages` | `{"text"?, "upload_ids"?, "reply_to_message_id"?, "force_rcs"?, "client_request_id"}` | `200` with `{operation, message_id}`. At least `text` or one upload. `force_rcs` is `invalid_request` unless the conversation is RCS with `send_mode=auto`. |
| `POST` | `/v1/conversations/{id}/typing` | `{}` | `204`. Fire-and-forget. Not idempotency-keyed; it has no lasting effect. |
| `POST` | `/v1/conversations/{id}/read` | `{"message_id", "client_request_id"}` | `200`. Marks the conversation read through that message. |
| `POST` | `/v1/messages/{id}/reactions` | `{"emoji", "client_request_id"}` | `200`. `SendReaction` with `ADD`, or `SWITCH` if the owner already has a different reaction on that message. `200` with `operation: null` if the owner already has exactly that reaction. |
| `DELETE` | `/v1/messages/{id}/reactions/{emoji}` | `?client_request_id=` | `200`. `REMOVE`. `200` with `operation: null` if there is nothing to remove. |
| `POST` | `/v1/uploads` | see §10.2 | `201` with an upload ticket |
| `GET`/`DELETE` | `/v1/uploads/{upload_id}` | — | the caller's own reservation |
| `PUT` | `/v1/uploads/{upload_id}/content` | raw bytes | authenticated by the **upload token**, not the access token |

`messages:delete` — and only these two:

| Method | Path | Body | Effect sentence (verbatim on all three surfaces) |
|---|---|---|---|
| `DELETE` | `/v1/messages/{message_id}` | `{"client_request_id"}` | *"deletes this message from your Google Messages account only; the recipient keeps it"* |
| `DELETE` | `/v1/conversations/{conversation_id}` | `{"client_request_id"}` | *"deletes this conversation from your Google Messages account only; the other people in it keep it"* |

There is no `mode` and no other delete. A request carrying `mode` anywhere is
`invalid_request` naming it.

`admin`:

| Method | Path | Notes |
|---|---|---|
| `GET`/`PATCH` | `/v1/admin/settings` | effective value, source (`default`\|`environment`\|`database`), mutability, restart requirement. `PATCH` validates the whole body; any invalid key rejects the request and changes nothing |
| `POST` | `/v1/admin/backfill` | `{"conversation_id"?}`; re-opens backfill |
| `POST` | `/v1/admin/backup` | writes `<data_dir>/backups/agent-gm-<ts>-<id>.sqlite3` via the SQLite backup API. The caller does not choose the path |
| `GET` | `/v1/admin/audit` | `kind`, `kind_prefix`, `authorization_id`, `after`, `before`, `cursor`, `limit` |
| `GET` | `/v1/admin/diagnostics` | the raw Google view: last 100 events by type, `dropped_events`, `unknown_events`, compiled/live ConfigVersion, `CurrentSessionID`, `IsBugleDefault`, upstream commit |
| — | `/v1/admin/enrollment-codes`, `/v1/admin/authorization-requests`, `/v1/admin/authorizations`, `/v1/admin/clients` | §9 |

### 7.7 `unsupported_capability` reasons

A closed vocabulary, in `details.reason`. Emitted **before** any operation row
exists, so a refused action never leaves a record that looks like an attempt.

| Reason | Meaning |
|---|---|
| `not_paired` | no Google Messages session |
| `conversation_read_only` | `Conversation.ReadOnly` is set |
| `conversation_deleted` | delete-for-me has been applied locally |
| `not_my_message` | reacting to or deleting something with the wrong ownership |
| `reply_not_supported` | `reply_to_message_id` on an SMS conversation; replies are RCS-only |
| `rcs_not_available` | `force_rcs` on a conversation that is not RCS |
| `media_pending` | the attachment's bytes are not downloaded yet |

An `unsupported_capability` answer carries the object ID, the action, the
capability's current value, and `details.reason`.

---
