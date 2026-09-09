# Push background mode

The server defaults to `AGENT_GM_CONNECTION_MODE=push`. It registers a Web
Push subscription with Google using the public HTTPS URL, receives encrypted
notifications, and opens a bounded passive listener to fetch pending updates.
API operations open the same serialized listener on demand. Startup performs
reconciliation. There is no periodic message sweep or active recovery pinger.
An idle account has no open Google listener. `connected` means the push
subscription is ready, including while its transport is idle.

`AGENT_GM_CONNECTION_MODE=active` selects the previous persistent connection.
That mode can suppress phone sound/vibration while it owns the active session.
Push mode avoids claiming an active browser session; the phone's physical
notification behavior still needs a manual check with its screen locked.

## Deployment

Use the ordinary changeset, CI, GHCR image, and digest-pinned Compose process.
Keep the existing data mount and data key. The existing public HTTPS tunnel
must route `/push/` to the server without requiring browser authentication.
The endpoint uses a random capability and authenticated Web Push encryption;
there is no bearer token to configure at Google. Both `aesgcm` (observed from
Google) and `aes128gcm` are supported. Do not log full push URLs.

Each account's encrypted `<account>-push` session sidecar stores the stable
endpoint capability, encryption keys and pending wake counters. Include it
in backups alongside the pairing. A push is acknowledged only after its wake
is saved. Failed fetches retry pending work with bounded exponential backoff;
new pushes are coalesced. Restart reuses the subscription and resumes saved
wakes. Signing out removes the sidecar. No new external push service is needed.

Stop other processes using the same pairing before starting the new image.
For rollback, stop the push server and use the previous image with the saved
data mount and key. Keep a backup before rollout. Active mode may bring back
the notification suppression that motivated this change.

## Implementation and verification

`third_party/mautrix-gmessages/AGENT_GM_PATCHES.md` records the small extension
to the pinned upstream library. Passive batches initialize an RPC session ID,
keep the listener open while a request is pending, drain and acknowledge
updates, then close. They do not send GET_UPDATES or start the recovery pinger.
Pairing does not automatically reconnect in active mode.

Tests cover both encryption formats, rejected/corrupted pushes, durable HTTP
acknowledgement, coalescing, pending-work retries, idle behavior and cancellation.
`devbox run check` includes the local library's race test. Live checks use an
isolated copy of the paired data and a second test phone, verifying the normal
REST send path and push ingestion back into the message store. Device sound
and vibration cannot be established by server logs alone.

`spike background-once` and `spike push-probe` remain diagnostic commands.
Run them only when every other process using that pairing is stopped. They
are not the production ingestion path and do not persist message history.
