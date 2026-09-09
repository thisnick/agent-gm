# Local libgm extension

This is pkg/libgm from the exact go.mod requirement
v0.2608.1-0.20260904125044-be48a58b7338, with its upstream license.
The Matrix connector is not included. Keep the source pin when updating.

Agent GM adds RunBackground(context, request): a serialized, bounded push/RPC
batch. It initializes an RPC session identifier without sending GET_UPDATES,
opens the listener before sending requests, holds it while an RPC is pending,
then drains and acknowledges updates. Background polling never starts the
recovery pinger, which could otherwise silently reclaim the active session.
An idle close is successful even when no messages were pending.

The main module uses an explicit local replacement so local and container
builds use the same source. This extension is not an upstream release.

Cancellation applies to the listener, retry delays, token refresh and final
acknowledgement. DisablePostPairConnect lets the supervisor register push
without an automatic active reconnect after Google pairing. The nested module
has its own cancellation/no-active-request race test, included in root checks.
