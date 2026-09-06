package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisnick/agent-gm/internal/gm"
)

// R-10i, which the reviewer marked unverified because the fake did not echo
// the media at all: a media send must produce a message row carrying its
// `att_` attachment. Without it "reserve, PUT, send" passed on the operation
// alone, and the attachment the whole sequence exists for was never asserted.
//
// Plant: drop the attachments from the fake's SendMedia echo and this fails
// at "the sent message carries no attachment". Planted 2026-09-07.
func TestSlice2_R10i_AMediaSendProducesAnAttachmentRow(t *testing.T) {
	s := newServer(t)
	defer s.close()

	accountID := s.addAccount("r10i@example.test")
	conv := s.seedConversation(accountID, "conv-r10i")

	// Real JPEG bytes: SOI, a minimal JFIF APP0 header, EOI. The reservation
	// verifies the DETECTED content type against the declared one, so a
	// placeholder string is correctly refused -- which is itself worth
	// knowing, and is why this is generated rather than committed.
	body := string([]byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00,
		0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9,
	})
	upload := s.call("POST", "/v1/uploads", map[string]any{
		"filename":          "fixture.jpg",
		"mime_type":         "image/jpeg",
		"size_bytes":        len(body),
		"client_request_id": key("r10i-reserve"),
	}).ok(t, 201)
	uploadID := getString(t, upload.Data, "upload_id")
	token := getString(t, upload.Data, "token")
	uploadURL := getString(t, upload.Data, "upload_url")

	s.put(uploadURL, token, []byte(body)).ok(t, 200)

	sent := s.call("POST", "/v1/conversations/"+conv.ID+"/messages", map[string]any{
		"upload_ids":        []string{uploadID},
		"client_request_id": key("r10i-send"),
	}).ok(t, 200)

	// message_id is always PRESENT, null until the echo lands (7.7, 6.5).
	raw, err := json.Marshal(sent.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"message_id"`) {
		t.Errorf("the send response omits message_id entirely: %s", raw)
	}

	// The harness does not run the per-account ingest goroutine -- an api
	// test drives routes, not the event loop -- so the echo is applied here,
	// through the SAME core.Ingester the loop uses. That is the path a real
	// echo takes; only the pump differs.
	drainEcho(t, s, accountID)

	messages := s.call("GET", "/v1/messages", nil).ok(t, 200)
	found := false
	for _, item := range messages.items() {
		m, _ := item.(map[string]any)
		atts, _ := m["attachments"].([]any)
		if len(atts) == 0 {
			continue
		}
		found = true
		att, _ := atts[0].(map[string]any)
		if id, _ := att["id"].(string); !strings.HasPrefix(id, "att_") {
			t.Errorf("the attachment's id is %q, want an att_ ID", id)
		}
		if mime, _ := att["mime_type"].(string); mime != "image/jpeg" {
			t.Errorf("the attachment's mime_type is %q", mime)
		}
	}
	if !found {
		t.Error("the sent message carries no attachment; a media send that stores no " +
			"att_ row is indistinguishable from a text send")
	}
}

// drainEcho applies whatever the fake emitted, through the real ingester.
func drainEcho(t *testing.T, s *server, accountID string) {
	t.Helper()
	be := s.backend(accountID)
	ing := s.engine(accountID).Ingest()
	for {
		select {
		case ev := <-be.Events():
			m, ok := ev.(*gm.EventMessage)
			if !ok {
				continue
			}
			if _, err := ing.IngestMessage(context.Background(), m.Message, true, false); err != nil {
				t.Fatalf("ingesting the echo: %v", err)
			}
		default:
			return
		}
	}
}
