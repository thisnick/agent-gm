# Contributing

Agent GM is a personal project. It is built for exactly one owner, one Google
account and one paired phone, and its scope is deliberately fixed: MCP, REST
and a CLI over that one Google Messages account. It is public because the
AGPL-3.0 requires it (Agent GM links `go.mau.fi/mautrix-gmessages/pkg/libgm`),
not because it is looking for users.

That means:

- **No feature requests.** Anything outside `plans/AGENT_GM_SPEC.md` §1
  "Non-goals" will be closed. Multi-user, multi-provider, a web UI, and a
  human-facing chat client are all permanent non-goals.
- **Bug reports are welcome** if they include the version, the failing command,
  and the redacted log line. Please never paste phone numbers, message bodies,
  tokens or the contents of `session.enc` into an issue.
- **Pull requests** are accepted only for correctness fixes, upstream `libgm`
  pin bumps, and documentation. Open an issue first; an unsolicited PR that
  changes a public contract (a REST route, an MCP tool schema, an error code,
  a CLI exit code) will be closed on sight, because those contracts are
  consumed by agents that cannot be asked to adapt.
- **The spec is the contract.** `plans/AGENT_GM_SPEC.md` wins over the code. If
  the code disagrees with the spec, that is the bug.

Development happens through Devbox only — see `plans/AGENT_GM_SPEC.md` §14. Run
`devbox run check` before proposing anything. Do not install tooling on the
host; add it to `devbox.json`.

Security issues: do not open a public issue. See `docs/security.md`.
