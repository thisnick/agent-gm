# @agent-gm/cli

## 1.3.1

### Patch Changes

- 267b3f7: Keep `-race` on the live gate and stop it failing on the pinned libgm's own unsynchronised disconnect handshake: `devbox run test-live` now passes a `race_top:` ThreadSanitizer suppression list (`scripts/race-suppressions.txt`) naming only upstream access sites, so a race in Agent GM's own code still fails the gate while the two pre-existing upstream ones no longer make a red run uninformative. `internal/lint` holds the list to that shape, and the changeset check no longer demands a version bump for a `_test.go` or `testdata/` change, neither of which the Go toolchain puts in any build.
- 7136486: Bump the pinned mautrix-gmessages (libgm) dependency from be48a58 to b0d61b4, carrying the maintained background-session patch forward onto upstream's now-native context-threaded Connect/ConnectBackground/Reconnect/long-polling API. ConfigVersion is unchanged, and still stale against Google, so conversation creation stays exposed until upstream publishes a newer one. The live gate of spec section 3.6(d) passed: list, a real text to the approved number with its echo and delivery states, and the inbound reply.

## 1.3.0

### Minor Changes

- f14e4ea: Refresh stale conversation, message and contact reads on demand, always refresh destinations before sending, and restore passive catch-up every fifteen minutes with durable freshness tracking and push diagnostics.

## 1.2.0

### Minor Changes

- 1f8659e: Default the server to durable Web Push background delivery and bounded passive API sessions, with an explicit active-mode fallback.

### Patch Changes

- 209ba90: Hide expired and inactive entries from admin enrollment-code, client, and authorization-request lists by default; add --all to inspect their history.

## 1.1.0

### Minor Changes

- 4b9b134: Add latest-activity date ranges and pinned-only conversation filters; fix participant search, direct-only filtering, and folder/type schema mismatches across MCP, REST and CLI.

## 1.0.5

### Patch Changes

- 5f4ddb8: Add copy buttons to the OAuth enrollment, review, and approval commands, with copy confirmation and manual selection when clipboard access is unavailable.

## 1.0.4

### Patch Changes

- 95ae3dd: Make OAuth enrollment and CLI approval instructions clear, add a simple responsive design, and fix browser security policies so approval polling and the client callback work. Refreshing the completion URL safely displays the request status.

  Fix CLI enrollment and approval scope flags to send JSON arrays as required by the server.

## 1.0.3

### Patch Changes

- 2649e20: The OAuth pages carry `Referrer-Policy: same-origin`, so a browser's own enrollment form post no longer arrives as `Origin: null` and is refused; a refused post now names the origin it received and is logged at warn

## 1.0.2

### Patch Changes

- 42a72e5: `agm auth login --admin` prints a hint when it grants all four scopes; admin sessions are narrowed with `--scopes`
- 7fae2a9: Versions are managed with changesets; the container, binaries and npm package share one version
