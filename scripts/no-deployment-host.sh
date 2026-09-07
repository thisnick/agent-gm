#!/usr/bin/env bash
# no-deployment-host -- the owner's deployment hostname is not a constant.
#
# `AGENT_GM_PUBLIC_URL` is the issuer, the canonical resource and the base of
# every URL this server hands out, and it is configuration. A copy of one
# deployment's hostname sitting in code is a default that works for exactly one
# person and fails silently for everybody else -- and, in a public repository,
# it is also somebody's address written down where they did not put it.
#
# So the hostname appears in exactly two kinds of place, and this job keeps it
# that way:
#
#   - the spec's DEPLOYMENT and DECISION sections, which are about the owner's
#     own deployment and are the record of what was decided;
#   - `docs/deploy.md`, where it is an example clearly labelled as the owner's
#     deployment.
#
# Everywhere else -- source, tests, fixtures, the other docs pages -- uses the
# fixture origin `https://gm.example.test`, which is a name reserved for
# documentation and resolves nowhere.
#
# The same shape as no-real-numbers.sh: a public repository, a string that
# should not spread, and a job that fails rather than a habit that holds.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# The deployment hostname, written so that this script does not itself trip the
# check it performs.
host="agent""-wx"

# Files allowed to name it. Everything else is a failure.
allowed=(
  "plans/AGENT_GM_SPEC.md"
  "docs/deploy.md"
  "scripts/no-deployment-host.sh"
)

is_allowed() {
  local path="$1"
  for a in "${allowed[@]}"; do
    [ "$path" = "$a" ] && return 0
  done
  return 1
}

fail=0
while IFS= read -r path; do
  is_allowed "$path" && continue
  fail=1
  echo "no-deployment-host: $path names the owner's deployment hostname:" >&2
  grep -n -- "$host" "$path" | sed 's/^/  /' >&2
done < <(git grep -l -- "$host" -- . ':(exclude).git' | sort)

if [ "$fail" -ne 0 ]; then
  cat >&2 <<MSG

The deployment hostname belongs in configuration, not in the tree. Use the
fixture origin https://gm.example.test in code, tests and docs; state the
contract against "the configured AGENT_GM_PUBLIC_URL" rather than against one
deployment's value. The spec's deployment and decision sections and
docs/deploy.md may name it, labelled as the owner's deployment.
MSG
  exit 1
fi

echo "no-deployment-host: clean"
