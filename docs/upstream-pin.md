# Upstream pins

Serves spec §3.6. Every dependency Agent GM pins by an exact revision is
recorded here, one row per pin, plus the entry each bump adds. The bump
procedures are in [operations.md](operations.md#the-libgm-pin-policy); this
page is the record they write to.

## The pins today

| Dependency | Pinned | Why this one | Bumped on |
|---|---|---|---|
| `go.mau.fi/mautrix-gmessages` (`pkg/libgm`) | `be48a58b733825f6dfd6bb630af5f236d3bc9ae8` (`be48a58`), `ConfigVersion` 2026.9.2 | The Google Messages client library. Its `ConfigVersion` must be current enough for Google to accept conversation creation | initial pin, `v1.0.0` |
| `github.com/modelcontextprotocol/go-sdk` | `v1.7.0` | The MCP protocol layer. It decides the JSON-RPC framing, the `WWW-Authenticate` challenge on `/mcp` and the RFC 9728 handler | initial pin, `v1.0.0` |
| Node | `22` (`devbox.json`, and `actions/setup-node` in `release.yml`) | Runs the `@agent-gm/cli` postinstall shim and packs the npm tarball | initial pin, `v1.0.0` |
| npm | `>= 11.5.1`, installed into a scratch prefix by `release.yml` and passed to `scripts/release.sh` as `AGENT_GM_NPM` | Trusted publishing (OIDC) is not implemented before 11.5.1: an older npm publishes unauthenticated and the registry answers `404`. Node 22 ships npm 10 | `v1.0.1` |

The `libgm` pin is the only one recorded in three places — `go.mod`,
`internal/gm/pin.go` and spec §3.6 — and `devbox run pin-consistency` fails if
they diverge. `GOFLAGS=-mod=readonly` is set so an accidental `go get` cannot
move any of them.

## Bumping a pin

For libgm, begin with the [maintained patch and reapplication guide](../third_party/mautrix-gmessages/AGENT_GM_PATCHES.md).
The local `replace` means the imported source must be updated as well as the
recorded hashes. The guide distinguishes direct library tests, adapter tests,
live validation, and known gaps; verify reconstruction against each new base.

The full procedures, with the commands, are in
[operations.md](operations.md#the-libgm-pin-policy) (`libgm`) and
[operations.md](operations.md#the-go-sdk-pin) (`go-sdk`). In outline:

- **`libgm`** — update all three places in one commit; diff `pkg/libgm` and
  `pkg/connector` between the old and new commits; `devbox run
  fixture-validation`; `devbox run check && devbox run conformance`; then a
  **live gate** run by the maintainer by hand — pair, list, send one text,
  receive a reply.
- **`go-sdk`** — update `go.mod`; `devbox run check`; then `devbox run
  conformance`, which is the gate, checked against
  `scripts/mcp-conformance-baseline.yaml` in both directions. No live gate:
  nothing in the SDK touches Google.
- **Node or npm** — `devbox run release-dry-run`, which packs the tarball and
  installs it for real, is the whole gate.

A bump is its own change, never a drive-by commit, and never part of a routine
upgrade.

## What a bump entry records

Add a section below on every bump, so the next reader can see what moved
without cloning anything:

```markdown
## <dependency> <old> → <new> (<release>)

- **`ConfigVersion`**: <before> → <after>.               (libgm only)
- **Symbols in spec §3.1 that changed**: <each one, or "none">.
- **Delivery-state and event vocabulary**: every added, removed or renamed
  enum value, or "none".
- **Wire behaviour that changed**: status codes, headers, error codes.
- **Gate**: what was run and what it did — for `libgm`, the live gate, with
  what was sent and what came back.
```

A bump with no entry here is a bump nobody can review later.

## Bumps

None yet beyond the initial pins above. `libgm` and `go-sdk` are at the
revisions Agent GM `v1.0.0` shipped with; npm moved at `v1.0.1` for the reason
in the table.
