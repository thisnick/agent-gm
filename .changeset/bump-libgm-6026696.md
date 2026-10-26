---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from c1697e9 to 6026696, carrying the maintained background-session patch forward unchanged (it applies cleanly). Upstream's media upload and download calls now take a `context.Context`, so cancelling a request aborts the transfer instead of abandoning it; media HTTP failures now surface as typed HTTP errors. ConfigVersion is unchanged. The live gate of spec §3.6(d) has not yet been run.
