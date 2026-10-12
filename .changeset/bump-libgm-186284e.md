---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from caaeaa7 to 186284e, carrying the maintained background-session patch forward unchanged. The only vendored change is a nil-receiver guard in `ListConversations`. ConfigVersion is unchanged. The live gate of spec §3.6(d) has not yet been run.
