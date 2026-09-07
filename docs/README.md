# Agent GM documentation

The specification is `../plans/AGENT_GM_SPEC.md`. It is the contract: where
the code and the spec disagree, the code is wrong. These pages are written
against it and each one names the spec sections it serves.

The pages are deliverables of the slices in spec §16. **Seven are written:**
`pairing.md`, `api.md`, `cli.md`, `operations.md`, `mcp.md`, `oauth.md` and
`deploy.md`. **One is outstanding:** `upstream-pin.md`, which has nothing to
record until a pin is bumped.

Two things live outside this directory and are looked for here often enough
to be worth naming: `../CHANGELOG.md`, which records each release with its
GHCR digest and both dependency pins, and `../npm/README.md`, which is what
an npm reader sees for `@agent-gm/cli`.

The table below is what each page owes, so that a page is reviewed against its
brief rather than against itself.

## The pages

| Page | Slice | Spec sections it serves |
|---|---|---|
| `pairing.md` | 2 | §3.2, §4.7, §11.4 — the pairing flow, the account model (adding, logging out, removing, re-pairing), exactly what the owner does on the phone, the two phone settings that must be right (default SMS app; group messaging set to MMS), and how to recover when an account's cookies expire |
| `api.md` | 2 | §7, §10 — every REST route, its parameters, its DTOs, the error envelope and the full code table, pagination, **the account-selection rule of §7.3**, idempotency, and the media ticket flow with its exact `curl` commands |
| `cli.md` | 2 | §11 — every `agm` command including the `agm accounts` subtree, the global flags including `--account` and `AGENT_GM_ACCOUNT`, the exit-code table, `--wait`/`--wait-for`, credential precedence and write-back safety |
| `operations.md` | 2 | §12, §15 — the environment table, the settings table with bounds **and per-account/server scope**, backup and restore (the three things that move together), health and diagnosis, and the runbook |
| `mcp.md` | 3 | §8 — the tool catalogue with schemas and annotations, `isError` semantics, scope gating, and **`list_accounts` and the account section of the instructions block**, and a **"First five minutes"** section byte-identical to the served `instructions` block (a test asserts it) |
| `oauth.md` | 3 | §9 — discovery documents, DCR, the authorization screen, enrollment codes, owner approval, tokens, revocation, and the budgets on unauthenticated endpoints |
| `deploy.md` | 3 | §14, §15 — the Dockerfile, the generic compose example, GHCR by digest, the `agent-gm healthcheck` probe, `/data`, and what a host must supply. Pulled forward from Slice 4 by decision D35: the connector gate runs against the container, so the container ships in Slice 3 |
| `upstream-pin.md` | every pin bump (spec §3.6) | §3.6 — the pinned `libgm` commit, what each bump changed in the §3.1 surface, and the live gate that accepted it |

## Two things to read before anything else

**The `libgm` pin.** Agent GM imports `go.mau.fi/mautrix-gmessages/pkg/libgm`
at commit `be48a58` (`ConfigVersion` 2026-09-02). That pin is the single most
load-bearing dependency fact in the project: a stale `ConfigVersion` breaks
conversation creation with an undocumented status 4 and no other symptom.
Spec §3.6 and §3.7.

**The approved-test-number rule.** Live sends go only to `<APPROVED_DIRECT_NUMBER>`
(direct) and `<APPROVED_GROUP_NUMBER_1>` / `<APPROVED_GROUP_NUMBER_2>` (group), and only the
coordinator sends live. Fictional `555` numbers are for examples and fixtures
and are never dialled. Spec §13.3.
