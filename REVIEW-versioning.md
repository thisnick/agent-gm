# Review — the changesets versioning slice (D39, §14.3)

Reviewed: `66afa7d..0a8234c` on `versioning/changesets` (PR #2), in
`.claude/worktrees/review-versioning` at `0a8234c`.

**Verdict: accept with required fixes.** The design is right and the guards are
real — seven of eight planted mutations died to named tests. But the machinery
has two holes on exactly the path CI cannot prove until the first merge, and one
of them fires on the merge of PR #2 itself.

- **PR #2 must not merge as it stands** — V-1.
- **The first automated `v1.0.2` must not be allowed to cut** until V-1 and V-2
  land. V-3 and V-4 should land with them; V-5..V-7 are follow-ups.

## 1. Evidence I produced

```
$ devbox run check
agent-gm devbox: go go1.27.0
0 issues.
ok  github.com/thisnick/agent-gm/internal/release  15.970s
… 22 packages ok, 2 no test files …
no-real-numbers: clean
no-deployment-host: clean
EXIT=0
```

```
$ devbox run release-dry-run
npm notice version: 1.0.1
npm notice filename: agent-gm-cli-1.0.1.tgz
release: agm version through the shim: agm 1.0.1 (0a8234c059aec4374…)
release: agm --nonsense exits 2 through the shim
release: dry run complete -- built everything, published nothing
EXIT=0
```

```
$ devbox run -- go test ./internal/release/ -list '.*' | grep -c '^Test'
48
```

48 tests in `internal/release`, as claimed. `TestEveryArtefactCarriesTheOneVersion`
does what the report says: it builds both binaries, starts the server on an
ephemeral port against the fake backend, mints an admin session from a throwaway
secret, reads `GET /v1/health`, packs the tarball, and compares six versions
(`internal/release/one_version_test.go:71`).

**PR #2, live.** `gh pr checks 2` on run `34164378520` (the pull_request-event
run): `changeset  pass  4s`. The `changeset` job on run `34164079802` shows
`skipping` — that is the *push* run, and the job is `if: github.event_name ==
'pull_request'`. So the claim "CI green on 34164079802" is true but that run did
**not** exercise the changeset job; the PR-event run did, and it passes. PR #2
carries `.changeset/olive-planes-invent.md`, so the pass is the
`this pull request carries a changeset` branch.

## 2. Findings

### V-1 — merging PR #2 makes `version.yml`'s `tag` job fail on main (blocker)

§14.3 / D39. `npm/package.json` goes `0.0.0-dev` → `1.0.1` in this PR, and
`v1.0.1` is already a tag on origin. `cut-tag.sh` sees a *moved* version whose
tag exists and `die`s:

```
$ ./scripts/cut-tag.sh "0.0.0-dev"
version=1.0.1
cut-tag: v1.0.1 already exists in this repository. The version went 0.0.0-dev -> 1.0.1 …
exit=1
```

It fails safe — no tag is pushed, nothing is published — but the very first push
through the new machinery turns main red, and the operator's only recovery is to
know it is expected.

**Fix.** In `scripts/cut-tag.sh:53`, when the tag already exists *and points at a
commit reachable from HEAD*, print `release=0` with a note instead of dying: that
version is already released, so there is nothing to cut. Keep the `die` for a tag
that exists but is **not** reachable — that is the genuinely dangerous case (same
version, different commit). This also covers the revert-and-remerge case the
script's own header describes, which today fails the job rather than quietly
doing nothing. Add a test beside `TestCutTagRefusesAVersionThatIsAlreadyTagged`.

### V-2 — a hand-edited manifest cuts a full signed release with no changelog entry (blocker for the first cut)

D39 says "the field is moved by changesets and by nothing else", and the
`version.yml` header asserts "which only the merge of that pull request does".
That is a rule, not a guard. Nothing in the release path reads `CHANGELOG.md`:

```
$ grep -rn CHANGELOG scripts/*.sh .github/workflows/*.yml
.github/workflows/version.yml:13:  #  … CHANGELOG.md. It publishes nothing.
.github/workflows/version.yml:45:  # … in a shape CHANGELOG.md has never used;
```

Demonstrated: with `npm/package.json` hand-set to `1.0.2` and no changeset, no
Version Packages PR and no changelog entry —

```
$ ./scripts/cut-tag.sh "1.0.1"
version=1.0.2
release=1
cut-tag: npm/package.json went 1.0.1 -> 1.0.2; v1.0.2 is free
$ grep -c '## \[1.0.2\]' CHANGELOG.md
0
```

`release=1` cuts the tag, dispatches `ci.yml` and `release.yml`, and every
downstream guard passes: the tag equals the manifest, the commit is on main, `ci`
is green, the digest resolves, the revision label matches. A signed public
release, an npm version and a sigstore entry, for a version the changelog has
never heard of. The same hole swallows the documented emergency path — the docs
warn in prose that "a release cut around it has an empty entry" and then do not
check.

Note `changeset-check.sh` does *not* close this: `npm/package.json` is a
`code_path`, so the PR needs a changeset — but a `no-release` label, or a
changeset for something else, lets it through.

**Fix.** One line, in the place that already decides: `cut-tag.sh` (and
`release.sh require_tag`, for the manual path) asserts
`grep -q "^## \[$version\]" CHANGELOG.md`, failing with "the changelog has no
entry for $version; merge the Version Packages pull request". This is the single
guard that makes "changesets moves the version" enforced rather than asserted.

### V-3 — `changeset-check` misclassifies a rename out of code and into docs

`ci.yml:220` runs `git diff --name-only "$base" "$head"`. Git collapses a rename
to its destination:

```
$ git mv internal/a.go docs/a.md && git commit …
$ git diff --name-only HEAD~1 HEAD
docs/a.md
$ git diff --no-renames --name-only HEAD~1 HEAD
docs/a.md
internal/a.go
```

and the script then says:

```
rename dest is doc -> exit=0 :: changeset-check: documentation only; no changeset needed
```

So deleting a Go file by renaming it under `docs/` ships a code change with no
changeset and no changelog entry. **Fix: `git diff --no-renames --name-only`**,
and assert `--no-renames` in `TestTheChangesetCheckIsAJobOnEveryPullRequest`
(which today asserts only that `base.sha` appears).

Two other classification results, both correct, both verified:

```
docs+code           -> exit=1   (code wins; good)
delete-only         -> exit=1   (deleted paths are still paths; good)
nested *.md         -> exit=1   (npm/README.md ships; good)
no-release label    -> exit=0
other labels only   -> exit=1
label whitespace    -> exit=0   (" no-release , bug" is honoured)
```

One miss: `LICENSE` is classified documentation (`changeset-check.sh:61`), but
`release.sh:238` copies `LICENSE` into the npm tarball — it ships. Move it out of
the docs arm.

### V-4 — `ci.yml` diffs two-dot where the script documents three-dot

`changeset-check.sh:5` documents `git diff --name-only origin/main...HEAD`
(merge-base). `ci.yml:220` uses the two-dot `git diff "$base" "$head"`, so
anything merged into main since the PR branched appears, inverted, in the changed
set. It over-requires rather than under-requires, so it is not a safety hole —
but it will demand a changeset for a PR that touched only docs, on a busy main,
and the script's own contract and its tests describe the other query. Make them
agree.

### V-5 — `changelog-format.mjs` silently destroys hand-written `[Unreleased]` content

`scripts/changelog-format.mjs:99-102` rewrites everything from the `[Unreleased]`
heading to the next `## [` with the fixed literal `## [Unreleased]\n\nNothing
yet.\n\n`. Anything a maintainer wrote there is gone:

```
$ # CHANGELOG.md with "- A HAND WRITTEN NOTE THAT MUST SURVIVE." under [Unreleased]
$ node scripts/changelog-format.mjs npmCHANGELOG.md CHANGELOG.md
changelog-format: 1.0.2 filed under [Unreleased] as of 2026-09-08
$ grep -c "HAND WRITTEN" CHANGELOG.md
0
```

The comment says `[Unreleased]` "is emptied by the release that just consumed
it", which is true of changeset-derived bullets and false of anything else.
**Fix:** carry any surviving `[Unreleased]` body into the new entry above the
changeset bullets, or refuse when it is not the literal `Nothing yet.` — either
is a decision; silent deletion is not.

The *good* news, verified byte-for-byte against the real file: **the existing
1.0.0 and 1.0.1 entries are preserved exactly.** Diffing the tail from
`## [1.0.1]` onward before and after shows one hunk, and it is the intended link
move:

```
< [Unreleased]: …/compare/v1.0.1...HEAD
---
> [Unreleased]: …/compare/v1.0.2...HEAD
> [1.0.2]: …/releases/tag/v1.0.2
```

### V-6 — `changesets/action@v1` is a mutable third-party tag with `contents: write`

Nothing in this repository is SHA-pinned (`actions/checkout@v4`,
`docker/*@v3`, `jetify-com/devbox-install-action@v0.13.0`, …), so `@v1` follows
house convention. But this is the first third-party action granted
`contents: write` **and** `pull-requests: write` on the default branch, and `v1`
is a tag its owner can move. Pin `changesets/action` by SHA with a `# v1.x.y`
comment, or say in `version.yml` why the repository accepts a floating tag here.
(`npm ci --ignore-scripts` in the same job is right and I checked it.)

### V-7 — `no-release` is not documented as "the code still ships"

The label is described in `CONTRIBUTING.md` and `docs/operations.md` as the
exception for a PR that "genuinely ships nothing". It does not say what actually
happens: the code merges, no version moves, and the code **ships in the next
release under a version whose changelog does not mention it**. The script's own
output is the only place that says so ("N changed path(s) will not appear in a
changelog"). One sentence in `CONTRIBUTING.md`.

## 3. The path CI cannot prove — read line by line

| Question | Answer | Evidence |
|---|---|---|
| Can `tag` fire on a bump that was not the Version Packages PR? | **Yes** | V-2, run above |
| Does the dispatch pass the tag ref correctly? | Yes | `gh workflow run <wf> --ref v1.0.2` → `POST …/dispatches {"ref":"v1.0.2"}`; the run's `github.ref` is `refs/tags/v1.0.2`, `GITHUB_REF_TYPE=tag`, `GITHUB_REF_NAME=v1.0.2`. Both `ci.yml:21` and `release.yml:46` carry `workflow_dispatch`, and both files will be on main before the first dispatch, which is GitHub's precondition |
| Does `release.yml`'s `github.ref` really become `refs/tags/vX.Y.Z`? | Yes — and `release.yml:60/95` (`startsWith(github.ref, 'refs/tags/v')`) therefore selects `release` and skips `dry-run` on a dispatch, as intended | read |
| If the `ci` dispatch is slower than the `release` dispatch? | Safe. `release.yml:215` loops 60×20s (20 min) for a digest **whose `org.opencontainers.image.revision` label equals `GITHUB_SHA`**, so a stale image from an earlier attempt is rejected, not picked up. Confirmed at `release.yml:218-220` | read |
| Can `tag` race a second push to main? | No. `concurrency: version-main, cancel-in-progress: false` serialises; `actions/checkout` pins `github.sha`, so the tag is created on the merge commit that bumped the version and not on main's tip; the follow-on run computes `previous == version` and answers `release=0`. `merge-base --is-ancestor` in `release.yml:168` still holds after main advances | read |
| Is the changesets action pinned by SHA? | **No** — V-6 | `grep uses:` |
| Does the formatter preserve 1.0.0/1.0.1 byte-for-byte? | **Yes** | V-5, diff above |
| Does the check misclassify docs+code / delete-only / rename? | docs+code no, delete-only no, **rename yes** — V-3 | V-3 |
| Does `no-release` block a bump but still ship the code, documented? | Behaviour yes, documentation **no** — V-7 | V-7 |
| Is the `no-release` bypass reachable without the label? | No | plant M5 |

Two smaller reads worth recording. `release.sh` no longer produces
`0.0.0-dev.<short>` off a tag: every dry run on main is now stamped with the
released version, so `dist/agent-gm_1.0.1_linux_amd64.tar.gz` from a dry run and
from the real release are the same filename. The old comment's "makes a dry run's
artefacts unmistakable" property is gone. That is the necessary consequence of
D39 and the revision label disambiguates, but it is worth a line in
`docs/operations.md`. And `AGENT_GM_RELEASE_TARGETS` (`release.sh:104`) is
correctly refused on a tag, so it cannot publish five archives out of six.

## 4. Planted mutations

Full `devbox run -- go test ./internal/release/` after each; reverted with
`git checkout --` after each; `git status --short` empty at the end.

| # | Mutation | Result |
|---|---|---|
| M1 | `cut-tag.sh:45` — `if [ "$previous" = "$version" ]` → `if false` (tag job fires with no version change) | killed — `TestAnOrdinaryPushToMainCutsNoTag` |
| M2 | `cut-tag.sh:53` — tag-exists refusal disabled | killed — `TestCutTagRefusesAVersionThatIsAlreadyTagged` |
| M3 | `release.sh:340` — `ref_name != tag` assertion deleted | killed — `TestReleaseScriptRefusesATagThatDisagreesWithTheManifest` |
| M3b | `release.yml:159-165` — the same assertion deleted from the workflow | killed — `TestTheManualTagPathStillCarriesEveryGuard` |
| M4 | `image.sh:69` — prerelease arm deleted, so `v1.0.2-rc.0` takes `latest` and `v1.0` | killed — `TestAPrereleaseImageIsNotTaggedLatestOrByMinor` |
| M5 | `changeset-check.sh:80` — `[ "$l" = "$label_bypass" ]` → `[ -n "$l" ]` (any label bypasses) | killed — `TestTheNoReleaseLabelIsTheWayPast` |
| M5b | `release.sh:539` — prerelease `dist_tag=next` deleted, so an rc takes npm `latest` | killed — `TestAPrereleaseIsPublishedUnderTheNextDistTag` |
| M6 | `changelog-format.mjs:102` — drop the newest previous entry from the tail | killed — `TestTheVersionEntryLandsInTheHouseFormat`, `TestAPrereleaseEntryIsFiledTheSameWay` |
| M7 | `release.sh:247` — staged-version assertion neutered to `[ "$staged" = "$staged" ]` | **SURVIVED**, 48 pass |
| M7b | M7 **plus** the staged `package.json` actually rewritten to `9.9.9` | killed — `TestEveryArtefactCarriesTheOneVersion` |

M7 is a note, not a finding, and M7b is why: the *guard* has no test of its own,
but the *consequence* it guards against is caught by the artefact test. It would
matter only if someone reintroduced a pack-time rewrite *and* the artefact test
were narrowed. Worth a one-line negative test; not a blocker.

## 5. Rubric — §18.1, "every name means what it means in Google Messages"

**Score: 2.**

This slice introduces no name in the Google Messages domain. Everything new —
`changeset`, `no-release`, `Version Packages`, `version-packages`, `cut-tag`,
`changeset-check`, `changelog-format`, `release.sh version`, the `next` dist-tag,
`rc` — is release-engineering vocabulary, and each is the term the tool it names
uses (`changesets`, npm dist-tags, semver prerelease identifiers). No
conversation, participant, message, delivery state or operation name moved.
`agm version`, `agent-gm version` and the `version` field of `GET /v1/health`
keep their meanings and now agree, which is a small improvement to the axis
rather than a cost to it. `devbox run lint-names` passes inside `devbox run check`.

## 6. Verified / not verified

**Verified at `0a8234c`:** the one version across six artefacts (built and run);
`release.sh version` as the single reader; no pack-time rewrite; the
tag==manifest assertion in both `release.sh` and `release.yml`; the six-archive
matrix and the refusal to narrow it on a tag; prerelease image tagging and the
npm `next` dist-tag; the `changeset` job's presence, its label plumbing and its
live pass on PR #2; the changeset check's docs-only, code, delete-only,
both-kinds and label paths; changelog formatting including byte-for-byte
preservation of 1.0.0 and 1.0.1; `cut-tag`'s three questions; every existing
Slice-4 release guard still present and still killed by its own test (M3b);
`devbox run check` and `devbox run release-dry-run` green.

**Not verified — no first merge has happened:** that `changesets/action` opens
the Version Packages PR at all (needs the repository setting, now on, and one
push to main); that `gh workflow run --ref <tag>` succeeds with this job's
`actions: write` token; that the dispatched `ci` image job pushes to GHCR under a
tag ref; that `release.yml` finds the digest within 20 minutes. All four are
first-merge facts, which is precisely why V-1 must be fixed before that merge:
the first exercise of this machinery should not be a failure that is expected.

**n/a:** live gates (§13.3), any real send, any deploy — coordinator only, and
untouched by this slice.

---

# Re-review at `2382cd4` (`0a8234c..2382cd4`)

Merged into `review/versioning`; the tree under test differs from
`origin/versioning/changesets` only by this file.

**Verdict: accept.** All seven findings are fixed and I reproduced each fix.
One new plant survived (R-1, small, a test-tightening) and one new finding is a
consequence of the out-of-slice work bundled into this commit (R-2). Neither
blocks the merge of PR #2 or the first automated `v1.0.2`; both should land
before the Version Packages PR is merged, because R-2 is about what that
release's changelog will say.

## Evidence

```
$ devbox run check
0 issues.  … no-real-numbers: clean … no-deployment-host: clean
EXIT=0
$ devbox run -- go test ./internal/release/ -list '.*' | grep -c '^Test'
52
```

**52, not the 55 reported.** 48 before, four genuinely new
(`TestAVersionAlreadyTaggedInThisHistoryCutsNothingAndPasses`,
`TestCutTagRefusesAVersionTheChangelogDoesNotMention`,
`TestReleaseScriptRefusesATagTheChangelogDoesNotMention`,
`TestHandWrittenUnreleasedNotesSurviveIntoTheEntry`) plus a rewritten
`TestCutTagRefusesAVersionThatIsAlreadyTagged`, and a fifth new test
(`TestAdminLoginSuggestsNarrowingWhenItGrantsAllFourScopes`) in
`internal/cli`. Miscount, not a defect.

## Each finding, re-run

**V-1 fixed.** The PR #2 merge now leaves main green, and the re-cut refusal
survives:

```
$ ./scripts/cut-tag.sh 0.0.0-dev
version=1.0.1
release=0
cut-tag: v1.0.1 already exists in this repository at b33794c…, and that commit
is in this history: 1.0.1 is already released and there is nothing to cut.
rc=0

$ # v9.9.9 pointed at an orphan commit built with git commit-tree
$ ./scripts/cut-tag.sh 1.0.1        # manifest hand-set to 9.9.9
cut-tag: v9.9.9 already exists in this repository at ef7c143… and is NOT in this
history. … Bump to a new version instead of re-cutting this one.
rc=1
```

I also checked the branch the local check short-circuits past: on the remote
path `already_cut` is handed the **annotated tag object** sha
(`git ls-remote … | head -1` returns `refs/tags/v1.0.1` before
`refs/tags/v1.0.1^{}`), and `git merge-base --is-ancestor` peels it —
`is-ancestor accepts the TAG OBJECT: OK`. So the remote branch behaves like the
local one rather than always dying. Good.

**V-2 fixed**, both paths:

```
$ ./scripts/cut-tag.sh 1.0.1                    # manifest 1.0.2, no entry
cut-tag: CHANGELOG.md has no entry for 1.0.2. … rc=1
$ ./scripts/cut-tag.sh 1.0.1                    # same, entry added
release=1 … rc=0
```

The dots are escaped (`${version//./\\.}`) in both scripts, and in `release.sh`
the check sits after the tag/manifest match and before `[ "$tagged" = 1 ]`.

**V-3 fixed.** `LICENSE` is out of the docs arm (`LICENSE rc=1`, listed under
"the paths that need one"), and with `--no-renames` the rename now presents both
paths, so `{docs/a.md, internal/a.go}` → `rc=1`. `docs/a.md` alone still passes.

**V-4 fixed.** `git diff --no-renames --name-only "${base}...${head}"`.

**V-5 fixed**, and it does the better of the two things I offered — the note is
carried into the entry rather than the run refused:

```
## [Unreleased]

Nothing yet.

## [1.0.2] — 2026-09-08

- A HAND WRITTEN NOTE THAT MUST SURVIVE.

- one sentence
```

`Nothing yet.` appears exactly once. The plain case is unchanged: diffing the
tail from `## [1.0.1]` before and after still yields one hunk, the intended link
move, so 1.0.0 and 1.0.1 remain byte-for-byte.

**V-6 fixed, and the sha is the right one.** I resolved it independently rather
than trusting the comment: `changesets/action` tag `v1.9.0` is an annotated tag
object `3841a0683d3cfa6dae0f9bb335290003010fe3f0`, which dereferences to commit
`a45c4d594aa4e2c509dc14a9f2b3b67ba3780d0d` — exactly the pin. The comment
explaining why this one action is pinned when the rest are not is the right
call.

**V-7 fixed.** CONTRIBUTING.md now says `no-release` "merges and **ships in the
next release, unmentioned in the changelog**", and operations.md picked up both
smaller reads (dry-run artefacts sharing a filename; both new guards).

## Re-plants at `2382cd4`

| # | Mutation | Result |
|---|---|---|
| P1 | `cut-tag.sh:96` — changelog guard disabled | killed — `TestCutTagRefusesAVersionTheChangelogDoesNotMention` |
| P2 | `release.sh:355` — changelog guard disabled | killed — `TestReleaseScriptRefusesATagTheChangelogDoesNotMention` |
| P3 | `cut-tag.sh:65` — `already_cut` always dies (the V-1 regression) | killed — `TestAVersionAlreadyTaggedInThisHistoryCutsNothingAndPasses` |
| P4 | `cut-tag.sh:65` — `already_cut` never dies (re-cut protection lost) | killed — `TestCutTagRefusesAVersionThatIsAlreadyTagged` |
| P5 | `ci.yml:228` — `--no-renames` deleted from the command | **SURVIVED**, 52 pass — R-1 |
| P6 | `ci.yml:228` — three-dot reverted to two-dot | killed — `TestTheChangesetCheckIsAJobOnEveryPullRequest` |
| P7 | `version.yml:75` — pin reverted to `changesets/action@v1` | killed — `TestTheVersionWorkflowCutsTheTagAndDispatchesTheReleasePath` |
| P8 | `changeset-check.sh:68` — `LICENSE` back in the docs arm | killed — `TestAPullRequestThatShipsSomethingIsRefusedWithoutAChangeset` |

P3 and P4 together are the pair I wanted: both directions of the new branch are
pinned, so the V-1 fix cannot be undone in either direction without a named test
failing.

### R-1 — the `--no-renames` assertion is satisfied by its own comment

`internal/release/changeset_check_test.go` asserts
`strings.Contains(ci, "--no-renames")`. The flag appears twice in `ci.yml`:

```
225:          # request on a busy day. --no-renames because git collapses a rename
228:          git diff --no-renames --name-only "${base}...${head}" \
```

Deleting it from line 228 leaves line 225 to satisfy the test, and the suite
stays green (P5, 52 pass). The production consequence is the V-3 hole
reopening silently: a rename of code under `docs/` classifies as
documentation-only and ships with no changeset and no changelog entry.

The V-4 assertion does not have this problem — it matches the quoted
`"${base}...${head}"`, which occurs only in the command — which is why P6 died
and P5 did not.

**Fix:** assert the invocation rather than the flag —
`strings.Contains(ci, "git diff --no-renames --name-only \"${base}...${head}\"")`
— which covers V-3 and V-4 in one assertion that no comment can satisfy.

### R-2 — the out-of-slice admin-session change ships unmentioned in 1.0.2

`2382cd4` also carries a user-visible CLI change (`agm auth login --admin` now
prints `Narrow with --scopes …` on stderr), `docs/cli.md`, and a new "Admin
sessions" section in `docs/operations.md`. The branch's only changeset says:

```
Versions are managed with changesets; the container, binaries and npm package
share one version
```

So the first release cut on these rails will have a changelog entry that
describes half of what shipped. This is not a process nit — it is the failure
mode D39 exists to prevent, arriving on the commit that builds D39. `changeset-check`
passed because *a* changeset is present; the check counts changesets, it cannot
read them.

**Fix:** add a second changeset for the admin-session change before the Version
Packages PR is opened. (No mechanism can enforce "the changeset describes the
change", and I am not asking for one — the guard against this is review, and
this is that review.)

Two smaller notes on the same change, neither blocking:

- `internal/cli/commands_impl.go:510` gates the hint on `len(session.Scopes) == 4`.
  Four is right today — `internal/authz/scopes.go:52` `canonicalOrder` has
  exactly `admin messages:read messages:write messages:delete` — but a magic
  number is not the question being asked. A fifth scope would silence the hint;
  a narrowed four-of-five session would get it wrongly. Compare against the
  canonical set (exporting it if need be), not against its length.
- The scope-narrowing behaviour itself is documentation of behaviour that already
  existed; I did not re-verify the 15-minute / 30-day / 90-day lifetimes beyond
  the implementer's citation of `internal/authz/settings.go`, and I record that
  as **unverified** rather than accepted.

## Standing verdict

- **PR #2 may merge.** V-1 is fixed and I reproduced the green path; the tag job
  will answer `release=0` on the merge.
- **The first automated `v1.0.2` may be allowed to cut**, once R-2's second
  changeset is in — otherwise the release it cuts is the one whose changelog is
  wrong. R-1 should land with it; it is a two-line test change.
- §18.1 rubric unchanged: **2**. Nothing in `2382cd4` introduces a Google
  Messages domain name, and the `--scopes` / `admin` / `messages:read`
  vocabulary in the new CLI documentation is the vocabulary the server already
  serves.
