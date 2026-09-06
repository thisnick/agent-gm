package store_test

import (
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
	"github.com/thisnick/agent-gm/internal/store"
)

// N-1. `failed` was a declared value of section 4.2's closed vocabulary that
// nothing could produce -- the worst kind, because a caller branches on it and
// never exercises the branch.
//
// Section 4.4 maps INCOMING_DOWNLOAD_FAILED(106) and its siblings to a
// message delivery_state of `download_failed`, and such a message can still
// carry a media ID: Google said the media exists and then said the download
// did not work. Reading the media ID alone served that attachment as
// `available`, and the redemption failed against Google instead of the route
// saying `failed` up front.
//
// Plant: drop the delivery-state veto from DownloadStateForMessage and the
// download_failed cases below fail. Planted 2026-09-07.
func TestDownloadStateIsVetoedByAFailedMessage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		media     string
		thumbnail string
		delivery  gm.DeliveryState
		want      string
	}{
		{"a media ID is available", "media-1", "", gm.DeliveryStateReceived, store.DownloadStateAvailable},
		{"a thumbnail alone is pending", "", "thumb-1", gm.DeliveryStateReceived, store.DownloadStatePending},
		{"neither is unavailable", "", "", gm.DeliveryStateReceived, store.DownloadStateUnavailable},
		{"a failed download beats a media ID", "media-1", "", gm.DeliveryStateDownloadFailed, store.DownloadStateFailed},
		{"a failed download beats a thumbnail", "", "thumb-1", gm.DeliveryStateDownloadFailed, store.DownloadStateFailed},
		{"a failed download with nothing at all", "", "", gm.DeliveryStateDownloadFailed, store.DownloadStateFailed},
		{"downloading is not failed", "media-1", "", gm.DeliveryStateDownloading, store.DownloadStateAvailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := store.DownloadStateForMessage(tc.media, tc.thumbnail, string(tc.delivery))
			if got != tc.want {
				t.Errorf("DownloadStateForMessage(%q, %q, %q) = %q, want %q",
					tc.media, tc.thumbnail, tc.delivery, got, tc.want)
			}
		})
	}
}

// Every value of section 4.2's closed vocabulary is reachable. A value
// nothing can produce is one a caller branches on and never exercises, which
// is how `failed` sat unreachable for a whole slice.
func TestEveryDownloadStateIsReachable(t *testing.T) {
	produced := map[string]bool{}
	for _, tc := range []struct{ media, thumb, delivery string }{
		{"m", "", string(gm.DeliveryStateReceived)},
		{"", "t", string(gm.DeliveryStateReceived)},
		{"", "", string(gm.DeliveryStateReceived)},
		{"m", "", string(gm.DeliveryStateDownloadFailed)},
	} {
		produced[store.DownloadStateForMessage(tc.media, tc.thumb, tc.delivery)] = true
	}
	for _, want := range []string{
		store.DownloadStateAvailable, store.DownloadStatePending,
		store.DownloadStateUnavailable, store.DownloadStateFailed,
	} {
		if !produced[want] {
			t.Errorf("no input produces download_state %q, though section 4.2 declares it", want)
		}
	}
}
