#!/usr/bin/env bash
# changeset-check -- a pull request that changes what Agent GM ships carries a
# changeset. Spec section 14.3, decision D39.
#
#   git diff --name-only origin/main...HEAD | scripts/changeset-check.sh
#   scripts/changeset-check.sh <file-of-changed-paths>
#   AGENT_GM_PR_LABELS="no-release" … | scripts/changeset-check.sh
#
# It reads the changed paths on stdin or from a file, one per line, and the
# labels from AGENT_GM_PR_LABELS (comma or newline separated). It writes its
# verdict and exits 0 or 1. Nothing here talks to GitHub: the workflow supplies
# the two facts and the judgement is a script anybody can run locally against
# their own branch (section 13.6), which is also what makes it testable.
#
# The rule, and why each half of it:
#
#   - a changed path under `.changeset/` that is not the config or the README
#     IS a changeset -- pass, whatever else changed;
#   - a pull request that changed only documentation needs none: prose does not
#     ship in an archive, and requiring a version bump for a typo would either
#     cut releases nobody wants or teach everyone to reach for `no-release`,
#     which is how a bypass stops meaning anything;
#   - the `no-release` label is the deliberate exception, named in the output
#     so it is visible in the log of the run that used it;
#   - everything else -- Go, the Dockerfile, devbox.json, scripts/, npm/, the
#     workflows -- is a change to what is shipped or to how it is shipped, and
#     a release that omits it is a release whose changelog is wrong.
set -euo pipefail

die() { echo "changeset-check: $*" >&2; exit 1; }

label_bypass="no-release"

labels="${AGENT_GM_PR_LABELS:-}"
labels="${labels//,/$'\n'}"

if [ $# -gt 0 ]; then
  [ -f "$1" ] || die "no such file: $1"
  changed="$(cat "$1")"
else
  changed="$(cat)"
fi

if [ -z "${changed//[[:space:]]/}" ]; then
  echo "changeset-check: this pull request changes no files; nothing to release"
  exit 0
fi

has_changeset=0
code_paths=()

while IFS= read -r p; do
  [ -n "$p" ] || continue
  case "$p" in
    .changeset/config.json|.changeset/README.md) ;;                 # the plumbing, not a change
    .changeset/*.md) has_changeset=1 ;;
    .changeset/*) ;;
    # Documentation, and only documentation. `plans/` is the spec, `docs/` is
    # the manual, and a top-level *.md is the README and its neighbours. None
    # of them is in an archive, an image or the npm tarball.
    docs/*|plans/*|LICENSE) ;;
    *.md) case "$p" in */*) code_paths+=("$p") ;; esac ;;
    *) code_paths+=("$p") ;;
  esac
done <<<"$changed"

if [ "$has_changeset" = 1 ]; then
  echo "changeset-check: this pull request carries a changeset"
  exit 0
fi

if [ "${#code_paths[@]}" = 0 ]; then
  echo "changeset-check: documentation only; no changeset needed"
  exit 0
fi

while IFS= read -r l; do
  l="${l//[[:space:]]/}"
  [ -n "$l" ] || continue
  if [ "$l" = "$label_bypass" ]; then
    echo "changeset-check: labelled '$label_bypass'; ${#code_paths[@]} changed path(s) will not appear in a changelog"
    exit 0
  fi
done <<<"$labels"

{
  echo "changeset-check: this pull request changes what Agent GM ships and carries no changeset."
  echo
  echo "  The version of the image, of both binaries and of @agent-gm/cli comes from"
  echo "  npm/package.json, and a changeset is the only thing that moves it (D39). Without"
  echo "  one this change is released under a version whose changelog does not mention it."
  echo
  echo "  Add one:      devbox run changeset"
  echo "  Or, if it genuinely ships nothing, label the pull request '$label_bypass'."
  echo
  echo "  The paths that need one:"
  printf '    %s\n' "${code_paths[@]}"
} >&2
exit 1
