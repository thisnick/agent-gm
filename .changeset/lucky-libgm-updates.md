---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from be48a58 to b0d61b4, carrying the maintained background-session patch forward onto upstream's now-native context-threaded Connect/ConnectBackground/Reconnect/long-polling API. ConfigVersion is unchanged, and still stale against Google, so conversation creation stays exposed until upstream publishes a newer one. The live gate of spec section 3.6(d) passed: list, a real text to the approved number with its echo and delivery states, and the inbound reply.
