#!/usr/bin/env bash
# no-real-numbers -- spec section 13.3.
#
# This repository is public. No commit, no fixture, no doc page, no example and
# no commit message may contain a real phone number. The approved live-gate
# numbers live in the operator's private notes and reach a live test only
# through AGENT_GM_LIVE_NUMBERS or the untracked testdata/live-numbers.local.
#
# Fictional 555 numbers (+12025550123) are reserved for examples and fixtures
# and are allowed here -- they must never be dialled.
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# A NANP-looking number: an optional +1 or 1 country code, a 3-digit area code
# starting 2-9, a 3-digit exchange starting 2-9, and 4 subscriber digits, with
# the usual separators. The lookarounds stop a longer run of digits -- a
# pseudo-version, a millisecond timestamp, a hash -- from matching a substring.
pattern='(?<![0-9])(\+?1[-. ]?)?\(?[2-9][0-9]{2}\)?[-. ]?[2-9][0-9]{2}[-. ]?[0-9]{4}(?![0-9])'

# Generated files whose content nobody wrote by hand, and which are pure
# base64 or hex. Scanning them produces false positives and no protection.
excludes=(
  ':(exclude)go.sum'
  ':(exclude)devbox.lock'
  ':(exclude)testdata/live-numbers.local'
)

# A meta-test passes explicit paths; CI and a developer shell pass none and
# every tracked file is scanned.
if [ "$#" -gt 0 ]; then
  files=$(printf '%s\n' "$@")
else
  files=$(git ls-files -- . "${excludes[@]}")
fi

status=0
while IFS= read -r file; do
  [ -z "$file" ] && continue
  # Skip anything that is not text.
  if ! grep -Iq . "$file" 2>/dev/null; then continue; fi
  while IFS= read -r hit; do
    [ -z "$hit" ] && continue
    lineno="${hit%%:*}"
    rest="${hit#*:}"
    # Strip separators, then drop a leading country code, and look at the
    # exchange. A 555 exchange is a fictional example and is allowed.
    digits=$(printf '%s' "$rest" | tr -cd '0-9')
    if [ "${#digits}" -eq 11 ]; then digits="${digits:1}"; fi
    if [ "${#digits}" -eq 10 ] && [ "${digits:3:3}" = "555" ]; then continue; fi
    printf 'no-real-numbers: %s:%s looks like a real phone number: %s\n' "$file" "$lineno" "$rest" >&2
    status=1
  done < <(grep -noP "$pattern" "$file" 2>/dev/null)
done <<< "$files"

if [ "$status" -ne 0 ]; then
  cat >&2 <<'MSG'

Spec section 13.3: no real phone number goes into this repository. Use the
placeholders <APPROVED_DIRECT_NUMBER>, <APPROVED_GROUP_NUMBER_1> and
<APPROVED_GROUP_NUMBER_2>, or a fictional 555 number for a fixture. A commit
that trips this check is reverted, not amended.
MSG
  exit 1
fi

echo "no-real-numbers: clean"
