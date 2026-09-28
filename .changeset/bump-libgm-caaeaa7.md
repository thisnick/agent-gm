---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from d7b1aaf to caaeaa7, carrying the maintained background-session patch forward. Upstream acks pending events immediately on logout, and `FinishGaiaPairing`'s returned phone ID now prefers a new `DestRegDevice.UnknownTS` field over the old `UnknownInt` whenever Google populates it, so a pairing performed after this bump may embed a differently-shaped integer than one performed before it (existing accounts are unaffected). ConfigVersion is unchanged and still stale against Google, so conversation creation stays exposed until upstream publishes a newer one. The automated checks (build, vet, the direct libgm race suite, fixture-validation) pass; the live gate of spec §3.6(d) has not yet been run and is required before this ships.
