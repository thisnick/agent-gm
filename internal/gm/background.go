package gm

import (
	"context"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

// RegisterWebPush registers an externally reachable encrypted push endpoint.
func (b *LibGM) RegisterWebPush(ctx context.Context, endpoint string, public, auth []byte) error {
	return b.client.RegisterPush(ctx, &libgm.PushKeys{URL: endpoint, P256DH: public, Auth: auth})
}

// PollBackgroundOnce is an experimental diagnostic, not the server lifecycle.
// Call only on an otherwise idle client. Upstream drains pending events and
// closes after a short idle period, without running postConnect (which claims
// the active session). The local library patch disables its recovery pinger; physical
// notification behavior must still be checked on the phone. Events must be consumed and the
// refreshed session saved by the caller after it returns.
//
// This diagnostic does not take a deadline; run it in a separate process when
// a hard deadline is needed.
func (b *LibGM) PollBackgroundOnce() error {
	return b.client.ConnectBackground(context.Background())
}

// BackgroundRequest executes a diagnostic batch with a bounded passive listener.
func (b *LibGM) BackgroundRequest(ctx context.Context, request func(context.Context) error) error {
	return b.client.RunBackground(ctx, request)
}
