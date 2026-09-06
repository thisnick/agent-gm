package store

import (
	"strings"

	"github.com/google/uuid"
)

// IDNamespace is the one frozen namespace UUID every derived ID hangs from.
// It is declared exactly once and never changed (spec section 4.1). Its value
// is UUIDv5(NameSpaceURL, "https://github.com/thisnick/agent-gm/ids/v1"),
// recorded here as a literal so that nothing about the derivation can drift.
var IDNamespace = uuid.MustParse("5ec9ab28-5363-57f8-b543-27672479b604")

// Public IDs are opaque, URL-safe, stable strings with a typed prefix. A
// caller never sees a raw Google conversation ID, message ID or participant
// ID on a public surface.
const (
	PrefixAccount      = "acct_"
	PrefixConversation = "conv_"
	PrefixMessage      = "msg_"
	PrefixAttachment   = "att_"
	PrefixReaction     = "react_"
	PrefixContact      = "contact_"
	PrefixParticipant  = "part_"
	PrefixOperation    = "op_"
)

// v5 derives a UUIDv5 from the frozen namespace and the joined components.
// Components are joined with a NUL byte, which cannot appear in a Google ID,
// so no two different component lists can produce the same input.
func v5(components ...string) uuid.UUID {
	var buf []byte
	for i, c := range components {
		if i > 0 {
			buf = append(buf, 0)
		}
		buf = append(buf, c...)
	}
	return uuid.NewSHA1(IDNamespace, buf)
}

// AccountID is UUIDv5(ns, "account", the lowercased AuthData.Mobile.SourceID).
//
// The address itself is therefore never an ID and never appears in a URL, and
// re-pairing the same Google account -- even onto a different phone -- reuses
// the same acct_ ID (D6, D28).
func AccountID(address string) string {
	// The address is lowercased here as well as at the gm boundary, so the
	// same Google account written two ways can never become two accounts.
	return PrefixAccount + v5("account", strings.ToLower(address)).String()
}

// ConversationID is UUIDv5(ns, account_id, "conversation", Google
// conversation ID). account_id is in the derivation, so conversation IDs are
// globally unique across accounts and a route never needs both.
func ConversationID(accountID, sourceID string) string {
	return PrefixConversation + v5(accountID, "conversation", sourceID).String()
}

// MessageID is UUIDv5(ns, account_id, "message", Google conversation ID,
// Google message ID).
func MessageID(accountID, convSourceID, msgSourceID string) string {
	return PrefixMessage + v5(accountID, "message", convSourceID, msgSourceID).String()
}

// ContactID is UUIDv5(ns, account_id, "contact", Google participant ID).
func ContactID(accountID, sourceID string) string {
	return PrefixContact + v5(accountID, "contact", sourceID).String()
}

// ParticipantID is UUIDv5(conversation ID, Google participant ID). It inherits
// the account transitively through its parent.
func ParticipantID(conversationID, sourceID string) string {
	return PrefixParticipant + v5(conversationID, sourceID).String()
}

// AttachmentID is UUIDv5(message ID, part index, Google media ID).
func AttachmentID(messageID, partIndex, mediaID string) string {
	return PrefixAttachment + v5(messageID, partIndex, mediaID).String()
}

// ReactionID is UUIDv5(message ID, participant ID, canonical
// EmojiType.Unicode()). For CUSTOM it is the caller's unicode after NFC
// normalisation; for a type with no unicode it is the EmojiType name. Every
// inbound emoji is canonicalised before it reaches here (spec section 3.7).
func ReactionID(messageID, participantID, canonicalEmojiOrTypeName string) string {
	return PrefixReaction + v5(messageID, participantID, canonicalEmojiOrTypeName).String()
}

// OperationID is UUIDv7: Agent GM creates it, so it is not derived.
func OperationID() string {
	return PrefixOperation + uuid.Must(uuid.NewV7()).String()
}

// HasPrefix reports whether an ID carries the expected typed prefix. An ID
// with the wrong prefix is invalid_request naming the parameter and the
// expected prefix -- never not_found (spec section 4.1).
func HasPrefix(id, prefix string) bool {
	if len(id) <= len(prefix) || id[:len(prefix)] != prefix {
		return false
	}
	_, err := uuid.Parse(id[len(prefix):])
	return err == nil
}
