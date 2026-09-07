# Agent GM

MCP and CLI access to your own Google Messages. One Go binary, one owner, as
many Google accounts as you have. It pairs directly with the phone, the way
the Google Messages web client does; there is no bridge and no UI for humans.

It imports the Google Messages client library from
[mautrix-gmessages](https://github.com/mautrix/gmessages) (`pkg/libgm`, pinned
by commit), keeps its state in one SQLite file, and serves three surfaces over
the same core:

- **MCP** — streamable HTTP at `/mcp`, OAuth 2.1, for claude.ai and ChatGPT
  connectors and for local agents. [`docs/mcp.md`](docs/mcp.md)
- **REST** — JSON under `/v1`, for scripts and for the CLI.
  [`docs/api.md`](docs/api.md)
- **CLI** — `agm`, from npm or from a GitHub release binary.
  [`docs/cli.md`](docs/cli.md)

The CLI is a client of the REST API and nothing else, so anything `agm` can do
an agent can do, and the reverse.

## Quickstart

Three secrets, one container, one pairing.

```sh
# 1. Run the server. It needs a public HTTPS origin in front of it.
export AGENT_GM_PUBLIC_URL=https://gm.example.com
export AGENT_GM_ADMIN_SECRET=$(openssl rand -base64 48)   # >= 43 characters
export AGENT_GM_DATA_KEY=$(openssl rand -hex 32)          # 256 bits, NOT rotatable
docker compose -f compose.example.yml up -d --wait
curl -fsS http://localhost:8080/healthz                   # {"status":"ok"}

# 2. Get a credential for the machine you drive it from.
npm i -g @agent-gm/cli
printf %s "$AGENT_GM_ADMIN_SECRET" |
  agm auth login --admin --server https://gm.example.com --secret-stdin

# 3. Pair a Google account. Chrome opens; you sign in and tap an emoji on the phone.
agm pair
agm conversations list
```

Back up `AGENT_GM_DATA_KEY` with the data directory. It cannot be rotated, and
without it a restored `sessions/` directory is unreadable.

Then: [`docs/deploy.md`](docs/deploy.md) for a real host,
[`docs/pairing.md`](docs/pairing.md) for what happens on the phone, and
[`docs/oauth.md`](docs/oauth.md) for giving a connector a token.

## Documentation

| | |
|---|---|
| [`docs/README.md`](docs/README.md) | index of the pages below |
| [`docs/pairing.md`](docs/pairing.md) | adding, refreshing, signing out and removing a Google account |
| [`docs/api.md`](docs/api.md) | every REST route, DTO and error code |
| [`docs/cli.md`](docs/cli.md) | every `agm` command, flag and exit code |
| [`docs/mcp.md`](docs/mcp.md) | the MCP tool catalogue and transport |
| [`docs/oauth.md`](docs/oauth.md) | the authorization server, enrollment codes and approval |
| [`docs/deploy.md`](docs/deploy.md) | the image, Compose, `/data`, releases |
| [`docs/operations.md`](docs/operations.md) | configuration, backup and restore, the runbook |
| [`CHANGELOG.md`](CHANGELOG.md) | each release with its GHCR digest and both pins |
| [`plans/AGENT_GM_SPEC.md`](plans/AGENT_GM_SPEC.md) | the specification, which is the contract |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | whether to send a pull request — short answer: this is a personal project |

## Development

Devbox only. Do not install tooling on the host; add it to `devbox.json`.

```sh
devbox run check    # build, vet, lint, test -race, and the repository lints
devbox run test
devbox run lint
devbox run build
```

`make check` and friends are thin wrappers over the same scripts.

## Licence

Licensed under AGPL-3.0-or-later. Agent GM is reachable over a network, so it
offers its own corresponding source: `GET /v1/health` reports `commit` and
`source_url`, and the MCP `serverInfo` reports the same two facts as semver
build metadata on `version` and as `websiteUrl`. If you deploy a modified
build, that offer must point at your source.
