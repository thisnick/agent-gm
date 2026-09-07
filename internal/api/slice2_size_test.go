package api_test

import (
	"testing"
)

// The Slice 2 live gate, finding 2: every OUTGOING attachment row was written
// with `size_bytes` null while the same account's incoming ones carried
// theirs. Google's echo of media we sent returns a MediaContent with Size
// unset, so the byte count -- which the upload reservation had counted and
// the send had handed to Google -- was dropped on the floor between the two.
//
// It is now written onto the operation before the send and put on the
// attachment when the echo is correlated, so it survives a restart between
// them (migration 0005).
//
// Plant: drop the SetOperationMediaSize call in core.SendMedia, or the fill
// in correlateEcho, and this fails at "the outgoing attachment reports size
// 0". Planted 2026-09-07.
func TestSlice2_AnOutgoingAttachmentCarriesTheSizeTheReservationCounted(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountID := s.addAccount("size@example.test")
	conv := s.seedConversation(accountID, "conv-size")

	body := string([]byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00,
		0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9,
	})
	upload := s.call("POST", "/v1/uploads", map[string]any{
		"filename":          "fixture.jpg",
		"mime_type":         "image/jpeg",
		"size_bytes":        len(body),
		"client_request_id": key("size-reserve"),
	}).ok(t, 201)
	uploadID := getString(t, upload.Data, "upload_id")
	token := getString(t, upload.Data, "token")
	uploadURL := getString(t, upload.Data, "upload_url")
	s.put(uploadURL, token, []byte(body)).ok(t, 200)

	s.call("POST", "/v1/conversations/"+conv.ID+"/messages", map[string]any{
		"upload_ids":        []string{uploadID},
		"client_request_id": key("size-send"),
	}).ok(t, 200)

	drainEcho(t, s, accountID)

	messages := s.call("GET", "/v1/messages", nil).ok(t, 200)
	seen := 0
	for _, item := range messages.items() {
		m, _ := item.(map[string]any)
		atts, _ := m["attachments"].([]any)
		for _, a := range atts {
			att, _ := a.(map[string]any)
			seen++
			size, _ := att["size"].(float64)
			if int(size) != len(body) {
				t.Errorf("the outgoing attachment reports size %v, want %d: the "+
					"reservation counted the bytes and the send handed them to "+
					"Google, so a null here is information thrown away, not "+
					"information Google withheld", att["size"], len(body))
			}
		}
	}
	if seen != 1 {
		t.Fatalf("the send produced %d attachment rows, want exactly 1", seen)
	}
}
