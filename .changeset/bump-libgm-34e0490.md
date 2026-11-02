---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from 6026696 to 34e0490, carrying the maintained background-session patch forward unchanged (it applies cleanly). Upstream now reads the long-poll stream with a 32 KiB buffer, and a Google-account pairing that times out against a phone not seen for 48 hours wraps a new `ErrPhoneInactive`, which still surfaces as `pairing_init_timeout`. ConfigVersion is unchanged. The live gate of spec §3.6(d) has not yet been run.
