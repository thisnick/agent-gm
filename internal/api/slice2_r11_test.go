package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/store"
)

// R-11. Every attachment was written `pending` unconditionally at ingest, and
// the only production caller that could change it was guarded by
// `!= available`, so NOTHING could ever mark one available. The consequence
// reached all the way out: `GET /v1/attachments/{id}/content` refuses
// anything not `available`, so it was unreachable for every real attachment,
// `sha256` was always null, and every download ticket minted could never be
// redeemed. A caller got `media_pending` back for bytes it had just uploaded.
//
// Test 18 passed anyway, because the harness flipped the state itself -- a
// transition no production code makes. This test makes the whole round trip
// through production and asserts the bytes come back.
//
// Plant: write store.DownloadStatePending unconditionally in ingest.go again
// and this fails at "download_state is pending". Planted 2026-09-07.
func TestSlice2_R11_UploadedBytesComeBackDown(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountID := s.addAccount("r11@example.test")
	conv := s.seedConversation(accountID, "conv-r11")
	body := jpegFixture()

	upload := s.call("POST", "/v1/uploads", map[string]any{
		"filename":          "fixture.jpg",
		"mime_type":         "image/jpeg",
		"size_bytes":        len(body),
		"client_request_id": key("r11-reserve"),
	}).ok(t, 201)
	uploadID := getString(t, upload.Data, "upload_id")
	s.put(getString(t, upload.Data, "upload_url"), getString(t, upload.Data, "token"), body).ok(t, 200)

	s.call("POST", "/v1/conversations/"+conv.ID+"/messages", map[string]any{
		"upload_ids":        []string{uploadID},
		"client_request_id": key("r11-send"),
	}).ok(t, 200)
	drainEcho(t, s, accountID)

	att := onlyAttachment(t, s, accountID)
	if att.DownloadState != store.DownloadStateAvailable {
		t.Fatalf("download_state is %q, want available: the bytes were just supplied, "+
			"and anything but available makes the content route unreachable",
			att.DownloadState)
	}

	// The metadata route mints a ticket, and the ticket redeems. Both were
	// impossible while the row said pending.
	meta := s.call("GET", "/v1/attachments/"+att.ID, nil).ok(t, 200)
	token := getString(t, meta.Data, "token")
	downloadURL := getString(t, meta.Data, "download_url")
	if token == "" || downloadURL == "" {
		t.Fatal("no download ticket was issued")
	}
	if sha, _ := meta.Data["sha256"].(string); sha == "" {
		t.Error("sha256 is null; it is the digest of the DECRYPTED bytes and they are here")
	}

	got := s.getBytes(t, token, strings.TrimPrefix(downloadURL, publicURL))
	if string(got) != string(body) {
		t.Errorf("the downloaded bytes differ from the uploaded ones (%d vs %d)",
			len(got), len(body))
	}
}

// An attachment with no media ID and no thumbnail has nothing to fetch and
// never will, so it is `unavailable` rather than `pending`: `pending` would
// promise bytes that are not coming, and the caller would poll for ever.
func TestSlice2_R11_DownloadStateIsDerivedFromTheAttachment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		media     string
		thumbnail string
		want      string
	}{
		{"a media ID is available", "media-1", "", store.DownloadStateAvailable},
		{"a thumbnail alone is pending", "", "thumb-1", store.DownloadStatePending},
		{"a media ID wins over a thumbnail", "media-1", "thumb-1", store.DownloadStateAvailable},
		{"neither is unavailable", "", "", store.DownloadStateUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := store.DownloadStateFor(tc.media, tc.thumbnail); got != tc.want {
				t.Errorf("DownloadStateFor(%q, %q) = %q, want %q",
					tc.media, tc.thumbnail, got, tc.want)
			}
		})
	}
}

func onlyAttachment(t *testing.T, s *server, accountID string) store.Attachment {
	t.Helper()
	env := s.call("GET", "/v1/messages", nil).ok(t, 200)
	for _, item := range env.items() {
		m, _ := item.(map[string]any)
		atts, _ := m["attachments"].([]any)
		if len(atts) == 0 {
			continue
		}
		first, _ := atts[0].(map[string]any)
		id, _ := first["id"].(string)
		row, err := s.Store.Attachment(t.Context(), id)
		if err != nil {
			t.Fatalf("reading attachment %s: %v", id, err)
		}
		return row
	}
	t.Fatal("no message carries an attachment")
	return store.Attachment{}
}

func jpegFixture() []byte {
	return []byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00,
		0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9,
	}
}

// getBytes fetches a raw body with a ticket in the Authorization header --
// never in the URL, which is the whole of section 10.3's token rule.
func (s *server) getBytes(t *testing.T, token, path string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.HTTP.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.HTTP.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, truncate(body))
	}
	// Section 10.1: an attachment is served for saving, never for rendering.
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", got)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the bytes were served without nosniff")
	}
	return body
}
