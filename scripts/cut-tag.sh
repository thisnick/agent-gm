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
#   3. Does the tag already exist? A tag that is moved is a tag nobody can
#      pin: the release page, the signed checksums, the GHCR digest and the
#      published npm version for that tag are already out there, and a second
#      one built from a different commit would silently disagree with all of
#      them. A repeated push -- a re-run, a revert-and-remerge that lands the
#      same version -- stops here.
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

# Local first: a checkout with `fetch-depth: 0` has the tags, and asking git
# is free. The remote is the authority when the workflow asks it to be.
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  die "$tag already exists in this repository. The version went $previous -> $version," \
      "but that release has already been cut: its release page, its signed checksums, its" \
      "GHCR digest and its npm version are published and a second $tag would contradict" \
      "all four. Bump to a new version instead of re-cutting this one."
fi
if [ "${AGENT_GM_TAG_CHECK_REMOTE:-0}" = 1 ]; then
  if git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null 2>&1; then
    die "$tag already exists on origin (it is not in this checkout, so the local check" \
        "passed and this one did not). That release has been cut; bump to a new version."
  fi
fi

echo "release=1"
echo "cut-tag: npm/package.json went ${previous:-none} -> $version; $tag is free" >&2
