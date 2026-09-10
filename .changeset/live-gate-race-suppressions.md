---
"@agent-gm/cli": patch
---

Keep `-race` on the live gate and stop it failing on the pinned libgm's own unsynchronised disconnect handshake: `devbox run test-live` now passes a `race_top:` ThreadSanitizer suppression list (`scripts/race-suppressions.txt`) naming only upstream access sites, so a race in Agent GM's own code still fails the gate while the two pre-existing upstream ones no longer make a red run uninformative. `internal/lint` holds the list to that shape, and the changeset check no longer demands a version bump for a `_test.go` or `testdata/` change, neither of which the Go toolchain puts in any build.
