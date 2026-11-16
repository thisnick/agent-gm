---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from 3bf5e3a to f756f0c, carrying the maintained background-session patch forward unchanged (it applies cleanly). Upstream now reports a failed attachment download as an HTTP error instead of trying to decrypt the error page, accepts Google-account pairing server keys with short coordinates (fixing an intermittent pairing failure), and fills in conversation and message IDs from nested fields when Google sends them without the top-level ones. ConfigVersion is unchanged. The live gate of spec §3.6(d) has not yet been run.
