# Changelog

Agent GM follows [semantic versioning](https://semver.org). The public
contract is the REST API (`docs/api.md`), the MCP tool catalogue
(`docs/mcp.md`), the OAuth surface (`docs/oauth.md`), the `agm` command line
and its exit codes (`docs/cli.md`), and the environment and settings tables
(`docs/operations.md`). A breaking change to any of those is a major version.

Two facts are recorded for every release, because neither is answerable from
the tag alone: the **GHCR digest** of the image built from that commit, and
the **pins** — `go.mau.fi/mautrix-gmessages` (libgm) and
`github.com/modelcontextprotocol/go-sdk`. Deployments pin by digest, not by
tag: a tag is a label and a digest is evidence.

## [Unreleased]

Nothing yet.

## [1.0.2] — 2026-09-07

- `agm auth login --admin` prints a hint when it grants all four scopes; admin sessions are narrowed with `--scopes`
- Versions are managed with changesets; the container, binaries and npm package share one version

## [1.0.1] — 2026-09-07

- Releases publish `@agent-gm/cli` through npm trusted publishing (OIDC);
  the one-time `NPM_TOKEN` used for 1.0.0 is gone. No runtime change.

## [1.0.0] — 2026-09-07

The first release. Everything below is new, so this entry describes what
Agent GM *is* rather than what changed.

| | |
|---|---|
| Container image | `ghcr.io/thisnick/agent-gm:v1.0.0` — the digest is in the GitHub release notes |
| `libgm` pin | `be48a58b733825f6dfd6bb630af5f236d3bc9ae8` (`ConfigVersion` 2026.9.2, V1=4 V2=6) |
| `go-sdk` pin | `v1.7.0` |
| npm | `@agent-gm/cli@1.0.0` |

### Google Messages, as an API

- One binary, `agent-gm`, that pairs with Google Messages over the Google
  account (gaia) flow, backfills history, ingests live, and serves it.
- **Several accounts** in one server, each with its own session file, its own
  state machine and its own backfill. Identifiers are derived from the Google
  account address rather than from the phone, so re-pairing — including onto a
  different handset — keeps every existing `conv_` and `msg_` ID.
- Sends are **synchronous and have no outbox**: a write either reaches Google
  or returns an error naming why. Every mutation returns an **operation with a
  server-minted id**, and status is checked by that id.
- **An idempotency key is optional**, and its only transport is the
  `Idempotency-Key` header. Sending one makes a repeat with the same body a
  replay that sends nothing; not sending one is the ordinary case, because an
  agent regenerates its arguments on a retry and cannot supply a stable key
  across one. If a call's result is lost, the instruction is to **look before
  sending again** — read the conversation, or list operations.
- SMS, MMS and RCS, with reactions, replies, read receipts, typing, media up
  and down, contacts and full-text search.

### Three ways in

- **REST** (`/v1`) — every route documented in `docs/api.md`, with one error
  envelope and one error-code table.
- **MCP** over streamable HTTP, with a tool catalogue an agent can read cold,
  and a conformance run against a pinned baseline in both directions.
- **`agm`**, the command line: every `/v1` route has a command, every route
  parameter has a flag, and every error code maps onto exactly one of the ten
  exit codes. `1` is deliberately unassigned, so a `1` is never Agent GM's.

### Security

- OAuth 2.1 with PKCE, dynamic client registration, enrollment codes and
  explicit owner approval. Tokens are audience-bound; refresh tokens rotate.
- Sessions are sealed with `AGENT_GM_DATA_KEY` and are a backup of the
  owner's **Google account credentials** — handled, and documented, as such.
- Logs redact by construction: no message bodies, no cookies, no tokens. A CI
  job scans the whole tree for anything resembling a real phone number.

### Packaging and operations

- Distroless container image, `nonroot`, no shell and no curl; the
  healthcheck is a subcommand. Multi-arch `linux/amd64` and `linux/arm64`.
- Release archives for `linux/{amd64,arm64}` and, for `agm`, `darwin/{amd64,
  arm64}`, with `checksums.txt` signed by cosign keyless over GitHub OIDC.
- `@agent-gm/cli` on npm: a thin wrapper that downloads the matching archive
  and verifies it against a `checksums.txt` pinned inside the npm tarball, so
  a release page compromised after publication cannot serve an already
  published version a different binary.
- Backup by `VACUUM INTO` inside the database's own transaction, and a
  **restore drill** (`devbox run restore-drill`) that CI runs on every push —
  including the documented failure mode, where the key is missing and every
  account comes back `signed_out` with its history intact.

### Known limits

- **Windows is not in the release matrix.** The cross-compile is kept green in
  CI, so it is a decision rather than a repair job, but no Windows archive is
  published.
- **`AGENT_GM_DATA_KEY` is not rotatable.** There is no in-place rotation and
  none is planned; losing it costs the sessions, not the history.
- **`AGENT_GM_PUBLIC_URL` cannot be changed casually.** It is a migration:
  every token is audience-bound and every registered client is tied to the
  old origin. See `docs/operations.md`.
- **Downgrade is not supported.** A database at a higher `user_version`
  refuses to open. Snapshot before an upgrade.
- claude.ai and ChatGPT connectors are **optional**: the third-party client
  gate was satisfied through the public URL with Codex CLI. Adding either
  later needs no code.

[Unreleased]: https://github.com/thisnick/agent-gm/compare/v1.0.2...HEAD
[1.0.2]: https://github.com/thisnick/agent-gm/releases/tag/v1.0.2
[1.0.1]: https://github.com/thisnick/agent-gm/releases/tag/v1.0.1
[1.0.0]: https://github.com/thisnick/agent-gm/releases/tag/v1.0.0
