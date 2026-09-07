#!/usr/bin/env bash
# cut-tag -- the judgement behind the tag that cuts a release. D39, §14.3.
#
#   scripts/cut-tag.sh <previous-version>
#
# Given the version `npm/package.json` carried BEFORE this push, it answers
# whether this push is a release, and refuses the two ways that answer can be
# wrong. It prints `version=…` and `release=0|1` in the shape a GitHub Actions
# output wants, and creates nothing: pushing the tag is the workflow's line,
# and it runs only after this one has said yes.
#
# The three questions:
#
#   1. Did the version change? Only the merge of the Version Packages pull
#      request changes it, so this is what distinguishes "a release" from
#      every other push to main. An ordinary merge answers no and the job ends
#      there.
#   2. Is it strict semver? `scripts/release.sh version` refuses anything else
#      before this script sees it, because a version that npm will reject must
#      be rejected before a tag exists rather than at the last step of a
#      release.
#   3. Does the tag already exist? Two different situations wear that one
#      answer. If the existing tag is REACHABLE from this commit, the version
#      it names is already released and is already in this history: there is
#      nothing to cut, and that is an ordinary push rather than a fault. It is
#      also what the very first push through this machinery looks like, when
#      the manifest moves from a placeholder to the version that is already
#      out. If the tag exists and is NOT reachable, it names a different
#      commit -- and a moved tag is a tag nobody can pin, because the release
#      page, the signed checksums, the GHCR digest and the published npm
#      version for it are already out there and a second build would silently
#      disagree with all four. That one refuses.
#   4. Does CHANGELOG.md have an entry for it? The version is supposed to move
#      only through a Version Packages pull request, which writes the entry in
#      the same commit -- but that is a rule, and this is the guard. Without
#      it a hand-edited manifest cuts a signed, public, unwithdrawable release
#      for a version the changelog has never heard of.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

die() { echo "cut-tag: $*" >&2; exit 1; }

[ $# -ge 1 ] || die "usage: cut-tag.sh <previous-version>; pass an empty string if there was none"
previous="$1"

# The version authority, asked through the one script that reads it, so the
# semver refusal is the same refusal `release.sh` would give.
version="$(./scripts/release.sh version)"
tag="v$version"

echo "version=$version"

if [ "$previous" = "$version" ]; then
  echo "release=0"
  echo "cut-tag: npm/package.json still says $version; this push is not a release" >&2
  exit 0
fi

# already_cut answers question 3 for one commit: `release=0` and exit when the
# tag is in this history, `die` when it is somewhere else.
already_cut() {
  local at="$1" where="$2"
  if [ -n "$at" ] && git merge-base --is-ancestor "$at" HEAD 2>/dev/null; then
    echo "release=0"
    echo "cut-tag: $tag already exists $where at $at, and that commit is in this history:" \
         "$version is already released and there is nothing to cut." >&2
    exit 0
  fi
  die "$tag already exists $where${at:+ at $at} and is NOT in this history. The version went" \
      "$previous -> $version, but that release was cut from a different commit: its release" \
      "page, its signed checksums, its GHCR digest and its npm version are published and a" \
      "second $tag would contradict all four. Bump to a new version instead of re-cutting this one."
}

# Local first: a checkout with `fetch-depth: 0` has the tags, and asking git
# is free. The remote is the authority when the workflow asks it to be.
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  already_cut "$(git rev-parse -q --verify "refs/tags/$tag^{commit}" || true)" "in this repository"
fi
if [ "${AGENT_GM_TAG_CHECK_REMOTE:-0}" = 1 ]; then
  remote_sha="$(git ls-remote --tags origin "refs/tags/$tag^{}" "refs/tags/$tag" 2>/dev/null | head -1 | cut -f1)"
  if [ -n "$remote_sha" ]; then
    # Reachability is asked of the object only if this checkout has it; a tag
    # on origin whose commit is not here cannot be in this history either.
    git cat-file -e "${remote_sha}^{commit}" 2>/dev/null || remote_sha=""
    already_cut "$remote_sha" "on origin"
  fi
fi

# The changelog entry, which is what makes "changesets moves the version" a
# guard rather than a convention. `$version` is escaped into the pattern
# because a dot in a version is not a wildcard.
changelog_heading="^## \[${version//./\\.}\]"
if ! grep -q "$changelog_heading" CHANGELOG.md; then
  die "CHANGELOG.md has no entry for $version. The version is moved by changesets, which" \
      "writes the entry in the same commit (D39); a manifest edited by hand would otherwise" \
      "cut a signed, public, unwithdrawable release for a version the changelog has never" \
      "heard of. Merge the Version Packages pull request instead of setting the version."
fi

echo "release=1"
echo "cut-tag: npm/package.json went ${previous:-none} -> $version; $tag is free" >&2
