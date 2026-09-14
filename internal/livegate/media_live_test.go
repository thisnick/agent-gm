//go:build live

package livegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/gm"
)

// The media leg of the libgm pin gate (docs/upstream-pin.md). It exists
// because a text-only gate says nothing about DownloadMedia, and the
// d7b1aaf bump changed exactly that path.
//
// TestLiveMediaDownloadApprovedConversation is read-only: it downloads every
// attachment in the approved direct conversation(s) through Backend.Download
// and writes the plaintext to AGENT_GM_LIVE_MEDIA_OUT, so the same run from
// the previous pin's checkout can be compared byte for byte with cmp.
func TestLiveMediaDownloadApprovedConversation(t *testing.T) {
	requireGate(t)
	numbers := approvedNumbers(t)
	out := os.Getenv("AGENT_GM_LIVE_MEDIA_OUT")
	if out == "" {
		t.Skip("set AGENT_GM_LIVE_MEDIA_OUT to a directory")
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	env := openLive(t)
	ctx := context.Background()
	a := env.accounts[0]

	convs, err := a.Backend.ListConversations(ctx, gm.FolderInbox, 200)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	var total, bytesTotal int
	for _, c := range convs {
		if c.IsGroup {
			continue
		}
		approved := false
		for _, p := range c.Participants {
			if !p.IsMe && sameNumber(p.PhoneE164, numbers.Direct) {
				approved = true
			}
		}
		if !approved {
			continue
		}
		msgs, _, err := a.Backend.ListMessages(ctx, c.SourceID, 100, nil)
		if err != nil {
			t.Fatalf("ListMessages(%s): %v", c.SourceID, err)
		}
		for _, m := range msgs {
			for _, att := range m.Attachments {
				if att.MediaID == "" || len(att.DecryptionKey) == 0 {
					continue
				}
				dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				start := time.Now()
				data, err := a.Backend.Download(dctx, att.MediaID, att.DecryptionKey)
				cancel()
				if err != nil {
					t.Errorf("Download(part %d of %s, %s, declared %d bytes): %v", att.PartIndex, m.SourceID, att.MimeType, att.SizeBytes, err)
					continue
				}
				sum := sha256.Sum256(data)
				name := fmt.Sprintf("%s-%d.bin", m.SourceID, att.PartIndex)
				if err := os.WriteFile(filepath.Join(out, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
				if att.SizeBytes > 0 && int64(len(data)) != att.SizeBytes {
					// Google declares the original's size but stores a transcode
					// (a HEIF comes back as a JPEG), so this is informational.
					t.Logf("%s: downloaded %d bytes, declared %d", name, len(data), att.SizeBytes)
				}
				t.Logf("downloaded %s: %s %d bytes sha256=%s in %s", name, att.MimeType, len(data), hex.EncodeToString(sum[:8]), time.Since(start).Round(time.Millisecond))
				total++
				bytesTotal += len(data)
			}
		}
	}
	if total == 0 {
		t.Fatal("no downloadable attachment in the approved conversation(s)")
	}
	t.Logf("downloaded %d attachments, %d bytes total", total, bytesTotal)
}

// TestLiveMediaSendEchoAndDownload sends real media: Upload, an immediate
// Download of the fresh upload (must be identical), SendMedia to the approved
// number, the echo carrying the TmpID with an attachment, and at least one
// delivery-state update. Google does not always attach the media id and key
// to the echo yet; when it has not, the test reports that and stops rather
// than failing, and the read-only test above covers the download once the
// message is listed. Use AGENT_GM_LIVE_MEDIA_FILE/_MIME for a real image:
// the phone answers FAILURE_3 for the header-only tinyJPEG.
func TestLiveMediaSendEchoAndDownload(t *testing.T) {
	requireGate(t)
	if os.Getenv("AGENT_GM_LIVE_SEND") != "1" {
		t.Skip("this test sends a real message: set AGENT_GM_LIVE_SEND=1 to allow it")
	}
	caption := os.Getenv("AGENT_GM_LIVE_MEDIA_CAPTION")
	if caption == "" {
		t.Skip("set AGENT_GM_LIVE_MEDIA_CAPTION")
	}
	numbers := approvedNumbers(t)
	env := openLive(t)
	ctx := context.Background()
	a := env.accounts[0]

	conv, err := findApprovedConversation(ctx, a, numbers.Direct)
	if err != nil {
		t.Fatalf("finding the approved conversation: %v", err)
	}
	refuseUnlessOnlyApproved(t, conv, numbers.Direct)

	payload := tinyJPEG()
	filename, mime := "agent-gm-gate.jpg", "image/jpeg"
	if f := os.Getenv("AGENT_GM_LIVE_MEDIA_FILE"); f != "" {
		var err error
		payload, err = os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		filename = filepath.Base(f)
		mime = os.Getenv("AGENT_GM_LIVE_MEDIA_MIME")
		if mime == "" {
			t.Fatal("set AGENT_GM_LIVE_MEDIA_MIME with AGENT_GM_LIVE_MEDIA_FILE")
		}
	}
	ref, err := a.Backend.Upload(ctx, payload, filename, mime)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	t.Logf("uploaded: format=%s size=%d mime=%s keylen=%d", ref.Format, ref.SizeBytes, ref.MimeType, len(ref.DecryptionKey))

	// Read back what we just uploaded, before it is attached to anything.
	back, err := a.Backend.Download(ctx, ref.MediaID, ref.DecryptionKey)
	if err != nil {
		t.Fatalf("Download of the fresh upload: %v", err)
	}
	if string(back) != string(payload) {
		t.Errorf("fresh upload round trip differs: got %d bytes, sent %d", len(back), len(payload))
	} else {
		t.Logf("fresh upload round trip: %d bytes identical", len(back))
	}

	tmpID := gm.GenerateTmpID()
	res, err := a.Backend.SendMedia(ctx, gm.SendMediaRequest{
		ConversationID: conv.SourceID,
		ParticipantID:  conv.DefaultOutgoingID,
		Media:          ref,
		Caption:        caption,
		TmpID:          tmpID,
		SIMPayload:     conv.SIMPayload,
	})
	if err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	if res.Status != gm.SendStatusSuccess {
		t.Fatalf("send status = %s, want SUCCESS", res.Status)
	}
	echo := waitForEcho(t, ctx, a, tmpID, 60*time.Second)
	t.Logf("echo: %s state=%s attachments=%d text=%q", echo.SourceID, echo.DeliveryState, len(echo.Attachments), echo.Text)

	states := map[gm.DeliveryState]bool{echo.DeliveryState: true}
	latest := echo
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) && len(states) < 2 {
		select {
		case ev := <-a.Backend.Events():
			a.Apply(ctx, ev)
			if m, ok := ev.(*gm.EventMessage); ok && m.Message.TmpID == tmpID {
				states[m.Message.DeliveryState] = true
				latest = m.Message
			}
		case <-time.After(time.Second):
		}
	}
	t.Logf("delivery states observed: %v", states)
	if len(states) < 2 {
		t.Errorf("no delivery-status update arrived; saw only %v", states)
	}
	if len(latest.Attachments) == 0 {
		t.Fatalf("the echo carries no attachment")
	}
	att := latest.Attachments[0]
	if att.MediaID == "" || len(att.DecryptionKey) == 0 {
		t.Logf("the echoed attachment carries no media id/key yet; download it later through TestLiveMediaDownloadApprovedConversation")
		return
	}
	got, err := a.Backend.Download(ctx, att.MediaID, att.DecryptionKey)
	if err != nil {
		t.Fatalf("Download of the echoed attachment: %v", err)
	}
	t.Logf("echoed attachment: mime=%s declared=%d downloaded=%d identical_to_upload=%v", att.MimeType, att.SizeBytes, len(got), string(got) == string(payload))
	if len(got) == 0 {
		t.Fatal("downloaded 0 bytes")
	}
}
