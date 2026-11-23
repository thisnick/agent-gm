---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from f756f0c to ed5dc19, carrying the maintained background-session patch forward unchanged (it applies cleanly). Upstream's `SetTyping` now takes an explicit typing flag (Agent GM keeps sending "typing"), adds a `PairedPhoneRegistered` check for a paired phone removed from the Google account, stops counting a closed recovery ping as a response, and moves the nested module to newer `go.mau.fi/util` and `golang.org/x` releases. ConfigVersion is unchanged. The live gate of spec §3.6(d) has not yet been run.
