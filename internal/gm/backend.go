package gm

import "context"

// Backend is the whole of Agent GM's dependency on Google Messages
// (spec section 2.3). There are exactly two implementations and only ever
// two: the libgm adapter in this package, and the fake in internal/gm/fake.
// A second production implementation is forbidden by non-goal N3 -- this
// interface exists for testability, not for extensibility (D10).
//
// One instance is one account.
type Backend interface {
	// lifecycle
	Connect(ctx context.Context) error
	Disconnect()
	IsConnected() bool
	IsLoggedIn() bool
	SessionID() string // libgm CurrentSessionID; drives the resync check

	// health -- everything GET /v1/health reports about Google
	FetchConfig(ctx context.Context) (ConfigInfo, error)
	CompiledConfigVersion() ConfigVersion
	IsDefaultSMSApp(ctx context.Context) (bool, error)

	// pairing
	StartGooglePairing(ctx context.Context, cookies map[string]string, deviceIndex int, emoji func(string)) (PairedDevice, error)
	RefreshGoogleCookies(ctx context.Context, cookies map[string]string) error

	// reads
	ListConversations(ctx context.Context, folder Folder, count int) ([]Conversation, error)
	GetConversation(ctx context.Context, convID string) (*Conversation, error)
	GetConversationType(ctx context.Context, convID string) (ConversationType, error)
	ListMessages(ctx context.Context, convID string, count int, cursor *Cursor) ([]Message, *Cursor, error)
	ListContacts(ctx context.Context) ([]Contact, error)
	ListTopContacts(ctx context.Context) ([]Contact, error)
	ContactAvatars(ctx context.Context, ids []string) (map[string][]byte, error)
	DownloadAvatar(ctx context.Context, url string) ([]byte, error)

	// writes
	ResolveConversation(ctx context.Context, numbers []string, groupName string) (ResolveResult, error)
	SendText(ctx context.Context, req SendTextRequest) (SendResult, error)
	SendMedia(ctx context.Context, req SendMediaRequest) (SendResult, error)
	React(ctx context.Context, msgID string, emoji string, action ReactionAction) error
	DeleteMessage(ctx context.Context, msgID string) error
	MarkRead(ctx context.Context, convID, msgID string) error
	SetTyping(ctx context.Context, convID string) error
	UpdateConversation(ctx context.Context, convID string, ch ConversationChange) error
	DeleteConversation(ctx context.Context, convID, phone string) error

	// media
	Upload(ctx context.Context, data []byte, filename, mime string) (MediaRef, error)
	Download(ctx context.Context, mediaID string, key []byte) ([]byte, error)
	RequestFullSizeImage(ctx context.Context, msgID, actionMsgID string) error

	// events
	Events() <-chan Event
}

// SessionPersister is the part of a Backend that Agent GM stores. The
// session is libgm.AuthData marshalled to JSON (spec section 3.3); the
// adapter exposes it as opaque bytes so that nothing above internal/gm has to
// know the shape.
type SessionPersister interface {
	// MarshalSession returns the current session bytes.
	MarshalSession() ([]byte, error)
	// LoadSession replaces the in-memory session.
	LoadSession(data []byte) error
	// SessionDirty reports whether the in-memory session differs from what
	// was last marshalled. Cookies mutate in place with no event, so the
	// 5-minute persistence timer of spec section 3.3 needs this.
	SessionDirty() bool
	// AccountAddress is AuthData.Mobile.SourceID, lowercased.
	AccountAddress() string
	// Shred zeroes the in-memory session, so the Google account cookies are
	// gone from the process (spec section 4.7).
	Shred()
}

// EventBufferSize is the capacity of the channel Events() returns. The libgm
// callback must not block -- libgm calls it synchronously from the long-poll
// loop -- so the adapter does a non-blocking send onto a channel of this
// capacity and increments dropped_events on overflow (spec section 2.3).
const EventBufferSize = 1024

// DroppedEventCounter is implemented by backends that can report how many
// events overflowed the channel, for GET /v1/health.
type DroppedEventCounter interface {
	DroppedEvents() uint64
	UnknownEvents() uint64
}
