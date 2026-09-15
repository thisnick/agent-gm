# @agent-gm/cli

## 1.4.0

### Minor Changes

- 9bb19ee: Add owner-declared OAuth clients, for connectors that cannot register themselves. `POST /v1/admin/clients` and `agm admin clients create <client-id> --redirect <uri>` declare a public client whose id the owner chooses, which is what a connector that asks its operator to paste a client*id needs; an id beginning client* is refused, since that prefix is what /oauth/register mints. Such a client may carry --default-resource, letting it omit `resource` at /oauth/authorize, read as this server's one canonical resource. Nothing else is relaxed: the redirect rules, PKCE, the enrollment code and the owner's approval all apply unchanged, and creation writes a client.created audit row. Migration 0009 adds the two columns; existing registrations read back unchanged.

## 1.3.3

### Patch Changes

- d1a0317: Run CI on pull requests and on pushes to `main` and to version tags only. A push to any other branch started a second run of every job beside the pull request's own — the same commit, the same verdict, twice the runners — so the branch push trigger is gone and the pull request run is the one that counts. `main` keeps its push run because `version.yml` waits on it before cutting a tag and `release.yml` requires it green before publishing, and a `v*` tag keeps its run because that is what builds the image `release.yml` waits for.
- 88765f7: Bump the pinned mautrix-gmessages (libgm) dependency from b0d61b4 to d7b1aaf, carrying the maintained background-session patch forward. Upstream adds nil-receiver guards to the public Client methods, streams attachment downloads instead of buffering them, caps avatar downloads at 5 MiB, and takes the whole ListConversationsRequest; Agent GM adapts its two call sites and keeps the []byte Download contract. ConfigVersion is unchanged and still stale against Google, so conversation creation stays exposed until upstream publishes a newer one.

## 1.3.2

### Patch Changes

- 08d9d60: Cache Go's build and module caches, and golangci-lint's, between CI runs. Every job started with them empty, so each one recompiled the standard library three times over — linux, windows and darwin `vet` — and every dependency again race-instrumented, which is where `check`'s five and a half minutes went. Measured locally: 133s cold, 42s warm, 45s warm with Agent GM's own code changed, so the key is the `go.sum`/`devbox.lock` hash rather than the commit, and only the `check` job saves the entry the others read.
- 588d2f7: Reconnect an account in-process after refreshing its Google cookies so it leaves `signed_out` immediately without restarting the server.

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
