package gm

import (
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// This file is the only place gmproto types are translated into Agent GM's
// vocabulary. Google timestamps are microseconds and are converted here,
// once, at the gm boundary (spec section 3.7).

func microsToTime(us int64) time.Time {
	if us == 0 {
		return time.Time{}
	}
	return time.UnixMicro(us).UTC()
}

func convertConfigVersion(v *gmproto.ConfigVersion) ConfigVersion {
	if v == nil {
		return ConfigVersion{}
	}
	return ConfigVersion{Year: v.Year, Month: v.Month, Day: v.Day, V1: v.V1, V2: v.V2}
}

func convertConversationType(t gmproto.ConversationType) ConversationType {
	switch t {
	case gmproto.ConversationType_SMS:
		return ConversationTypeSMSMMS
	case gmproto.ConversationType_RCS:
		return ConversationTypeRCS
	default:
		return ConversationTypeUnknown
	}
}

func convertSendMode(m gmproto.ConversationSendMode) SendMode {
	switch m {
	case gmproto.ConversationSendMode_SEND_MODE_XMS:
		return SendModeXMS
	case gmproto.ConversationSendMode_SEND_MODE_XMS_LATCH:
		return SendModeXMSLatch
	default:
		return SendModeAuto
	}
}

func folderForStatus(s gmproto.ConversationStatus) Folder {
	switch s {
	case gmproto.ConversationStatus_ARCHIVED, gmproto.ConversationStatus_KEEP_ARCHIVED:
		return FolderArchive
	case gmproto.ConversationStatus_SPAM_FOLDER, gmproto.ConversationStatus_BLOCKED_FOLDER:
		return FolderSpamBlocked
	default:
		return FolderInbox
	}
}

func deletedForStatus(s gmproto.ConversationStatus) bool {
	return s == gmproto.ConversationStatus_DELETED || s == gmproto.ConversationStatus_TRASH_FOLDER
}

func convertParticipant(p *gmproto.Participant) Participant {
	if p == nil {
		return Participant{}
	}
	id := p.GetID()
	return Participant{
		SourceID:        id.GetParticipantID(),
		ContactID:       p.GetContactID(),
		DisplayName:     p.GetFullName(),
		FirstName:       p.GetFirstName(),
		PhoneE164:       id.GetNumber(),
		FormattedNumber: p.GetFormattedNumber(),
		IdentifierType:  id.GetType().String(),
		IsMe:            p.GetIsMe(),
		IsVisible:       p.GetIsVisible(),
	}
}

// ConvertConversation turns a gmproto.Conversation into Agent GM's own.
// Conversation.LatestMessage is deliberately dropped: it is large and
// duplicative, and upstream itself clones and nils it before logging
// (connector/startchat.go:227-230). It goes through the message ingest path
// or nowhere (spec section 3.7).
func ConvertConversation(c *gmproto.Conversation) Conversation {
	if c == nil {
		return Conversation{}
	}
	out := Conversation{
		SourceID:          c.GetConversationID(),
		Name:              c.GetName(),
		IsGroup:           c.GetIsGroupChat(),
		Type:              convertConversationType(c.GetType()),
		SendModeRaw:       convertSendMode(c.GetSendMode()),
		Folder:            folderForStatus(c.GetStatus()),
		Deleted:           deletedForStatus(c.GetStatus()),
		Unread:            c.GetUnread(),
		Pinned:            c.GetPinned(),
		ReadOnly:          c.GetReadOnly(),
		DefaultOutgoingID: c.GetDefaultOutgoingID(),
		LatestMessageID:   c.GetLatestMessageID(),
		LastActivity:      microsToTime(c.GetLastMessageTimestamp()),
		GroupAvatarURL:    c.GetGroupAvatarURL(),
	}
	if sp := c.GetSimCard().GetSIMData().GetSIMPayload(); sp != nil {
		if raw, err := proto.Marshal(sp); err == nil {
			out.SIMPayload = raw
		}
	}
	for _, p := range c.GetParticipants() {
		out.Participants = append(out.Participants, convertParticipant(p))
	}
	return out
}

func convertReactions(entries []*gmproto.ReactionEntry) []Reaction {
	if len(entries) == 0 {
		return nil
	}
	out := make([]Reaction, 0, len(entries))
	for _, e := range entries {
		data := e.GetData()
		t, _ := EmojiTypeForRaw(int32(data.GetType()))
		out = append(out, Reaction{
			Emoji:          CanonicalEmoji(t, data.GetUnicode()),
			Type:           t,
			ParticipantIDs: append([]string(nil), e.GetParticipantIDs()...),
		})
	}
	return out
}

// ConvertMessage turns a gmproto.Message into Agent GM's own. Text parts are
// joined with a newline in part order, exactly as the content hash of spec
// section 5.3 expects.
func ConvertMessage(m *gmproto.Message) Message {
	if m == nil {
		return Message{}
	}
	raw := int32(m.GetMessageStatus().GetStatus())
	state, _ := DeliveryStateFor(raw)
	out := Message{
		SourceID:         m.GetMessageID(),
		ConversationID:   m.GetConversationID(),
		ParticipantID:    m.GetParticipantID(),
		Subject:          m.GetSubject(),
		Timestamp:        microsToTime(m.GetTimestamp()),
		TmpID:            m.GetTmpID(),
		StatusRaw:        raw,
		Kind:             KindForStatus(raw),
		DeliveryState:    state,
		ReplyToMessageID: m.GetReplyMessage().GetMessageID(),
		Reactions:        convertReactions(m.GetReactions()),
	}
	var text string
	for i, info := range m.GetMessageInfo() {
		switch data := info.GetData().(type) {
		case *gmproto.MessageInfo_MessageContent:
			if text != "" {
				text += "\n"
			}
			text += data.MessageContent.GetContent()
		case *gmproto.MessageInfo_MediaContent:
			mc := data.MediaContent
			out.Attachments = append(out.Attachments, Attachment{
				PartIndex:        i,
				ActionMessageID:  info.GetActionMessageID(),
				MediaID:          mc.GetMediaID(),
				ThumbnailMediaID: mc.GetThumbnailMediaID(),
				DecryptionKey:    append([]byte(nil), mc.GetDecryptionKey()...),
				Filename:         mc.GetMediaName(),
				MimeType:         mc.GetMimeType(),
				MediaFormat:      mc.GetFormat().String(),
				SizeBytes:        mc.GetSize(),
				Width:            mc.GetDimensions().GetWidth(),
				Height:           mc.GetDimensions().GetHeight(),
			})
		}
	}
	out.Text = text
	return out
}

func convertContact(c *gmproto.Contact, top bool) Contact {
	if c == nil {
		return Contact{}
	}
	return Contact{
		SourceID:        c.GetParticipantID(),
		ContactID:       c.GetContactID(),
		DisplayName:     c.GetName(),
		PhoneE164:       c.GetNumber().GetNumber(),
		FormattedNumber: c.GetNumber().GetFormattedNumber(),
		IsTop:           top,
	}
}

func convertCursor(c *gmproto.Cursor) *Cursor {
	if c == nil || (c.GetLastItemID() == "" && c.GetLastItemTimestamp() == 0) {
		return nil
	}
	return &Cursor{LastItemID: c.GetLastItemID(), LastItemTimestampUS: c.GetLastItemTimestamp()}
}

func toProtoCursor(c *Cursor) *gmproto.Cursor {
	if c == nil {
		return nil
	}
	return &gmproto.Cursor{LastItemID: c.LastItemID, LastItemTimestamp: c.LastItemTimestampUS}
}

func toProtoSIMPayload(raw []byte) *gmproto.SIMPayload {
	if len(raw) == 0 {
		return nil
	}
	var sp gmproto.SIMPayload
	if err := proto.Unmarshal(raw, &sp); err != nil {
		return nil
	}
	return &sp
}
