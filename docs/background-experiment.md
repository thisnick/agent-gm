# Push background mode

The server defaults to `AGENT_GM_CONNECTION_MODE=push`. It registers a Web
Push subscription with Google using the public HTTPS URL, receives encrypted
notifications, and opens a bounded passive listener to fetch pending updates.
API operations open the same serialized listener on demand. Reads refresh data older than one minute; sends always refresh destination metadata before sending. Startup performs
reconciliation. A passive reconciliation sweep runs every 15 minutes. There is no active recovery pinger.
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

The [libgm patch maintenance record](../third_party/mautrix-gmessages/AGENT_GM_PATCHES.md)
contains the exact patch, reapplication instructions, test inventory, and known
limitations (including final acknowledgment cancellation). Passive batches initialize an RPC session ID,
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

## Freshness and bounded catch-up

Conversation listings and message searches refresh the recent 100 conversations
per included folder when stale. Direct thread reads refresh that thread's
metadata and messages, including threads outside the recent list. Contacts have
a separate one-minute freshness window. Signed-out history remains readable
without a live refresh; a failed required refresh on a paired account returns
an error instead of silently presenting stale data as current.

Successful refresh start and completion times are persisted in `refresh_state`, by account and
scope (`conversations`, `contacts`, or conversation ID). Completion time controls the one-minute freshness window. Start times are committed
only after the fetch succeeds, so messages arriving during the fetch remain in
the next catch-up interval. Message pagination stops before the first message
older than the previous successful sync timestamp; IDs deduplicate repeated
fetches. This bounds recent synchronization, not every historical edit/deletion.
Newly discovered threads use the account sweep cutoff; backfill covers history.

Concurrent stale requests share one refresh. Each batch reuses a passive
listener. Before text or media sends, the destination metadata is always fetched
and saved before capability checks and the send. Refreshing does not advance a
thread's message checkpoint unless messages were also fetched. Existing send
idempotency and uncertain-outcome handling remain in effect.

Safe application logs record refresh start/completion/failure and push receipt
and completion. The encrypted push sidecar also stores last receipt/completion
timestamps. No message bodies, phone numbers, push URLs or keys are logged.

Schema migration 8 adds freshness state. Back up data before upgrading. An older
binary cannot open the newer schema; rollback requires its pre-upgrade backup
(or a forward fix), not just changing the image tag.
