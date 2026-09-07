#!/usr/bin/env bash
# The name lint of spec section 13.5.
#
# It reads the catalogue the server actually serves -- tool names,
# descriptions, argument names and descriptions, enums, and the `initialize`
# instructions block -- plus every markdown page under `docs/`, and fails on a
# Matrix vocabulary asserted as a live contract.
#
# It is a Go test rather than a grep because two of its three scoping rules
# cannot be expressed as one: the served catalogue has to be READ from the
# server's own tables, and the banned `mode` is an argument NAME matched whole
# rather than a substring anywhere in a file.
#
# The meta-test (TestNameLintExemptions and its siblings) runs under
# `devbox run test` with everything else; this script is the named entry point
# section 16 Slice 3 test 24 calls.
set -euo pipefail

cd "$(dirname "$0")/.."

exec go test ./internal/lint/ -run 'TestNameLint|TestABareModeArgumentIsCaught' -count=1
