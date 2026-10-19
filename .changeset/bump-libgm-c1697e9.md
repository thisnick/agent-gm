---
"@agent-gm/cli": patch
---

Bump the pinned mautrix-gmessages (libgm) dependency from 186284e to c1697e9, carrying the maintained background-session patch forward. Upstream added re-pairing with the previously paired phone (`DestPhoneID`, `HasOtherGaiaPhone`), a new `conversations.proto` field, and an atomic `longPollingConn`; only the `Client` struct hunk of the patch needed a manual port. ConfigVersion is unchanged. The live gate of spec §3.6(d) has not yet been run.
