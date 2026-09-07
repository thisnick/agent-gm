# Agent GM documentation

These pages describe Agent GM as it runs. The specification is
`../plans/AGENT_GM_SPEC.md`: it is the contract, and where the code and the
spec disagree the code is wrong. Each page below names the spec sections it
serves, so a claim can be traced back to the clause it comes from.

| Page | What it covers |
|---|---|
| [`pairing.md`](pairing.md) | Adding a Google account, what happens on the phone, the two phone settings that must be right, refreshing cookies, signing out, removing, re-pairing, and every pairing failure |
| [`api.md`](api.md) | Every REST route with its scope and parameters, the envelopes, the error-code table, pagination, idempotency, the account-selection rule, and the media ticket flow |
| [`cli.md`](cli.md) | Every `agm` command and flag, the exit-code table, `--wait`/`--wait-for`, credentials and profiles |
| [`mcp.md`](mcp.md) | The tool catalogue with schemas and annotations, the transport, `isError` semantics, scope gating, and the instructions block the server serves |
| [`oauth.md`](oauth.md) | Discovery, dynamic client registration, the authorization screen, enrollment codes, owner approval, tokens, revocation, and the budgets on unauthenticated endpoints |
| [`deploy.md`](deploy.md) | The image, the Compose example, pinning by digest, the healthcheck, `/data`, and release artefacts |
| [`operations.md`](operations.md) | The environment and settings tables, backup and restore, health and diagnosis, the runbook, the pin policy and the release procedure |

Two things live outside this directory and are looked for here often enough to
be worth naming: `../CHANGELOG.md`, which records each release with its GHCR
digest and both dependency pins, and `../npm/README.md`, which is what an npm
reader sees for `@agent-gm/cli`.

## One thing to read before anything else

**The `libgm` pin.** Agent GM imports `go.mau.fi/mautrix-gmessages/pkg/libgm`
at commit `be48a58` (`ConfigVersion` 2026-09-02). It is the most load-bearing
dependency fact in the project: a stale `ConfigVersion` breaks conversation
creation with an undocumented status and no other symptom. The pin agrees in
`go.mod`, `internal/gm/pin.go` and spec §3.6, and CI fails if they diverge.
[`operations.md`](operations.md#the-libgm-pin-policy) has the bump procedure.
