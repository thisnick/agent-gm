---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from 6026696 to 3bf5e3a, carrying the maintained background-session patch forward unchanged (it applies cleanly). Upstream now stops its acknowledgment timer on disconnect, reads the long-poll stream with a smaller buffer, logs more detail when an incoming message can't be decoded, reports a Google-account pairing timeout as "phone hasn't been seen recently" when the phone has been offline for over 48 hours, and returns an error instead of panicking when media is downloaded on a nil client. ConfigVersion is unchanged. The live gate of spec §3.6(d) has not yet been run.
