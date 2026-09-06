# Agent GM

MCP and CLI access to your own Google Messages account, paired directly with
your phone through the Google Messages web protocol. One Go binary, one
owner, one phone. No Matrix, no bridge, no UI for humans.

It imports the Google Messages client library from
[mautrix-gmessages](https://github.com/mautrix/gmessages) (`pkg/libgm`,
pinned by commit), keeps its state in SQLite, and serves three surfaces over
the same core:

- **MCP** — streamable HTTP at `/mcp`, OAuth 2.1, for claude.ai and ChatGPT
  connectors and for local agents.
- **REST** — JSON under `/v1`, for scripts and for the CLI.
- **CLI** — `agm`, published on npm as `@agent-gm/cli`.

Status: **design phase**. No code yet.

## Where things are

| | |
|---|---|
| The specification, and the contract | [`plans/AGENT_GM_SPEC.md`](plans/AGENT_GM_SPEC.md) |
| What each doc page will cover | [`docs/README.md`](docs/README.md) |
| Whether to send a pull request | [`CONTRIBUTING.md`](CONTRIBUTING.md) — short answer: this is a personal project |

Start with spec §1 (goals and non-goals) and §3 (the Google layer contract).
§16 is the build order, four slices, each with its acceptance tests.

## Development

Devbox only. Do not install tooling on the host; add it to `devbox.json`.

```sh
devbox run check    # build, vet, lint, test -race
devbox run test
devbox run lint
devbox run build
```

`make check` and friends are thin wrappers over the same scripts.

## Licence

AGPL-3.0-or-later, because Agent GM links `libgm`, which is AGPL-3.0, and no
upstream exception covers this project. Agent GM is reachable over a network,
so AGPL §13 applies: it serves its own source URL and built commit from
`GET /v1/health` and from the MCP `serverInfo`. That is why this repository is
public. Spec §1.4.
