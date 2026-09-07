# Contributing

Agent GM is a personal project. It is built for exactly one owner — with as
many of their own Google accounts as they have phones — and its scope is
fixed: MCP, REST and a CLI over those Google Messages accounts. It is public;
it is not looking for users.

That means:

- **No feature requests.** Anything outside `plans/AGENT_GM_SPEC.md` §1
  "Non-goals" will be closed. Multi-*user*, multi-provider, a web UI and a
  human-facing chat client are permanent non-goals. (Multi-*account* is
  supported: several Google accounts, one owner.)
- **Bug reports are welcome** if they include the version, the failing command,
  and the redacted log line. Please never paste phone numbers, message bodies,
  tokens or the contents of a session file into an issue.
- **Pull requests** are accepted only for correctness fixes, upstream `libgm`
  pin bumps, and documentation. Open an issue first; an unsolicited PR that
  changes a public contract (a REST route, an MCP tool schema, an error code,
  a CLI exit code) will be closed on sight — those contracts are consumed by
  agents that cannot be asked to adapt.
- **The spec is the contract.** `plans/AGENT_GM_SPEC.md` wins over the code. If
  the code disagrees with the spec, that is the bug.

Development happens through Devbox only — see `plans/AGENT_GM_SPEC.md` §13.6.
Run `devbox run check` before proposing anything. Do not install tooling on the
host; add it to `devbox.json`.

Security issues: do not open a public issue. Email the address on the owner's
GitHub profile. Spec §12 is the threat model.

Never put a real phone number, a message body, a token, a Google cookie or
anything from a session file in an issue, a pull request, a test fixture or a
commit message. CI enforces the phone-number half of that.

Agent GM is licensed under AGPL-3.0-or-later; contributions are accepted under
the same licence.
