# `@agent-gm/cli`

`agm` is the command line for [Agent GM](https://github.com/thisnick/agent-gm),
a single-binary server that gives an agent Google Messages (SMS, MMS and RCS)
over REST and MCP.

```sh
npm i -g @agent-gm/cli
agm auth login --server https://gm.example.test
agm conversations list
```

## What this package is

A **thin wrapper**, not a bundle. Installing it downloads the `agm` archive
for your platform from the matching GitHub release and verifies it against a
copy of `checksums.txt` that was pinned inside this npm tarball when it was
published — so a release page edited afterwards cannot hand an
already-published version a different binary.

The package version equals the Go release version: `@agent-gm/cli@1.4.2`
installs `agm 1.4.2`.

Supported platforms are `linux/x64`, `linux/arm64`, `darwin/x64` and
`darwin/arm64`. Anything else fails the install with a message naming the
platform, rather than leaving a shim that cannot run.

## Environment

| Variable | Effect |
|---|---|
| `AGENT_GM_CLI_SKIP_DOWNLOAD=1` | skip the postinstall download, for CI images that supply the binary themselves |
| `AGENT_GM_CLI_BINARY=<path>` | run this binary instead of the vendored one |

Everything else is `agm`'s own; see
[docs/cli.md](https://github.com/thisnick/agent-gm/blob/main/docs/cli.md).

## Exit codes

The shim propagates the exit code exactly. `agm --nonsense` exits `2` through
npm as it does natively, and `1` stays unassigned so that a `1` is always the
wrapper's, the shell's or the runtime's — never Agent GM's.

## Licence

Licensed under AGPL-3.0-or-later. This package distributes AGPL binaries; the
corresponding source is <https://github.com/thisnick/agent-gm> at the commit
named in the release notes and reported by `agm version`.
