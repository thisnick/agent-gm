package gm_test

import (
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/thisnick/agent-gm/internal/gm"
)

// Live-gate finding 2: every outgoing media send produced TWO attachment
// rows for one file.
//
// `att_` is UUIDv5 over (message ID, part index, media ID), so the index
// decides identity, and it was the position among ALL MessageInfo entries.
// Google does not promise an order: an echo carrying [media, text] and a
// later echo carrying [text, media] -- the same message, the same file --
// produced two different att_ IDs and two rows.
//
// Counting media parts among themselves makes the index a property of the
// attachment rather than of how Google ordered that particular echo.
//
// Plant: index by the position among all parts again and this fails at "the
// media part's index moved". Planted 2026-09-07.
func TestMediaPartIndexIsStableAcrossPartOrdering(t *testing.T) {
	media := &gmproto.MessageInfo{
		Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{
			MediaID:   "media-1",
			MediaName: "photo.jpg",
			MimeType:  "image/jpeg",
		}},
	}
	text := &gmproto.MessageInfo{
		Data: &gmproto.MessageInfo_MessageContent{
			MessageContent: &gmproto.MessageContent{Content: "caption"},
		},
	}

	mediaFirst := gm.ConvertMessage(&gmproto.Message{
		MessageID: "m1", ConversationID: "c1",
		MessageInfo: []*gmproto.MessageInfo{media, text},
	})
	textFirst := gm.ConvertMessage(&gmproto.Message{
		MessageID: "m1", ConversationID: "c1",
		MessageInfo: []*gmproto.MessageInfo{text, media},
	})

	if len(mediaFirst.Attachments) != 1 || len(textFirst.Attachments) != 1 {
		t.Fatalf("attachments: %d and %d, want one each",
			len(mediaFirst.Attachments), len(textFirst.Attachments))
	}
	if mediaFirst.Attachments[0].PartIndex != textFirst.Attachments[0].PartIndex {
		t.Errorf("the media part's index moved with the part ordering: %d then %d; "+
			"att_ derives from it, so the same file becomes two attachment rows",
			mediaFirst.Attachments[0].PartIndex, textFirst.Attachments[0].PartIndex)
	}
	if mediaFirst.Text != "caption" || textFirst.Text != "caption" {
		t.Errorf("the caption was lost: %q / %q", mediaFirst.Text, textFirst.Text)
	}

	// Two genuinely different media parts still get different indices.
	second := &gmproto.MessageInfo{
		Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{
			MediaID: "media-2", MimeType: "image/jpeg",
		}},
	}
	two := gm.ConvertMessage(&gmproto.Message{
		MessageID: "m1", ConversationID: "c1",
		MessageInfo: []*gmproto.MessageInfo{media, text, second},
	})
	if len(two.Attachments) != 2 {
		t.Fatalf("attachments = %d, want 2", len(two.Attachments))
	}
	if two.Attachments[0].PartIndex == two.Attachments[1].PartIndex {
		t.Error("two different media parts share a part index")
	}
}
