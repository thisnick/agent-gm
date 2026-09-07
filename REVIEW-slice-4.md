# Review — Slice 4 (`121648d..a9f5626`, PR #1)

Reviewer worktree `.claude/worktrees/review-slice-4`, branch `review/slice-4`,
reset to `a9f5626`. Every command below was run by me under Devbox. No live
send, no real phone number, no pairing, no tag, no publish; the only ports
bound were ephemeral loopback ports the OS handed out (`45449`, `36985` and
the drill's own), never `8080/8081/8090/8787`, and every server was signalled
by a PID captured at launch.

## Verdict

**Accept with required fixes.** The slice is real: the wrapper does what §14.3
says under a real `npm install`, the D38 contract holds on a live server, the
restore drill restores and fails loudly when it should. But the **tag path**
— the half of the release that CI can never run — has no guard against three
mistakes I was able to plant and watch survive a full `devbox run check` and a
full `devbox run release-dry-run`.

**PR #1 may merge** once R-1 and R-2 land (both are one line each in
`scripts/release.sh`) and the two tests I wrote are kept.

**`v1.0.0` may NOT be tagged yet.** R-1..R-4 all fire on the tag path only,
which means they are all invisible until the one day they matter.

## Counts

| | |
|---|---|
| `devbox run check` at `a9f5626` | **EXIT=0**, 0 lint issues, 22 packages `ok`, `no-real-numbers: clean`, `no-deployment-host: clean` |
| `devbox run release-dry-run` | **EXIT=0** — 6 archives + `checksums.txt` + `agent-gm-cli-0.0.0-dev.a9f5626.tgz`, install into a throwaway prefix, `agm 0.0.0-dev.a9f5626 (a9f5626c1cd…)`, `--nonsense` = 2 |
| `devbox run restore-drill` | **PASS** — 1 conversation, 1 message, `backup.keep=3`, account `connected`; wrong key → `signed_out` with history intact |
| `internal/release` tests | 14 top-level, 36 including subtests (implementer said "18 wrapper tests"; the honest number depends on how you count subtests) |
| Reviewer mutations planted | **20** — 15 killed by a named test, 5 survived (2 benign, 3 findings) |
| Reviewer tests added | 2 files, 5 live tests + 2 planted-and-skipped |

## Findings

### R-1 (high) — a mistyped or non-release tag cuts a real GitHub release

`scripts/release.sh:270-272` `require_tag` only asserts `tagged=1`, which
`scripts/release.sh:45` sets for **any** tag matching `tag:v*`. Every comment
in the file (`:19`, `:21`, `:271`) says "only a vX.Y.Z tag publishes". It
does not.

Evidence I ran:

```
$ GITHUB_REF_TYPE=tag GITHUB_REF_NAME=vnonsense ./scripts/release.sh notes
## agent-gm vnonsense
```

`.github/workflows/release.yml:22` fires the `release` job on `tags: ["v*"]`,
so `git tag vtest && git push --tags` runs the whole job: it builds, signs
with a real OIDC certificate, and `gh release create vtest` succeeds. `npm
publish` then fails on the invalid version — **after** a signed public release
called `vtest` exists, which cannot be unpublished from the sigstore log.

**Fix.** In `require_tag`, match the version:
`[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die …`.
Planted, skipped test: `internal/release/matrix_consistency_test.go`
`TestOnlyASemverTagPublishes`.

### R-2 (high) — `release.sh publish` will happily pin `@unknown`

`scripts/release.sh:290` defaults `AGENT_GM_IMAGE_DIGEST` to the literal
`unknown`, and `do_publish` (`:333-342`) never checks it. The refusal exists
only in `.github/workflows/release.yml:117-120`, whose own comment says "a
release note that says `@unknown` is worse than a release that waits" — a
judgement that lives in YAML and not in the script the note comes from.

`devbox.json` `"release"` is `build, sign, npm-pack, publish` and sets no
digest, so **the documented local release path always publishes `@unknown`**:

```
$ ./scripts/release.sh notes
    ghcr.io/thisnick/agent-gm@unknown
```

Spec §14.2 makes the digest the thing a deployment pins by. A release note
that names none is a deployment instruction nobody can follow.

**Fix.** `do_publish` dies unless `AGENT_GM_IMAGE_DIGEST` starts `sha256:`.
Planted, skipped test: `TestPublishRefusesToRecordAnUnknownImageDigest`.

### R-3 (medium) — nothing tied the six archives, the wrapper's platforms, the notes' cosign identity and the publish upload set together

Three mutations survived the entire suite **and** the dry run:

- **P14** — delete `"agm:darwin:amd64"` from `targets` (`scripts/release.sh:67`).
  The dry run cheerfully builds five archives and passes. Consequence: every
  Intel Mac running `npm i -g @agent-gm/cli` 404s in `postinstall`, because
  `npm/scripts/platform.js:50-53` still offers `darwin:x64`.
- **P13** — rewrite `release\.yml` to `publish\.yml` in the
  `--certificate-identity-regexp` the notes print (`scripts/release.sh:321`).
  Consequence: every reader who follows the release note's own
  `cosign verify-blob` gets a failure indistinguishable from a forged binary.
- **P16** — `gh release create … "$dist/checksums.txt"` with the `*.tar.gz`
  glob removed (`scripts/release.sh:340`). Consequence: a release page with a
  signature and nothing to verify.

**Fixed in this review**, not merely reported:
`internal/release/matrix_consistency_test.go` adds four tests that read the
shipped files as text and kill all three. Re-planted after writing them:

```
P14 → FAIL TestReleaseScriptBuildsExactlyTheSixArchivesOfSection143
       FAIL TestEveryPlatformTheWrapperSupportsIsAPlatformTheReleaseBuilds
P13 → FAIL TestTheNotesCosignIdentityNamesTheWorkflowThatSigns
P16 → FAIL TestPublishUploadsTheArchivesTheSignatureAndTheChecksums
```

Residual, not fixed: `docs/deploy.md` carries a **third** copy of the cosign
identity regexp. It is byte-identical today; nothing keeps it that way.

### R-4 (medium) — migration 0007's copy of a populated table was never exercised

`internal/store/migration0007_internal_test.go` opens a **fresh** store, so
`0001..0007` run against an empty `operations` and the twenty-column
`INSERT … SELECT … FROM operations_pre0007`
(`internal/store/migrations.go:833-841`) never moves a row. This is precisely
the statement the Slice 4 cutover runbook will run against the owner's live
data directory.

Proof it was untested — **P17**, transposing `conversation_id` and
`message_id` in the copy's SELECT:

```
existing test  → ok    github.com/thisnick/agent-gm/internal/store
reviewer test  → FAIL  operation row 0 changed across migration 0007
```

**Fixed in this review**:
`internal/store/migration0007_populated_internal_test.go` builds a database at
`user_version 6` with the shipped statements, seeds three `operations` rows
covering every nullable column, opens it with the current binary, and asserts
the rows survive byte for byte, the scaffolding table is gone,
`foreign_key_check` and `integrity_check` are clean, two keyless inserts
coexist **on a migrated database**, and a legacy key still collides. It kills
P17 and P18 (dropping `tmp_id` from the copy).

### R-5 (medium) — a `v*` tag on any commit publishes, and CI is not a gate

`.github/workflows/release.yml:76` gates the `release` job on
`startsWith(github.ref, 'refs/tags/v')` and nothing else. There is no check
that the tagged commit is an ancestor of `main`, and `release.yml` is a
separate workflow from `ci.yml` with no `needs:` and no required-status
coupling, so **a tag on a red or off-main commit builds, signs, creates the
release and publishes to npm with `NPM_TOKEN`.** §14.3 says "no release is cut
from a commit that has not passed a live gate" and §16 makes the tag the
coordinator's act — but the workflow enforces neither.

**Fix (choose one).** Put the `release` job behind a GitHub *environment* with
a required reviewer, or add a step that runs
`git merge-base --is-ancestor "$GITHUB_SHA" origin/main` and dies otherwise.
I did not write a test: this is a repository setting, not a file.

### R-6 (low) — the digest lookup does not tie the image to this commit

`.github/workflows/release.yml:110-116` polls
`docker buildx imagetools inspect ghcr.io/thisnick/agent-gm:$GITHUB_REF_NAME`
until a digest appears. The manifest-list digest is the right thing to pin,
and the 20-minute wait is a real wait. But a tag that was pushed, built, moved
and re-pushed resolves **immediately** to the previous image, and the release
note then pins a digest built from different source. Reading the digest and
then asserting the image's `org.opencontainers.image.revision` label equals
`GITHUB_SHA` would close it.

### R-7 (low) — `release.sh npm-publish` can be run against a stale tarball

`do_npm_publish` (`:344-357`) reads `dist/npm-tarball.txt` and checks nothing
else — not the version, not that `dist/checksums.txt` is the one packed into
it. In `release.yml` the ordering makes it safe (`build`, `npm-check`, `sign`,
`publish`, `npm-publish`, and the GitHub release therefore exists before npm
does). Run by hand via `devbox run release-npm-publish` after a stale `dist/`,
it publishes whatever is lying around. `do_npm_pack`'s
`ls "$dist"/agent-gm-cli-*.tgz | head -1` (`:208`) has the same shape: it
sorts lexicographically, so a leftover `0.0.0-dev.*` tarball wins over
`1.0.0`. `do_build` does `rm -rf "$dist"` first, which is what makes this low
rather than medium.

### R-8 (low) — a stale D7 comment in `internal/apierr/decode_test.go`

`:15` and `:35` still say `client_request_id` "has exactly two transports,
neither of them a query". After D38 it has **zero**. The assertion is right;
the sentence explaining it is a year out of date and will mislead the next
reader. `internal/api/invoke.go:67-70` also has a duplicated "any." from the
edit.

## What I verified, by running it

### The npm wrapper, driven for real on Linux

`npm pack` from `release.sh npm-pack`, installed into a throwaway prefix
(`npm install --prefix`), served from `file://dist`:

| Behaviour | Result |
|---|---|
| `agm version` through the installed shim | `agm 0.0.0-dev.a9f5626 (a9f5626c1cd18dee94cc6e1a511dc413ddca330e)`, rc 0 |
| `agm --nonsense` (§11.2 = 2) | **rc 2** |
| `not_found` against a stub server (§11.2 = 5) | **rc 5** — `agm: not_found: no conversation with that id` |
| `vendor/agm` removed → §11.2 = 9 | **rc 9**, with a message naming the path and the release page |
| `AGENT_GM_CLI_BINARY` → nonexistent path | **rc 9** |
| `AGENT_GM_CLI_BINARY` → a real binary | runs it, rc 0, no vendored copy |
| `AGENT_GM_CLI_SKIP_DOWNLOAD=1` install | rc 0, **no `vendor/`**, shim then exits 9 |
| forged `win32/x64` and `linux/riscv64` | `UnsupportedPlatformError` naming the platform and listing `darwin:arm64, darwin:x64, linux:arm64, linux:x64` |

**The tamper test, done the hard way.** I corrupted the served archive **and
regenerated the served `checksums.txt` to match it**, so the release page was
internally consistent and only the pinned copy disagreed:

```
npm error @agent-gm/cli: install failed
npm error checksum mismatch for agm_0.0.0-dev.a9f5626_linux_amd64.tar.gz
npm error   expected 6e3c47d94ace…  (from the checksums.txt pinned in this package)
npm error   actual   c2d357b211c8…  (from file:///tmp/…)
npm error The download does not match the release this package was published against. Nothing was installed.
```

Install failed, `vendor/` was never created. This is the §14.3 property that
matters and it holds.

### D38, on a live server (fake backend, ephemeral loopback port)

`bin/agent-gm serve`, `AGENT_GM_BACKEND=fake`, `AGENT_GM_ALLOW_FAKE=1`:

| Clause | Observed |
|---|---|
| `client_request_id` in a send body | `{"code":"invalid_request","field":"client_request_id"}` |
| two keyless sends | `op_01a07c9d-67c0-…` and `op_01a07c9d-67c7-…` — **distinct** |
| header replay, same key + body | same operation id twice |
| same key, different body | `idempotency_conflict` |
| empty-string `Idempotency-Key:` header | two distinct operations, both `NULL` in the column |
| `operations` on disk | `7` rows, `5` with `idempotency_key IS NULL` |

Cross-account: my live drive could not produce a second account
(`AGENT_GM_FAKE_ACCOUNT` is one address, so both pairings returned the same
`acct_`), so I fell back to the unit path —
`internal/core/slice2_operations_test.go` `TestSlice2Test35IdempotencyIsPerAccount`
now asserts `details.field == "Idempotency-Key"` and that the refused replay
never reached account B's backend. Covered; not something I proved end to end.

MCP: `client_request_id`, `idempotency_key` and `Idempotency-Key` appear
nowhere in `mcp.Tools` (names, descriptions, arg descriptions, **rendered**
input schemas), in `mcp.Instructions`, or in the golden served catalogue
`internal/mcp/testdata/served-catalogue.json`. `FreshKeySentence` is gone and
`LostResultSentence` is served in its place.

### The restore drill

Baseline PASS as quoted above. Broken deliberately (**P19**: restore step 3
started with `$wrong_key`):

```
restore-drill: the account came back signed_out even though sessions/ and the key were restored
rc=1
```

It fails loudly and the restored server does resume the fake account
(`state = connected`) when all three things move together.

## Plant table

| # | File:line | Mutation | Result |
|---|---|---|---|
| P1 | `internal/store/operations.go:263` | drop the `key == ""` guard in `OperationByKey` | **SURVIVED** — benign: `= NULL` is never true, which is the guard's own comment. Only dangerous together with P3 |
| P2 | `internal/store/migrations.go:850` | make the unique index unconditional | **SURVIVED** — benign: SQLite treats NULLs as distinct in a plain unique index too. The `WHERE` is documentation |
| P3 | `internal/store/operations.go:238` | `InsertOperation` writes `''` not NULL | killed — `TestMigration0007LetsKeylessOperationsCoexist` |
| P4 | `internal/mcp/tools.go:36` | reword the served `LostResultSentence` | killed — `TestD38TheLostResultSentenceSaysBothThings`, `TestSlice3DocsCoverTheServedCatalogue`, `TestTheServedCatalogueMatchesItsGolden` |
| P5 | `internal/cli/run.go:534` | mint a key when `--idempotency-key` is absent | killed — `TestD38TheCLISendsNoIdempotencyKeyUnlessAsked/an_ordinary_send_carries_no_key` |
| P6 | `npm/bin/agm.js:287` | collapse the exit status to 1/0 | killed — `TestExitCodesSurviveTheShim` (both subtests) |
| P7 | `npm/scripts/postinstall.js:217` | accept a checksum mismatch | killed — `TestATamperedDownloadFailsInstallNamingTheFile` |
| P8 | `npm/scripts/platform.js:73` | asset name uses dashes | killed — `TestPostinstallInstallsAnArchiveThatMatchesThePinnedChecksum`, `TestATamperedDownloadFailsInstallNamingTheFile` |
| P9 | `npm/scripts/platform.js:53` | quietly add `win32:x64` | killed — `TestUnsupportedPlatformIsRefusedByNameBeforeAnythingIsInstalled/win32/x64`, `TestPostinstallFailsOnAnUnsupportedPlatform`; and after this review also `TestEveryPlatformTheWrapperSupportsIsAPlatformTheReleaseBuilds` |
| P10 | `internal/api/strict.go:108` | drop the 200-byte / control-char validation | killed — `TestIdempotencyKeyTransport/an_unusable_key_is_refused_and_writes_nothing` |
| P11 | `internal/api/routes.go` | re-admit `client_request_id` on `messages_send` | killed — `TestD38ClientRequestIDInABodyIsRefusedByName/send`, `TestNoRouteTakesTheIdempotencyKeyAsAQueryParameter` |
| P12 | `scripts/release.sh:214` | remove the "checksums.txt is in the tarball" assertion | n/a — self-referential; kept for P12b |
| P12b | `npm/package.json:21` | drop `checksums.txt` from `files[]` | killed twice — `TestPackageManifestSaysWhatSection143Requires`, **and** `release.sh npm-pack` dies with "the pin of section 14.3 is not in the package" |
| P13 | `scripts/release.sh:321` | wrong workflow in the notes' cosign identity | **SURVIVED** → R-3; now killed by `TestTheNotesCosignIdentityNamesTheWorkflowThatSigns` |
| P14 | `scripts/release.sh:67` | six archives become five | **SURVIVED** → R-3; now killed by two new tests |
| P16 | `scripts/release.sh:340` | publish uploads only `checksums.txt` | **SURVIVED** → R-3; now killed by `TestPublishUploadsTheArchivesTheSignatureAndTheChecksums` |
| P17 | `internal/store/migrations.go:838` | transpose `conversation_id`/`message_id` in 0007's copy | **SURVIVED** the existing test → R-4; now killed by `TestMigration0007CarriesAPopulatedOperationsTableForward` |
| P18 | `internal/store/migrations.go:838` | drop `tmp_id` from 0007's copy | same as P17 |
| P19 | `scripts/restore-drill.sh:208` | restore with the wrong data key | killed — the drill dies `rc=1`, naming the clause |
| P20 | `scripts/restore-drill.sh:220` | remove the resume assertion | n/a — self-referential |

Every mutation reverted with `git checkout --`; `git status --porcelain`
after each batch showed only my two new test files.

## Acceptance axis (§18.1): every name means what it means in Google Messages

**Score: 2.**

D38 is the strongest evidence for it in this slice. `client_request_id` was
never a Google Messages word — it was an HTTP-plumbing word an agent had to
invent, and removing it leaves `Idempotency-Key`, which is the HTTP word for
the HTTP thing, in the HTTP place. The names this slice adds are release
words, not messaging words, and they are the ordinary ones: `checksums.txt`,
`vendor/agm`, `agm version`, `--idempotency-key`, `restore-drill`,
`AGENT_GM_CLI_SKIP_DOWNLOAD`, `AGENT_GM_CLI_BINARY`. Nothing invents a term
for a thing that already has one, and no name says less than it means. The
`LostResultSentence` constant is named for what it tells a model, not for what
it replaced.

## Spec clauses verified / not verified

**Verified at `a9f5626`** (each by output I produced):

- §14.3 six archive names, `checksums.txt`, the AGPL LICENSE in every archive
  — `release.sh build`/`verify`, 6/6.
- §14.3 npm wrapper: `bin`, `license`, version equal to the Go version,
  pinned `checksums.txt`, `postinstall` platform map, `vendor/agm` + `0755`.
- §14.3 / §16 Slice 4 test 2 — tampered download **with a matching served
  checksums** is refused by the pin.
- §16 Slice 4 test 3 — unsupported platform named, nothing installed.
- §16 Slice 4 test 4 — `--nonsense` = 2 and `not_found` = 5 through a real
  `npm install`, natively and through the shim, plus a missing binary = 9.
- §15.2 — the drill: standalone snapshot, no `-wal`, `integrity_check` ok,
  restore into a fresh directory, and the documented wrong-key failure mode.
- §6.3 / D38 — header-only transport, optional key, replay, conflict, the
  200-byte and control-character refusals, `client_request_id` as an unknown
  body field on all eight mutations, two keyless calls = two operations.
- §8.2 — no MCP tool, description, arg description, rendered schema,
  instructions block or golden catalogue mentions any key spelling.
- §11.2 — the ten codes survive the wrapper.
- §4.3 / migration 0007 — nullable column, partial index, **and now** a
  populated forward migration.
- §13.6 — `devbox run check` green; every CI step is a `devbox run`.

**Not verified** (and why):

- §16 Slice 4 test 1's **qemu execution** of `linux/arm64`. This host has no
  `binfmt_misc/qemu-aarch64`, so `release.sh verify` took the `file` branch
  for both arm64 archives. The workflow's `docker/setup-qemu-action` is the
  right arrangement; I could not run it.
- §16 Slice 4 test 5 — a real GitHub release, a real cosign signature and a
  real GHCR digest. This is the tag path, and it is exactly what R-1..R-4 are
  about: **it has never run, and after this review it is checked by text
  assertions, not by execution.** Treat the first `v1.0.0` as a live gate in
  its own right.
- §16 Slice 4 test 6 — the owner's Mac live gate. Coordinator's.
- Cross-account idempotency **end to end**. Covered by
  `TestSlice2Test35IdempotencyIsPerAccount`; my live drive could not make a
  second fake account.
- npm publish, `--provenance`, and the provenance fallback at
  `scripts/release.sh:352-355`. Never executed by anything.
- The `image` job's GHCR push and the digest handshake between `ci.yml` and
  `release.yml`. Never executed by anything.

## Must land before `v1.0.0` is tagged

1. R-1 — `require_tag` matches a semver version.
2. R-2 — `do_publish` refuses a digest that is not `sha256:…`.
3. R-5 — the `release` job is gated on the commit being on `main` (or on a
   protected environment).
4. Keep the two test files this review added; un-skip the two planted tests
   as R-1 and R-2 land.

---

# Addendum — re-review at `ee7cad1`

Synced with `git fetch origin slice-4/release` and `git checkout --detach ee7cad1`.
`devbox run check` at `ee7cad1`: **EXIT=0**, 0 lint issues, every package `ok`,
`no-real-numbers`/`no-deployment-host` clean. `internal/release` is now **22
tests, 22 passing**, up from 14.

My two files were carried in unchanged — the only diff against `review/slice-4`
is the removal of the two `t.Skip` lines. No assertion was weakened, which is
the one thing a plant's author has to check.

## R-1, R-2, R-6, R-7, R-8: confirmed fixed, driven by me

I ran `scripts/release.sh` with forged `GITHUB_REF_TYPE`/`GITHUB_REF_NAME`
rather than reading the diff.

**R-1.** Nine non-versions × three publishing modes = 27 invocations, all
refused *before* cosign, `gh` or npm is touched:

```
vnonsense / vtest / v1.0 / v1.0.0.1 / v-1.0.0 / latest / "v1.0.0 " (trailing space)
  × sign, publish, npm-publish   →  release: refusing to <mode> from the tag …
```

And the check that matters more, because a guard that refuses everything is
not a guard: `v1.0.0`, `v0.0.1`, `v10.20.30`, `v1.0.0-rc.1`, `v1.0.0-alpha.1`
all get **past** `require_tag` and stop at the digest instead.

**R-2 / R-6.** `do_publish` now refuses, in this order: unset → *"a release
note pins a deployment by digest (section 14.2)"*; `unknown` → *"not a
sha256:<64 hex> digest"*; `sha256:abc` and a bare 64-hex string → same; a
well-shaped digest → *"does not resolve on the registry"*. The revision-label
comparison is the right answer to R-6, and `scripts/image.sh:77` does pass
`--build-arg COMMIT=$commit`, so the label will be populated.

**R-7.** A stale `dist/npm-tarball.txt` is refused by name: *"the packed
tarball is agent-gm-cli-0.0.0-dev.deadbee.tgz, but this release is 1.0.0"*.

**R-8.** Both sentences fixed.

**Their plant is a good one.** Deleting the `LABEL` line, or hardcoding the
revision instead of `${COMMIT}`, is killed by
`TestTheImageCarriesTheRevisionLabelTheDigestGuardReads`. Reverting
`require_tag` is killed by both `TestOnlyASemverTagPublishes` (mine) and
`TestReleaseScriptRefusesATagThatIsNotAVersion` (theirs).

## R-9 (blocking) — the `ci`-is-green gate can never pass, so no release can ever be cut

`.github/workflows/release.yml:150-153`:

```
conclusion="$(gh api "…/actions/runs?head_sha=${GITHUB_SHA}&per_page=100" \
  --jq '[.workflow_runs[] | select(.name == "ci" and .event == "push")]
        | sort_by(.run_started_at) | last | .conclusion // "none"')"
[ "$conclusion" = success ] || exit 1
```

`ci.yml:12` fires on `tags: ["v*"]`. So pushing `v1.0.0` starts a **second**
`ci` push run for that same `head_sha`, and it is by definition still running
when the `release` job's first step asks. `sort_by(.run_started_at) | last`
picks that one. Its conclusion is `null`, `// "none"` turns it into `none`,
and the job exits 1.

I ran the jq against the exact shape GitHub will return:

```
--- implementer's rule (sort_by | last):        none      ← the release dies
--- "any ci push run for this sha succeeded":   1         ← the fact it meant to check
```

The gate is right in intent and inverted in effect: **it fails closed on every
release, including a perfectly green one.** R-5 as I filed it is satisfied by
the `merge-base` check; this is a new defect introduced by the fix for it.

**Fix.** Ask whether *any* `ci` push run for this commit concluded `success`,
rather than what the newest one concluded:

```
green="$(gh api "…/actions/runs?head_sha=${GITHUB_SHA}&per_page=100" \
  --jq '[.workflow_runs[] | select(.name=="ci" and .event=="push"
         and .conclusion=="success")] | length')"
[ "$green" -ge 1 ] || { echo "::error::no green ci run for ${GITHUB_SHA}" >&2; exit 1; }
```

A commit only reaches a tag by being on `main`, and the ancestry check on the
line above already proves that, so "some push run of `ci` for this exact sha
went green" is the honest question.

## R-10 (medium) — the workflow is still the one file nothing tests

Two plants survived the whole suite at `ee7cad1`:

| # | File | Mutation | Result |
|---|---|---|---|
| Q5 | `scripts/image.sh:77` | `--build-arg "COMMIT="` (stop passing the commit) | **SURVIVED** — the Dockerfile test proves the `LABEL` reads `${COMMIT}`; nothing proves anybody *sets* it. The label ships empty and `require_digest` dies with "carries no org.opencontainers.image.revision label" on release day |
| Q6 | `.github/workflows/release.yml:145` | delete the `merge-base --is-ancestor` check | **SURVIVED** — no test reads the workflow at all |

Q6 is not a hypothetical: **R-9 is a live bug sitting in exactly the region
Q6 shows is unguarded.** `tag_guards_test.go` runs `release.sh`, which is a
real advance; the YAML that calls it is still asserted by nobody.

**Fix.** A small text test over `.github/workflows/release.yml` in the shape
of my `matrix_consistency_test.go`: the `release` job contains a
`merge-base --is-ancestor` against `origin/main`, its ci-conclusion jq selects
on `.conclusion=="success"` rather than on ordering, and `image.sh` passes
both `--build-arg VERSION=` and `--build-arg COMMIT=` with non-empty values.
I have not written it; R-9 has to be decided first and the test should encode
the decision.

## R-11 (low, new) — the digest shape check is not hex

`scripts/release.sh` gates on `sha256:` followed by 64 `?` glob wildcards, so

```
$ … AGENT_GM_IMAGE_DIGEST=sha256:zzzz…zzzz ./scripts/release.sh publish
release: ghcr.io/thisnick/agent-gm@sha256:zzz… does not resolve on the registry
```

It is caught, but by the registry rather than by the shape, so the diagnosis a
reader gets for a typo'd digest is "the image is not there". `[0-9a-f]` in a
`[[ =~ ]]` would name the real problem. Also `v01.0.0` passes `require_tag`
and is not valid semver (leading zero); npm rejects it, so nobody is harmed.

## Residual, unchanged

`docs/deploy.md` still carries a third copy of the cosign identity regexp with
nothing keeping it byte-identical to `scripts/release.sh`. My answer to the
implementer's question: **yes, tie it down** — it is four lines in
`TestTheNotesCosignIdentityNamesTheWorkflowThatSigns`, and it is the only
copy an operator actually reads.

## Revised verdict

**PR #1 may merge.** R-1, R-2, R-6, R-7 and R-8 are genuinely fixed and I
verified each by running it.

**`v1.0.0` still may not be tagged**, for a new reason: R-9 means the first
tag would fail its own gate. That is fail-*closed*, so nothing unsafe would
ship — but it must be fixed before a release is attempted, and fixing it under
time pressure on release day is precisely the situation this slice exists to
avoid.

Must land before a tag:

1. **R-9** — the ci-green jq, which currently cannot pass.
2. **R-10** — a text test over `release.yml` and `image.sh`, so R-9 cannot
   recur silently.
3. R-11 and the `docs/deploy.md` cosign copy: nice to have, not blocking.
