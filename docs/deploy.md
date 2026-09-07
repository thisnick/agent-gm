# Deploying Agent GM

Serves spec §14.1, §14.2, §15.1, §15.2 and §1.4.

Two binaries, two names: **`agent-gm`** is the server (the thing in the
container: `agent-gm serve`, `agent-gm healthcheck`), and **`agm`** is the
command-line client you run from anywhere (`agm auth login`, `agm admin
backup`), shipped separately as the `@agent-gm/cli` npm package. Every
`agent-gm …` command on this page runs inside the container; every `agm …`
command runs wherever you are.

This page is for a **generic host**: one machine, Docker, a public HTTPS URL
in front. It says what the repository ships, what the host must supply, and
what to pin. Configuration knobs, the runtime settings table, backup mechanics
and the 3am runbook are in [operations.md](operations.md); this page points at
them rather than repeating them.

No particular deployment lives in this repository: a deployment is a fact about
one machine. What the repository ships is a Dockerfile, a Compose example, an
environment table and these docs.

## What Agent GM needs from a host

1. **A public HTTPS URL that terminates TLS.** Agent GM serves plain HTTP and
   has no certificate of its own. A tunnel or a reverse proxy provides the
   `https://` origin, and that origin is `AGENT_GM_PUBLIC_URL`.
2. **Persistent storage for `/data`.** Losing it means re-pairing every
   account.
3. **Three secrets or near-secrets**, below.
4. **Nothing else.** No database server, no Redis, no sidecar. One binary,
   one SQLite file.

Agent GM must not be exposed to the internet directly. It has no TLS, and its
OAuth issuer, rate limiting and client-source resolution all assume the URL
clients use is the one configured, not one derived from a `Host` header.

## The image

```
ghcr.io/thisnick/agent-gm
```

Built by CI from [`../Dockerfile`](../Dockerfile) on every push. Two stages:
`golang:1.27` builds, and the runtime is
`gcr.io/distroless/static-debian12:nonroot` — no libc, no shell, no package
manager, no `curl`. That is possible because `CGO_ENABLED=0` and the SQLite
driver is pure Go (`modernc.org/sqlite`), and it is the reason the container's
health probe is a subcommand of the program (below) rather than a `curl`
one-liner.

The image runs as `USER nonroot:nonroot` (uid 65532), declares
`VOLUME ["/data"]`, exposes `8080`, and presets
`AGENT_GM_DATA_DIR=/data` and `AGENT_GM_LISTEN_ADDR=0.0.0.0:8080`. `/data`
exists **in the image**, owned by 65532: a volume mounted onto a path the
image does not contain is created by the daemon as `root:root`, which a
non-root process cannot write, and the server would fail to open its database
on the first start.

Architectures: `linux/amd64` and `linux/arm64`, as one manifest list.

### Tags

| Tag | When it moves |
|---|---|
| `sha-<short>` | one build; never moves |
| `main` | every push to `main` |
| `vX.Y.Z` | one release; never moves |
| `vX.Y` | every patch release in that minor |
| `latest` | every release |

**Pin by digest, not by tag.** A tag is a label and a digest is evidence:

```yaml
image: ghcr.io/thisnick/agent-gm@sha256:<digest>
```

Every release note records the digest and the upstream `libgm` commit the
build was made from (§3.6). `docker buildx imagetools inspect
ghcr.io/thisnick/agent-gm:vX.Y.Z` prints the digest of a published tag, and
`scripts/image.sh push` prints it at the end of a CI publish.

`:latest` silently changes what you are running; a digest does not.

### Building it yourself

```bash
devbox run image          # both architectures, no push
devbox run image-smoke    # linux/amd64, loaded locally, then the assertions below
```

Both call [`../scripts/image.sh`](../scripts/image.sh), which is what CI runs,
so the image CI publishes is the image you can build. `VERSION` and `COMMIT`
are build arguments and reach the binary as `-X main.version` and
`-X main.commit`. A container build has no `.git` and cannot read a VCS stamp,
so without them the binary reports commit `unknown`, and the commit is part of
the source offer below.

## Compose

[`../compose.example.yml`](../compose.example.yml) is a **generic example**,
and it runs with nothing but the three required variables:

```bash
export AGENT_GM_PUBLIC_URL=https://gm.example.com
export AGENT_GM_ADMIN_SECRET=$(openssl rand -base64 48)
export AGENT_GM_DATA_KEY=$(openssl rand -hex 32)
docker compose -f compose.example.yml up -d --wait
curl -fsS http://localhost:8080/healthz     # {"status":"ok"}
```

It builds from the checkout so the example works with no registry access; a
real host replaces the `build:` block with a digest-pinned `image:`. It also
carries a commented-out **Cloudflare Tunnel sidecar**: uncomment it, supply
`CLOUDFLARE_TUNNEL_TOKEN`, point the tunnel's public hostname at the origin
`http://agent-gm:8080` (the service name on the Compose network, not
`localhost`), and drop the published port — otherwise the tunnel is a
protected way in and the port is an unprotected one.

`read_only: true` and `no-new-privileges` are set in the example and are safe:
the process writes nothing outside `/data`.

## Environment

The full table, with every optional variable and every runtime setting, is in
[operations.md](operations.md#configuration). A host must supply exactly
three; everything else has a default that works.

| Variable | Required | What a host must decide |
|---|---|---|
| `AGENT_GM_PUBLIC_URL` | **yes** | The public origin, e.g. `https://gm.example.com`. It is the OAuth issuer, the canonical MCP resource, and the base of every URL Agent GM hands out. **Never derived from the `Host` header.** Changing it later is a migration, not an edit — see [operations.md](operations.md#changing-the-public-url) |
| `AGENT_GM_ADMIN_SECRET` | **yes** | The owner's bootstrap credential, **≥43 characters**. The server refuses to start with a shorter one |
| `AGENT_GM_DATA_KEY` | **yes** | 256 bits, as 64 hex characters or standard base64. It seals every account's session file. **Not rotatable** — there is no in-place rotation, and losing it means re-pairing every account |
| `AGENT_GM_LISTEN_ADDR` | no | Preset to `0.0.0.0:8080` in the image. Changing it means changing the port mapping and nothing else — the healthcheck reads the same variable |
| `AGENT_GM_DATA_DIR` | no | Preset to `/data` in the image |
| `AGENT_GM_TRUSTED_PROXY_CIDRS` | no | Set **only** if a proxy in front is trusted to set `X-Forwarded-For`. An invalid value refuses to start. Empty means rate limiting keys on the peer address, which is the safe default |
| `AGENT_GM_LOG_LEVEL` / `AGENT_GM_LOG_FORMAT` | no | `info` and `json` |

Generate the two secrets with `openssl rand -base64 48` and
`openssl rand -hex 32` (or `devbox run gen-secret`), store them wherever the
host keeps secrets, and **back up the data key with the data** — see below.

## The healthcheck

```
agent-gm healthcheck [--addr <host:port>] [--timeout <duration>]
```

A subcommand of the program itself. It GETs `/healthz` against
`AGENT_GM_LISTEN_ADDR` (default `0.0.0.0:8080`, rewritten to loopback because
a wildcard bind address is not a destination), and exits **0** on `200` and
**7** on anything else — a non-200, a refused connection, a timeout, a
redirect away from `/healthz`. It follows no redirects: a probe asks one
server one question.

The runtime image has no shell and no `curl`, so a `HEALTHCHECK` there can
only execute a binary, and this is the only binary in the image. The
`HEALTHCHECK` is declared in the Dockerfile and
repeated in the Compose example, so `docker compose up --wait` and
`depends_on: condition: service_healthy` work.

It probes `/healthz` and **not** `/v1/health`. `/healthz` needs no token and
never touches SQLite, so it answers during a migration; `/v1/health` reports
account-level conditions, and an account that is signed out is not an
unhealthy container. That distinction is the single most important thing about
monitoring Agent GM and is spelled out in
[operations.md](operations.md#health-and-diagnosis).

## `/data`

One volume, four things:

| Path | What | If you lose it |
|---|---|---|
| `agent-gm.sqlite3` (+ `-wal`, `-shm`) | conversations, messages, accounts, OAuth state, settings | Everything except the pairings |
| `sessions/` | one encrypted file per account — the credential that keeps the phone paired | Every account must be re-paired |
| `media-cache/` | a bounded LRU of downloaded attachments | Nothing; it refills |
| `backups/` | snapshots written by `POST /v1/admin/backup` | The snapshots |

Message text is **not** encrypted at rest. The file mode is the at-rest model,
which is why the server warns when something outside it has loosened
permissions on the data directory. Treat the volume as sensitive.

### Backup and restore

Mechanics, retention and the restore procedure are in
[operations.md](operations.md#backup-and-restore). The two facts a host must
carry into its own backup design:

1. **A complete backup is three things and they move together**: the snapshot
   (or all of `data/`), the whole `sessions/` directory, and
   `AGENT_GM_DATA_KEY`. Without the third, the second is unreadable.
2. **Never copy the database file while the server is running.** A copy of a
   WAL-mode database without its log opens perfectly well and is quietly out
   of date. Use `agm admin backup`, which uses SQLite's own backup API and
   produces a snapshot with no `-wal` sidecar.

`agm admin backup` writes into `/data/backups/` **inside the volume**, so a
host-side backup job still has to copy it out — `docker cp` from the running
container, or a throwaway container with the volume mounted read-only:

```bash
docker run --rm -v agent-gm-data:/data:ro -v "$PWD:/out" alpine:3 \
  sh -c 'cp /data/backups/$(ls -1 /data/backups | tail -1) /out/ && cp -a /data/sessions /out/'
```

Store `AGENT_GM_DATA_KEY` somewhere else. Those two in one place are the whole
Google account.

## Upgrading

Change the pinned digest, `docker compose up -d`, done: migrations run
forward on start, and the database is not readable by an older binary once
they have. Take a backup first, keep the previous digest — that is the
rollback — and read [operations.md](operations.md#upgrading), which covers
what a rollback can and cannot undo and the `libgm` pin policy.

## Releases and their artefacts

A GitHub release for `vX.Y.Z` carries, from this public repository, so no
token is needed to download any of it:

```text
agent-gm_X.Y.Z_linux_amd64.tar.gz     the server
agent-gm_X.Y.Z_linux_arm64.tar.gz
agm_X.Y.Z_linux_amd64.tar.gz          the CLI, four platforms
agm_X.Y.Z_linux_arm64.tar.gz
agm_X.Y.Z_darwin_amd64.tar.gz
agm_X.Y.Z_darwin_arm64.tar.gz
checksums.txt
checksums.txt.sig                     cosign keyless, GitHub OIDC
checksums.txt.pem
```

Every archive holds the binary, `LICENSE` and `README.md`. Windows is not in
the v1 matrix.

Verify before you run anything:

```bash
shasum -a 256 -c checksums.txt --ignore-missing

cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-identity-regexp 'https://github.com/thisnick/agent-gm/\.github/workflows/release\.yml@refs/tags/v.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The signature is keyless, so there is no public key to fetch and no private
key in this repository. The identity it binds is the workflow file at the tag,
which is what `--certificate-identity-regexp` above checks — read that line
rather than pasting it, because a signature that verifies against the wrong
identity has told you nothing.

**The release notes carry the GHCR digest** of the image built from the same
commit, together with the `libgm` and `go-sdk` pins. Deploy the digest.

The CLI is also on npm as `@agent-gm/cli`, a thin wrapper that downloads the
matching `agm` archive and verifies it against a `checksums.txt` pinned inside
the npm tarball; see [cli.md](cli.md#installing).

## The source offer

Agent GM is licensed under **AGPL-3.0-or-later** and is reachable over a
network, so whoever interacts with it must be offered the corresponding source.
Agent GM serves that offer itself. `GET /v1/health` reports `commit` and
`source_url`; the MCP `serverInfo` reports the same two facts in the two fields
the protocol has for them — the commit as **semver build metadata on
`version`** (`0.1.0+abc1234`), and the source URL as `websiteUrl`. Both point
at `https://github.com/thisnick/agent-gm` at the **exact built commit**.

A build that reports `unknown` does not meet it. Verify on a deployed
container:

```bash
docker run --rm ghcr.io/thisnick/agent-gm@sha256:<digest> version
# agent-gm <version> (<commit>, libgm pinned at <pin>)
```

and the same commit appears in `GET /v1/health`. If you deploy a modified
build, the offer must point at **your** source, not at this repository.
