package api_test

import (
	"context"
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
// in correlateEcho, or the COALESCE on size_bytes in UpsertAttachment, and
// this fails at "the outgoing attachment reports size 0". Planted 2026-09-07.
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
	}).ok(t, 201)
	uploadID := getString(t, upload.Data, "upload_id")
	token := getString(t, upload.Data, "token")
	uploadURL := getString(t, upload.Data, "upload_url")
	s.put(uploadURL, token, []byte(body)).ok(t, 200)

	s.call("POST", "/v1/conversations/"+conv.ID+"/messages", map[string]any{
		"upload_ids":        []string{uploadID},
	}).ok(t, 200)

	drainEcho(t, s, accountID)

	// The message then walks a rung up the delivery ladder, and Google
	// replays the thread the way it does on a reconnect: the same message,
	// now carrying `sent`, marked old (section 5.4) and -- like every echo of
	// our own media -- carrying no size. The status moved, so this replay is
	// not a no-op: applyParts upserts the attachment from a sizeless
	// MediaContent. And a replay suppresses side effects, so the correlation
	// that filled the size in the first place does not run again.
	//
	// Without the COALESCE in section 4.2 the size is right until the first
	// reconnect and null forever after, which is worse than never having had
	// it: it appears once and then goes.
	s.backend(accountID).StepDelivery()
	replayThread(t, s, accountID, "conv-size")

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

// replayThread re-ingests every message Google would hand back for a thread,
// marked old, through the same Ingester the event loop uses for a replayed
// event (internal/accounts/supervisor.go, `e.IsOld`).
func replayThread(t *testing.T, s *server, accountID, conversationSourceID string) {
	t.Helper()
	ctx := context.Background()
	msgs, _, err := s.backend(accountID).ListMessages(ctx, conversationSourceID, 50, nil)
	if err != nil {
		t.Fatalf("listing the thread: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("the thread is empty, so the replay proves nothing")
	}
	in := s.engine(accountID).Ingest()
	for _, m := range msgs {
		if _, err := in.IngestMessage(ctx, m, true, true); err != nil {
			t.Fatalf("replaying the thread: %v", err)
		}
	}
}
