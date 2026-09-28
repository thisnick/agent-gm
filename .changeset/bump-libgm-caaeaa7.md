---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from d7b1aaf to caaeaa7, carrying the maintained background-session patch forward. Upstream acks pending events immediately on logout, and `FinishGaiaPairing`'s returned phone ID now prefers a new `DestRegDevice.UnknownTS` field over the old `UnknownInt` whenever Google populates it, so a pairing performed after this bump may store a differently-shaped phone_id than one performed before it; phone_id is not an account key, so existing and new accounts are identified as before. ConfigVersion is unchanged and still stale against Google, so conversation creation stays exposed until upstream publishes a newer one. The live gate of spec §3.6(d) is not yet recorded; see `docs/upstream-pin.md`.
