---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from b0d61b4 to d7b1aaf, carrying the maintained background-session patch forward. Upstream adds nil-receiver guards to the public Client methods, streams attachment downloads instead of buffering them, caps avatar downloads at 5 MiB, and takes the whole ListConversationsRequest; Agent GM adapts its two call sites and keeps the []byte Download contract. ConfigVersion is unchanged and still stale against Google, so conversation creation stays exposed until upstream publishes a newer one.
