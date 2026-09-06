package gm

import (
	"fmt"
	"time"
)

// Everything in this file is Agent GM's own vocabulary. No gmproto type
// crosses this boundary: spec section 2.3 requires that nothing above
// internal/gm imports libgm or gmproto.

// ConfigVersion is Google Messages for web's client version, as carried by
// util.ConfigMessage (compiled in) and by the live Config document.
type ConfigVersion struct {
	Year  int32 `json:"year"`
	Month int32 `json:"month"`
	Day   int32 `json:"day"`
	V1    int32 `json:"v1"`
	V2    int32 `json:"v2"`
}

func (c ConfigVersion) String() string {
	return fmt.Sprintf("%d.%d.%d.%d.%d", c.Year, c.Month, c.Day, c.V1, c.V2)
}

// SameDate reports whether two versions agree in year, month and day. Spec
// section 3.7: the config_version_stale detection rule is the date diff alone.
func (c ConfigVersion) SameDate(other ConfigVersion) bool {
	return c.Year == other.Year && c.Month == other.Month && c.Day == other.Day
}

// ConfigInfo is what a live FetchConfig round trip tells us.
type ConfigInfo struct {
	Live ConfigVersion
	// DeviceEmail is Config.DeviceInfo.Email. Upstream compares it against
	// AuthData.Mobile.SourceID to decide whether a re-authentication is the
	// same account (connector/login.go:267-270).
	DeviceEmail string
	// SessionID is Config.DeviceInfo.DeviceID, which FetchConfig parses into
	// AuthData.SessionID (client.go:389).
	DeviceID string
}

// Folder mirrors gmproto.ListConversationsRequest_Folder: UNKNOWN=0, INBOX=1,
// ARCHIVE=2, SPAM_BLOCKED=5 (spec section 3.1).
type Folder int32

const (
	FolderUnknown     Folder = 0
	FolderInbox       Folder = 1
	FolderArchive     Folder = 2
	FolderSpamBlocked Folder = 5
)

func (f Folder) String() string {
	switch f {
	case FolderInbox:
		return "active"
	case FolderArchive:
		return "archived"
	case FolderSpamBlocked:
		return "spam_blocked"
	default:
		return "unknown"
	}
}

// ConversationType is the derived public value of spec section 4.6:
// sms_mms for ConversationType_SMS(1), rcs for RCS(2), unknown for 0.
type ConversationType string

const (
	ConversationTypeUnknown ConversationType = "unknown"
	ConversationTypeSMSMMS  ConversationType = "sms_mms"
	ConversationTypeRCS     ConversationType = "rcs"
)

// SendMode is gmproto.ConversationSendMode. It is internal and is never
// served on a public surface (spec section 4.6); it exists so that
// capabilities.force_rcs can be derived.
type SendMode string

const (
	SendModeAuto     SendMode = "SEND_MODE_AUTO"
	SendModeXMS      SendMode = "SEND_MODE_XMS"
	SendModeXMSLatch SendMode = "SEND_MODE_XMS_LATCH"
)

// Participant is one person in a conversation.
type Participant struct {
	SourceID        string
	ContactID       string
	DisplayName     string
	FirstName       string
	PhoneE164       string
	FormattedNumber string
	IdentifierType  string
	IsMe            bool
	IsVisible       bool
}

// Conversation is one Google Messages thread.
type Conversation struct {
	SourceID          string
	Name              string
	IsGroup           bool
	Type              ConversationType
	SendModeRaw       SendMode
	Folder            Folder
	Unread            bool
	Pinned            bool
	ReadOnly          bool
	// Deleted is Google's own delete-for-me: ConversationStatus DELETED or
	// TRASH_FOLDER. It is the only delete Agent GM has (non-goal N6).
	Deleted bool
	DefaultOutgoingID string
	LatestMessageID   string
	LastActivity      time.Time
	GroupAvatarURL    string
	// SIMPayload is opaque: it is carried back verbatim on sends and typing
	// notifications and is never interpreted.
	SIMPayload   []byte
	Participants []Participant
}

// ForceRCSEligible is spec section 4.6: true exactly when the conversation is
// RCS and its send mode is SEND_MODE_AUTO.
func (c Conversation) ForceRCSEligible() bool {
	return c.Type == ConversationTypeRCS && c.SendModeRaw == SendModeAuto
}

// MessageKind separates ordinary messages from Google's own in-thread system
// events (MessageStatusType 200-279). "tombstone" is Matrix vocabulary and
// does not appear on any surface (spec section 1.3 N6).
type MessageKind string

const (
	MessageKindMessage MessageKind = "message"
	MessageKindSystem  MessageKind = "system"
)

// Direction is which way a message went.
type Direction string

const (
	DirectionIncoming Direction = "incoming"
	DirectionOutgoing Direction = "outgoing"
)

// Attachment is one media part of a message.
type Attachment struct {
	PartIndex        int
	ActionMessageID  string
	MediaID          string
	ThumbnailMediaID string
	DecryptionKey    []byte
	Filename         string
	MimeType         string
	MediaFormat      string
	SizeBytes        int64
	Width            int64
	Height           int64
}

// Reaction is one reaction entry on a message. Emoji is nil when the
// EmojiType has no unicode of its own (EMOTIFY, and any unrecognised type):
// spec section 3.7 requires {"emoji": null, "type": ...} rather than the
// upstream behaviour of silently skipping it.
type Reaction struct {
	Emoji          *string
	Type           EmojiType
	ParticipantIDs []string
}

// Message is one message in a conversation.
type Message struct {
	SourceID       string
	ConversationID string
	ParticipantID  string
	Text           string
	Subject        string
	// Timestamp is already converted from Google's microseconds. The
	// conversion happens once, at this boundary (spec section 3.7).
	Timestamp        time.Time
	TmpID            string
	StatusRaw        int32
	Kind             MessageKind
	DeliveryState    DeliveryState
	ReplyToMessageID string
	IsOld            bool
	Attachments      []Attachment
	Reactions        []Reaction
}

// Direction is derived from the raw status band: outgoing statuses run 1-27,
// incoming 100-118. System events and MESSAGE_DELETED(300) carry no direction
// of their own, so they follow the band they are nearest to.
func (m Message) Direction() Direction {
	if m.StatusRaw >= 100 && m.StatusRaw <= 118 {
		return DirectionIncoming
	}
	return DirectionOutgoing
}

// Contact is somebody in the phone's contact list.
type Contact struct {
	SourceID        string
	ContactID       string
	DisplayName     string
	PhoneE164       string
	FormattedNumber string
	IsTop           bool
}

// Cursor is gmproto.Cursor: {lastItemID, lastItemTimestamp}. The timestamp is
// Google's own microseconds and is carried back verbatim.
type Cursor struct {
	LastItemID          string
	LastItemTimestampUS int64
}

// PairedDevice is what a completed gaia pairing produced.
type PairedDevice struct {
	// PhoneID is FinishGaiaPairing's "<mobile sourceID>/<destRegDevice int>".
	PhoneID string
	// AccountAddress is AuthData.Mobile.SourceID, lowercased: the Google
	// account address, and the only stable account identifier at this pin
	// (spec section 3.2, D28).
	AccountAddress string
	// DestRegUUID and DeviceLastSeen record which phone was chosen, so
	// "which phone did we pair?" is answerable afterwards (spec section 3.2).
	DestRegUUID    string
	DeviceLastSeen time.Time
	DeviceCount    int
	DeviceIndex    int
}

// SendStatus mirrors gmproto.SendMessageResponse_Status.
type SendStatus int32

const (
	SendStatusUnknown  SendStatus = 0
	SendStatusSuccess  SendStatus = 1
	SendStatusFailure2 SendStatus = 2
	SendStatusFailure3 SendStatus = 3
	SendStatusFailure4 SendStatus = 4
)

// IsTransient mirrors connector.isTransientSendFailure: only FAILURE_2 and
// FAILURE_3 are retried, on SendRetryBackoff.
func (s SendStatus) IsTransient() bool {
	return s == SendStatusFailure2 || s == SendStatusFailure3
}

func (s SendStatus) String() string {
	switch s {
	case SendStatusUnknown:
		return "UNKNOWN"
	case SendStatusSuccess:
		return "SUCCESS"
	case SendStatusFailure2:
		return "FAILURE_2"
	case SendStatusFailure3:
		return "FAILURE_3"
	case SendStatusFailure4:
		return "FAILURE_4"
	default:
		return fmt.Sprintf("UNRECOGNISED(%d)", int32(s))
	}
}

// SendRetryBackoff is connector.sendRetryBackoff at the pin.
var SendRetryBackoff = []time.Duration{3 * time.Second, 8 * time.Second, 20 * time.Second}

// SendTextRequest is one text send attempt.
type SendTextRequest struct {
	ConversationID string
	ParticipantID  string
	Text           string
	// TmpID is a bare UUID minted per send attempt (D22). It is set on all
	// three of SendMessageRequest.TmpID, MessagePayload.TmpID and
	// MessagePayload.TmpID2.
	TmpID            string
	ReplyToMessageID string
	ForceRCS         bool
	SIMPayload       []byte
}

// SendMediaRequest is a media send. The caption, when present, is appended as
// a second MessageInfo entry carrying MessageContent, exactly as upstream does.
type SendMediaRequest struct {
	ConversationID   string
	ParticipantID    string
	Media            MediaRef
	Caption          string
	TmpID            string
	ReplyToMessageID string
	ForceRCS         bool
	SIMPayload       []byte
}

// SendResult is the phone's own answer to a send.
type SendResult struct {
	Status SendStatus
	// GoogleAccountSwitch is non-empty when the phone switched Google
	// accounts underneath us (spec section 3.7).
	GoogleAccountSwitch string
}

// ResolveStatus mirrors gmproto.GetOrCreateConversationResponse_Status, which
// declares exactly UNKNOWN=0, SUCCESS=1, CREATE_RCS=3 at this pin. Any other
// value is unnamed and Agent GM never invents a name for one.
type ResolveStatus int32

const (
	ResolveStatusUnknown   ResolveStatus = 0
	ResolveStatusSuccess   ResolveStatus = 1
	ResolveStatusCreateRCS ResolveStatus = 3
)

// ResolveResult carries the conversation and the raw status, so core can
// distinguish SUCCESS, CREATE_RCS and an unnamed value without reaching past
// the Backend interface (spec section 2.3).
type ResolveResult struct {
	Conversation *Conversation
	Status       ResolveStatus
}

// MediaRef is the result of an upload: gmproto.MediaContent, in Agent GM's
// vocabulary. SizeBytes is the plaintext size.
type MediaRef struct {
	MediaID       string
	Format        string
	Name          string
	SizeBytes     int64
	DecryptionKey []byte
	MimeType      string
}

// ReactionAction mirrors gmproto.SendReactionRequest_Action.
type ReactionAction int32

const (
	ReactionActionUnspecified ReactionAction = 0
	ReactionActionAdd         ReactionAction = 1
	ReactionActionRemove      ReactionAction = 2
	ReactionActionSwitch      ReactionAction = 3
)

// ConversationChange carries the optional folder, pinned and unread fields of
// PATCH /v1/conversations/{id} (spec section 2.3).
type ConversationChange struct {
	Folder *Folder
	Pinned *bool
	Unread *bool
}
