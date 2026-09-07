// Package fake is a deterministic in-memory Google Messages.
//
// One instance is one account (spec section 13.1). A test wanting two
// accounts constructs two fakes and registers both with internal/accounts,
// exactly as production does; the fake never models several accounts
// internally, because if it did, the multi-account paths above it would be
// tested against a shape production does not have.
//
// It is not a mock with canned returns. It is a small simulator: it accepts
// sends, mints message IDs, emits the same event sequence a real phone would,
// and can be scripted to fail with any error in spec section 3.5.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/thisnick/agent-gm/internal/clock"
	"github.com/thisnick/agent-gm/internal/gm"
)

// Device is one primary-looking device on the account, for the several-device
// case of spec section 3.2.
type Device struct {
	RegUUID  string
	LastSeen time.Time
}

// Backend is the fake. It implements gm.Backend in full.
type Backend struct {
	mu    sync.Mutex
	clock clock.Clock

	// address is AuthData.Mobile.SourceID, settable so a test can pair two
	// distinct accounts, or re-pair the same one.
	address string

	loggedIn  bool
	connected bool
	sessionID string
	phoneID   string
	cookies   map[string]string
	devices   []Device

	compiledConfig gm.ConfigVersion
	liveConfig     gm.ConfigVersion
	deviceEmail    string
	defaultSMSApp  bool

	conversations map[string]*gm.Conversation
	messages      map[string][]*gm.Message // conversation source ID -> messages
	contacts      []gm.Contact
	topContacts   []gm.Contact
	avatars       map[string][]byte
	media         map[string][]byte

	// pairing script
	pairEmoji string
	pairError error
	// refreshAddress lets a test script a wrong-account cookie refresh.
	refreshAddress string

	// call script
	nextErrors      []error
	sendStatuses    []gm.SendStatus
	resolveStatuses []gm.ResolveStatus

	// event plumbing
	events     chan gm.Event
	dropEvents bool
	dropped    atomic.Uint64
	unknown    atomic.Uint64

	calls map[string]int

	// outgoing messages still walking the delivery ladder
	pending []*pendingSend

	session []byte
	dirty   bool
}

type pendingSend struct {
	convID string
	msgID  string
	rung   int
}

var _ gm.Backend = (*Backend)(nil)
var _ gm.SessionPersister = (*Backend)(nil)
var _ gm.DroppedEventCounter = (*Backend)(nil)

// Option configures a fake.
type Option func(*Backend)

// WithClock gives this fake its own clock. Two fakes may share one clock or
// hold two; a test that advances one account's clock while the other's stands
// still needs two.
func WithClock(c clock.Clock) Option { return func(b *Backend) { b.clock = c } }

// WithDevices sets the primary-looking devices the account has, for the
// several-device selection of spec section 3.2.
func WithDevices(d ...Device) Option { return func(b *Backend) { b.devices = d } }

// New builds a fake for one account. address is the Google account address
// that AuthData.Mobile.SourceID would carry; it is lowercased exactly as
// signInGaiaGetToken does.
func New(address string, opts ...Option) *Backend {
	b := &Backend{
		clock:          clock.NewFake(),
		address:        strings.ToLower(address),
		compiledConfig: gm.ConfigVersion{Year: 2026, Month: 9, Day: 2, V1: 4, V2: 6},
		liveConfig:     gm.ConfigVersion{Year: 2026, Month: 9, Day: 2, V1: 4, V2: 6},
		deviceEmail:    strings.ToLower(address),
		defaultSMSApp:  true,
		conversations:  map[string]*gm.Conversation{},
		messages:       map[string][]*gm.Message{},
		avatars:        map[string][]byte{},
		media:          map[string][]byte{},
		pairEmoji:      "\U0001F98B",
		events:         make(chan gm.Event, gm.EventBufferSize),
		calls:          map[string]int{},
		devices:        []Device{{RegUUID: "11111111-1111-4111-8111-111111111111"}},
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Clock returns this fake's clock.
func (b *Backend) Clock() clock.Clock { return b.clock }

// Address is the account address this fake pairs as.
func (b *Backend) Address() string { return b.address }

// SetAddress changes the account address, so a test can pair two distinct
// accounts or re-pair the same one.
func (b *Backend) SetAddress(a string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.address = strings.ToLower(a)
	b.deviceEmail = b.address
}

// CallCount reports how many times a Backend method was called, so a test can
// assert the backend was called exactly once (idempotency, spec section 13.2).
func (b *Backend) CallCount(method string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[method]
}

func (b *Backend) note(method string) error {
	b.calls[method]++
	if len(b.nextErrors) > 0 {
		err := b.nextErrors[0]
		b.nextErrors = b.nextErrors[1:]
		if err != nil {
			return gm.Classify(err)
		}
	}
	return nil
}

// --- scripting --------------------------------------------------------------

// ScriptErrors queues errors to be returned by the next calls, one per call.
// A nil entry means "this call succeeds". Every error in spec section 3.5 can
// be queued here.
func (b *Backend) ScriptErrors(errs ...error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextErrors = append(b.nextErrors, errs...)
}

// ScriptSendStatuses queues SendMessageResponse.Status values, one per send.
// FAILURE_2, FAILURE_2, SUCCESS exercises the retry backoff.
func (b *Backend) ScriptSendStatuses(st ...gm.SendStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sendStatuses = append(b.sendStatuses, st...)
}

// ScriptResolveStatuses queues GetOrCreateConversation statuses, one per
// ResolveConversation call. A value the proto has no name for is accepted so
// that google_undocumented_status is testable.
func (b *Backend) ScriptResolveStatuses(st ...gm.ResolveStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resolveStatuses = append(b.resolveStatuses, st...)
}

// ScriptPairError makes the next pairing fail with err, which is how each
// GaiaPairingErrorCode outcome is reproduced.
func (b *Backend) ScriptPairError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pairError = err
}

// SetPairEmoji sets the emoji handed to the callback.
func (b *Backend) SetPairEmoji(e string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pairEmoji = e
}

// ScriptRefreshAddress makes the next cookie refresh sign in as a different
// Google account, which must be refused with pairing_wrong_account.
func (b *Backend) ScriptRefreshAddress(a string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refreshAddress = strings.ToLower(a)
}

// SetLiveConfigVersion scripts a live ConfigVersion differing from the
// compiled one, so config_version_stale is testable with no phone.
func (b *Backend) SetLiveConfigVersion(v gm.ConfigVersion) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.liveConfig = v
}

// SetCompiledConfigVersion overrides the compiled version this fake reports.
func (b *Backend) SetCompiledConfigVersion(v gm.ConfigVersion) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.compiledConfig = v
}

// SetDefaultSMSApp scripts IsBugleDefault.
func (b *Backend) SetDefaultSMSApp(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.defaultSMSApp = v
}

// SetDropEvents makes every subsequent emit overflow, so the dropped_events
// counter is testable.
func (b *Backend) SetDropEvents(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropEvents = v
}

// --- seeding ----------------------------------------------------------------

// SeedConversation adds or replaces a conversation.
func (b *Backend) SeedConversation(c gm.Conversation) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := c
	b.conversations[c.SourceID] = &cp
}

// SeedMessage adds or replaces a message in its conversation.
func (b *Backend) SeedMessage(m gm.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.putMessageLocked(m)
}

func (b *Backend) putMessageLocked(m gm.Message) *gm.Message {
	list := b.messages[m.ConversationID]
	for i, existing := range list {
		if existing.SourceID == m.SourceID {
			cp := m
			list[i] = &cp
			return list[i]
		}
	}
	cp := m
	b.messages[m.ConversationID] = append(list, &cp)
	return &cp
}

// SeedContacts sets the contact list.
func (b *Backend) SeedContacts(c ...gm.Contact) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contacts = c
}

// SeedTopContacts sets the top-contacts list.
func (b *Backend) SeedTopContacts(c ...gm.Contact) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.topContacts = c
}

// SeedAvatar sets one participant avatar.
func (b *Backend) SeedAvatar(id string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.avatars[id] = data
}

// --- lifecycle --------------------------------------------------------------

func (b *Backend) Connect(ctx context.Context) error {
	b.mu.Lock()
	if err := b.note("Connect"); err != nil {
		b.mu.Unlock()
		return err
	}
	if !b.loggedIn {
		b.mu.Unlock()
		return gm.Classify(gm.ErrInvalidCredentials)
	}
	b.connected = true
	b.sessionID = uuid.NewString()
	convs := b.conversationsLocked(gm.FolderInbox, 0)
	sess := b.sessionID
	b.mu.Unlock()

	b.Emit(&gm.EventClientReady{SessionID: sess, Conversations: convs})
	return nil
}

func (b *Backend) Disconnect() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls["Disconnect"]++
	b.connected = false
}

func (b *Backend) IsConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls["IsConnected"]++
	return b.connected
}

func (b *Backend) IsLoggedIn() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls["IsLoggedIn"]++
	return b.loggedIn && len(b.cookies) > 0
}

func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls["SessionID"]++
	return b.sessionID
}

// SetSessionID changes the current session ID, so the BROWSER_ACTIVE resync
// comparison of spec section 3.4 is testable.
func (b *Backend) SetSessionID(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessionID = id
}

func (b *Backend) DroppedEvents() uint64 { return b.dropped.Load() }
func (b *Backend) UnknownEvents() uint64 { return b.unknown.Load() }

// --- health -----------------------------------------------------------------

func (b *Backend) FetchConfig(ctx context.Context) (gm.ConfigInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("FetchConfig"); err != nil {
		return gm.ConfigInfo{}, err
	}
	return gm.ConfigInfo{
		Live:        b.liveConfig,
		DeviceEmail: b.deviceEmail,
		DeviceID:    "00000000-0000-4000-8000-000000000001",
	}, nil
}

func (b *Backend) CompiledConfigVersion() gm.ConfigVersion {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.compiledConfig
}

func (b *Backend) IsDefaultSMSApp(ctx context.Context) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("IsDefaultSMSApp"); err != nil {
		return false, err
	}
	return b.defaultSMSApp, nil
}

// --- pairing ----------------------------------------------------------------

func (b *Backend) StartGooglePairing(ctx context.Context, cookies map[string]string, deviceIndex int, emoji func(string)) (gm.PairedDevice, error) {
	b.mu.Lock()
	if err := b.note("StartGooglePairing"); err != nil {
		b.mu.Unlock()
		return gm.PairedDevice{}, err
	}
	if len(cookies) == 0 {
		b.mu.Unlock()
		return gm.PairedDevice{}, gm.Classify(gm.ErrNoCookies)
	}
	if missing := gm.MissingRequiredCookies(cookies); len(missing) > 0 {
		e := gm.Classify(gm.ErrNoCookies)
		e.Details = map[string]any{"missing_cookies": missing}
		b.mu.Unlock()
		return gm.PairedDevice{}, e
	}
	if len(b.devices) == 0 {
		b.mu.Unlock()
		return gm.PairedDevice{}, gm.Classify(gm.ErrNoDevicesFound)
	}
	// Device selection is not "the single primary": newest-first, then
	// devices[switcher % len]. The library does not error on several.
	devices := append([]Device(nil), b.devices...)
	sort.SliceStable(devices, func(i, j int) bool { return devices[i].LastSeen.After(devices[j].LastSeen) })
	chosen := devices[((deviceIndex%len(devices))+len(devices))%len(devices)]
	e := b.pairEmoji
	pairErr := b.pairError
	b.pairError = nil
	b.mu.Unlock()

	if emoji != nil {
		emoji(e)
	}
	if pairErr != nil {
		return gm.PairedDevice{}, gm.Classify(pairErr)
	}

	b.mu.Lock()
	if b.address == "" {
		b.mu.Unlock()
		return gm.PairedDevice{}, gm.Classify(gm.ErrNoAccountAddress)
	}
	b.cookies = map[string]string{}
	for k, v := range cookies {
		b.cookies[k] = v
	}
	b.loggedIn = true
	b.dirty = true
	b.phoneID = fmt.Sprintf("%s/%d", b.address, deviceIndex)
	dev := gm.PairedDevice{
		PhoneID:        b.phoneID,
		AccountAddress: b.address,
		DestRegUUID:    chosen.RegUUID,
		DeviceLastSeen: chosen.LastSeen,
		DeviceCount:    len(devices),
		DeviceIndex:    deviceIndex,
	}
	phoneID := b.phoneID
	b.mu.Unlock()

	b.Emit(&gm.EventPairSuccessful{PhoneID: phoneID})
	return dev, nil
}

func (b *Backend) RefreshGoogleCookies(ctx context.Context, cookies map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("RefreshGoogleCookies"); err != nil {
		return err
	}
	if !b.loggedIn && b.phoneID == "" {
		return gm.Classify(gm.ErrRequestedEntityNotFound)
	}
	if missing := gm.MissingRequiredCookies(cookies); len(missing) > 0 {
		e := gm.Classify(gm.ErrNoCookies)
		e.Details = map[string]any{"missing_cookies": missing}
		return e
	}
	if b.refreshAddress != "" && b.refreshAddress != b.address {
		// Refused, and nothing changes: the old cookies stay in place.
		b.refreshAddress = ""
		return gm.Classify(gm.ErrWrongAccount)
	}
	b.refreshAddress = ""
	b.cookies = map[string]string{}
	for k, v := range cookies {
		b.cookies[k] = v
	}
	b.loggedIn = true
	b.connected = true
	b.dirty = true
	return nil
}

// --- reads ------------------------------------------------------------------

func (b *Backend) conversationsLocked(folder gm.Folder, count int) []gm.Conversation {
	out := make([]gm.Conversation, 0, len(b.conversations))
	for _, c := range b.conversations {
		if c.Folder != folder {
			continue
		}
		out = append(out, *c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastActivity.Equal(out[j].LastActivity) {
			return out[i].LastActivity.After(out[j].LastActivity)
		}
		return out[i].SourceID > out[j].SourceID
	})
	if count > 0 && len(out) > count {
		out = out[:count]
	}
	return out
}

func (b *Backend) ListConversations(ctx context.Context, folder gm.Folder, count int) ([]gm.Conversation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("ListConversations"); err != nil {
		return nil, err
	}
	return b.conversationsLocked(folder, count), nil
}

func (b *Backend) GetConversation(ctx context.Context, convID string) (*gm.Conversation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("GetConversation"); err != nil {
		return nil, err
	}
	c, ok := b.conversations[convID]
	if !ok {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (b *Backend) GetConversationType(ctx context.Context, convID string) (gm.ConversationType, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("GetConversationType"); err != nil {
		return gm.ConversationTypeUnknown, err
	}
	if c, ok := b.conversations[convID]; ok {
		return c.Type, nil
	}
	return gm.ConversationTypeUnknown, nil
}

// ListMessages pages newest-first with a real cursor. Google timestamps
// collide, so the message ID is always the tiebreaker and it is part of the
// cursor -- which is what makes the equal-timestamp case testable.
func (b *Backend) ListMessages(ctx context.Context, convID string, count int, cursor *gm.Cursor) ([]gm.Message, *gm.Cursor, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("ListMessages"); err != nil {
		return nil, nil, err
	}
	all := make([]gm.Message, 0, len(b.messages[convID]))
	for _, m := range b.messages[convID] {
		all = append(all, *m)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Timestamp.Equal(all[j].Timestamp) {
			return all[i].Timestamp.After(all[j].Timestamp)
		}
		return all[i].SourceID > all[j].SourceID
	})
	start := 0
	if cursor != nil {
		for i, m := range all {
			us := m.Timestamp.UnixMicro()
			if us < cursor.LastItemTimestampUS ||
				(us == cursor.LastItemTimestampUS && m.SourceID < cursor.LastItemID) {
				start = i
				break
			}
			start = i + 1
		}
	}
	if start >= len(all) {
		return nil, nil, nil
	}
	end := len(all)
	if count > 0 && start+count < end {
		end = start + count
	}
	page := all[start:end]
	var next *gm.Cursor
	if end < len(all) {
		last := page[len(page)-1]
		next = &gm.Cursor{LastItemID: last.SourceID, LastItemTimestampUS: last.Timestamp.UnixMicro()}
	}
	return append([]gm.Message(nil), page...), next, nil
}

func (b *Backend) ListContacts(ctx context.Context) ([]gm.Contact, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("ListContacts"); err != nil {
		return nil, err
	}
	return append([]gm.Contact(nil), b.contacts...), nil
}

func (b *Backend) ListTopContacts(ctx context.Context) ([]gm.Contact, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("ListTopContacts"); err != nil {
		return nil, err
	}
	return append([]gm.Contact(nil), b.topContacts...), nil
}

func (b *Backend) ContactAvatars(ctx context.Context, ids []string) (map[string][]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("ContactAvatars"); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, id := range ids {
		if data, ok := b.avatars[id]; ok {
			out[id] = append([]byte(nil), data...)
		}
	}
	return out, nil
}

func (b *Backend) DownloadAvatar(ctx context.Context, url string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("DownloadAvatar"); err != nil {
		return nil, err
	}
	return []byte("fake-avatar:" + url), nil
}

// --- writes -----------------------------------------------------------------

func (b *Backend) ResolveConversation(ctx context.Context, numbers []string, groupName string) (gm.ResolveResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("ResolveConversation"); err != nil {
		return gm.ResolveResult{}, err
	}
	status := gm.ResolveStatusSuccess
	if len(b.resolveStatuses) > 0 {
		status = b.resolveStatuses[0]
		b.resolveStatuses = b.resolveStatuses[1:]
		// The adapter retries a CREATE_RCS exactly once, exactly as upstream
		// does, so the fake consumes a second scripted status for the retry.
		if status == gm.ResolveStatusCreateRCS && len(b.resolveStatuses) > 0 {
			status = b.resolveStatuses[0]
			b.resolveStatuses = b.resolveStatuses[1:]
		}
	}
	if status != gm.ResolveStatusSuccess {
		return gm.ResolveResult{Status: status}, nil
	}
	id := "conv-" + strings.Join(numbers, "-")
	conv, ok := b.conversations[id]
	if !ok {
		newConv := gm.Conversation{
			SourceID:          id,
			Name:              groupName,
			IsGroup:           len(numbers) > 1,
			Type:              gm.ConversationTypeSMSMMS,
			SendModeRaw:       gm.SendModeAuto,
			Folder:            gm.FolderInbox,
			DefaultOutgoingID: "me@" + b.address,
			LastActivity:      b.clock.Now(),
		}
		// The OWNER's own participant first, then the recipients.
		//
		// Omitting it made a conversation created through the real
		// POST /v1/conversations serve a sender.id that was a well-formed
		// part_ ID resolving to no participants row, so `sender=me` returned
		// 0 against the real server while passing against a seeded
		// conversation. A fake whose shape differs from the real backend's
		// in one field tests the layer above it in a world that does not
		// exist -- and `is_me` is the field every `sender=me` query turns on
		// (spec section 7.6).
		newConv.Participants = append(newConv.Participants, gm.Participant{
			SourceID: newConv.DefaultOutgoingID, IsMe: true, IsVisible: true,
		})
		for _, n := range numbers {
			newConv.Participants = append(newConv.Participants, gm.Participant{
				SourceID: "part-" + n, PhoneE164: n, IsVisible: true,
			})
		}
		b.conversations[id] = &newConv
		conv = &newConv
	}
	cp := *conv
	return gm.ResolveResult{Conversation: &cp, Status: status}, nil
}

func (b *Backend) SendText(ctx context.Context, req gm.SendTextRequest) (gm.SendResult, error) {
	return b.doSend("SendText", req.ConversationID, req.TmpID, req.Text)
}

// SendMedia echoes the media part back on the message, which SendText's echo
// obviously does not carry.
//
// Without this the fake accepted a media send and emitted a message with no
// attachments, so nothing above it could tell a working media path from a
// broken one: section 16 Slice 2 test 17's "reserve, PUT, send" passed on the
// operation alone, and the att_ row the send is FOR was never asserted. A
// fake that quietly drops the payload makes every layer above it untestable
// in exactly the place it matters (spec section 13.1).
//
// The echo carries NO SIZE, which is not an omission: the Slice 2 live gate
// found every outgoing attachment row written with size_bytes null while
// incoming ones carried theirs, because Google's echo of media we sent
// returns a MediaContent with Size unset. The fake said the size back, so the
// fill that production needs was invisible here. A fake that is kinder than
// Google tests itself.
func (b *Backend) SendMedia(ctx context.Context, req gm.SendMediaRequest) (gm.SendResult, error) {
	att := []gm.Attachment{{
		PartIndex:     0,
		MediaID:       req.Media.MediaID,
		DecryptionKey: req.Media.DecryptionKey,
		Filename:      req.Media.Name,
		MimeType:      req.Media.MimeType,
		MediaFormat:   req.Media.Format,
	}}
	return b.doSend("SendMedia", req.ConversationID, req.TmpID, req.Caption, att...)
}

func (b *Backend) doSend(method, convID, tmpID, text string, attachments ...gm.Attachment) (gm.SendResult, error) {
	b.mu.Lock()
	if err := b.note(method); err != nil {
		b.mu.Unlock()
		return gm.SendResult{}, err
	}
	status := gm.SendStatusSuccess
	if len(b.sendStatuses) > 0 {
		status = b.sendStatuses[0]
		b.sendStatuses = b.sendStatuses[1:]
	}
	if status != gm.SendStatusSuccess {
		b.mu.Unlock()
		return gm.SendResult{Status: status}, nil
	}

	conv := b.conversations[convID]
	msgID := "msg-" + uuid.NewString()
	now := b.clock.Now()
	msg := gm.Message{
		SourceID:       msgID,
		ConversationID: convID,
		Text:           text,
		Timestamp:      now,
		// The echo carries back the TmpID it was given, so the
		// echo-correlation path of spec section 6.3 is exercised, not stubbed.
		TmpID:         tmpID,
		StatusRaw:     5, // OUTGOING_SENDING
		Kind:          gm.MessageKindMessage,
		DeliveryState: gm.DeliveryStateSending,
		Attachments:   attachments,
	}
	if conv != nil {
		msg.ParticipantID = conv.DefaultOutgoingID
		conv.LastActivity = now
		conv.LatestMessageID = msgID
	}
	b.putMessageLocked(msg)
	b.pending = append(b.pending, &pendingSend{convID: convID, msgID: msgID})
	b.mu.Unlock()

	b.Emit(&gm.EventMessage{Message: msg})
	return gm.SendResult{Status: gm.SendStatusSuccess}, nil
}

func (b *Backend) React(ctx context.Context, msgID string, emoji string, action gm.ReactionAction) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("React"); err != nil {
		return err
	}
	t, canonical := gm.CanonicaliseEmojiInput(emoji)
	for _, list := range b.messages {
		for _, m := range list {
			if m.SourceID != msgID {
				continue
			}
			// One reaction per person per message: an add over an existing
			// one replaces it (D23).
			filtered := m.Reactions[:0]
			for _, r := range m.Reactions {
				if len(r.ParticipantIDs) == 1 && r.ParticipantIDs[0] == "me" {
					continue
				}
				filtered = append(filtered, r)
			}
			m.Reactions = append([]gm.Reaction(nil), filtered...)
			if action != gm.ReactionActionRemove {
				m.Reactions = append(m.Reactions, gm.Reaction{
					Emoji: canonical, Type: t, ParticipantIDs: []string{"me"},
				})
			}
			return nil
		}
	}
	return gm.Classify(gm.RequestError{Type: 5, Message: "message not found"})
}

func (b *Backend) DeleteMessage(ctx context.Context, msgID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("DeleteMessage"); err != nil {
		return err
	}
	for _, list := range b.messages {
		for _, m := range list {
			if m.SourceID == msgID {
				m.StatusRaw = gm.MessageDeletedStatus
				m.DeliveryState = gm.DeliveryStateDeleted
				return nil
			}
		}
	}
	return nil
}

func (b *Backend) MarkRead(ctx context.Context, convID, msgID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("MarkRead"); err != nil {
		return err
	}
	if c, ok := b.conversations[convID]; ok {
		c.Unread = false
	}
	return nil
}

func (b *Backend) SetTyping(ctx context.Context, convID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("SetTyping"); err != nil {
		return err
	}
	return nil
}

func (b *Backend) UpdateConversation(ctx context.Context, convID string, ch gm.ConversationChange) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("UpdateConversation"); err != nil {
		return err
	}
	c, ok := b.conversations[convID]
	if !ok {
		return nil
	}
	if ch.Folder != nil {
		c.Folder = *ch.Folder
	}
	if ch.Pinned != nil {
		c.Pinned = *ch.Pinned
	}
	if ch.Unread != nil {
		c.Unread = *ch.Unread
	}
	return nil
}

func (b *Backend) DeleteConversation(ctx context.Context, convID, phone string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("DeleteConversation"); err != nil {
		return err
	}
	if c, ok := b.conversations[convID]; ok {
		c.Deleted = true
	}
	return nil
}

// --- media ------------------------------------------------------------------

func (b *Backend) Upload(ctx context.Context, data []byte, filename, mime string) (gm.MediaRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("Upload"); err != nil {
		return gm.MediaRef{}, err
	}
	id := "media-" + uuid.NewString()
	b.media[id] = append([]byte(nil), data...)
	return gm.MediaRef{
		MediaID:       id,
		Format:        "IMAGE_JPEG",
		Name:          filename,
		SizeBytes:     int64(len(data)),
		DecryptionKey: []byte("fake-decryption-key-32-bytes----"),
		MimeType:      mime,
	}, nil
}

func (b *Backend) Download(ctx context.Context, mediaID string, key []byte) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("Download"); err != nil {
		return nil, err
	}
	data, ok := b.media[mediaID]
	if !ok {
		return nil, gm.Classify(gm.RequestError{Type: 5, Message: "media not found"})
	}
	return append([]byte(nil), data...), nil
}

func (b *Backend) RequestFullSizeImage(ctx context.Context, msgID, actionMsgID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.note("RequestFullSizeImage"); err != nil {
		return err
	}
	return nil
}

// --- events -----------------------------------------------------------------

func (b *Backend) Events() <-chan gm.Event { return b.events }

// Emit delivers one event, non-blocking. An overflow, or a scripted drop,
// increments dropped_events.
func (b *Backend) Emit(ev gm.Event) {
	b.mu.Lock()
	drop := b.dropEvents
	b.mu.Unlock()
	if drop {
		b.dropped.Add(1)
		return
	}
	if _, ok := ev.(*gm.EventUnknown); ok {
		b.unknown.Add(1)
	}
	select {
	case b.events <- ev:
	default:
		b.dropped.Add(1)
	}
}

// EmitBatch delivers several events as one batch. abandonAfter reproduces the
// library's own dedup behaviour: on a hit the handler loop returns, abandoning
// every remaining part of the batch (spec section 3.4). Passing a value >= 0
// drops everything from that index on, which is real message loss, and is what
// the reconciliation sweep of spec section 5.4 exists for.
func (b *Backend) EmitBatch(abandonAfter int, evs ...gm.Event) {
	for i, ev := range evs {
		if abandonAfter >= 0 && i >= abandonAfter {
			b.dropped.Add(1)
			continue
		}
		b.Emit(ev)
	}
}

// --- delivery ladder --------------------------------------------------------

// deliveryLadder is the status sequence Google reports for an outgoing
// message: OUTGOING_SENDING, OUTGOING_COMPLETE, OUTGOING_DELIVERED,
// OUTGOING_DISPLAYED.
var deliveryLadder = []int32{5, 1, 2, 11}

// Advance moves this fake's clock forward and walks every in-flight outgoing
// message one rung up the delivery ladder, emitting the status update as a
// remote echo. An sms_mms conversation stops at `sent`, which is what makes
// the spec section 5.5 behaviour testable.
//
// It is the only way delivery progresses: the fake never uses wall time, so
// no test sleeps.
func (b *Backend) Advance(d time.Duration) {
	if f, ok := b.clock.(*clock.Fake); ok {
		f.Advance(d)
	}
	b.StepDelivery()
}

// StepDelivery walks the ladder without touching the clock.
func (b *Backend) StepDelivery() {
	b.mu.Lock()
	var echoes []gm.Message
	remaining := b.pending[:0]
	for _, p := range b.pending {
		conv := b.conversations[p.convID]
		maxRung := len(deliveryLadder) - 1
		// SMS usually stops at `sent`, and so does the fake, unless the
		// conversation is RCS.
		if conv == nil || conv.Type != gm.ConversationTypeRCS {
			maxRung = 2
		}
		if conv != nil && conv.Type == gm.ConversationTypeSMSMMS {
			maxRung = 1
		}
		if p.rung >= maxRung {
			continue
		}
		p.rung++
		raw := deliveryLadder[p.rung]
		for _, m := range b.messages[p.convID] {
			if m.SourceID != p.msgID {
				continue
			}
			state, _ := gm.DeliveryStateFor(raw)
			m.StatusRaw = raw
			m.DeliveryState = state
			echoes = append(echoes, *m)
		}
		if p.rung < maxRung {
			remaining = append(remaining, p)
		}
	}
	b.pending = remaining
	b.mu.Unlock()

	for _, m := range echoes {
		b.Emit(&gm.EventMessage{Message: m})
	}
}

// --- session persistence ----------------------------------------------------

type fakeSession struct {
	Address string            `json:"address"`
	PhoneID string            `json:"phone_id"`
	Cookies map[string]string `json:"cookies"`
}

func (b *Backend) MarshalSession() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, err := json.Marshal(fakeSession{Address: b.address, PhoneID: b.phoneID, Cookies: b.cookies})
	if err != nil {
		return nil, err
	}
	b.session = append([]byte(nil), data...)
	b.dirty = false
	return data, nil
}

func (b *Backend) LoadSession(data []byte) error {
	var s fakeSession
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.address = s.Address
	b.deviceEmail = s.Address
	b.phoneID = s.PhoneID
	b.cookies = s.Cookies
	b.loggedIn = len(s.Cookies) > 0
	b.session = append([]byte(nil), data...)
	b.dirty = false
	return nil
}

func (b *Backend) SessionDirty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dirty
}

func (b *Backend) AccountAddress() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.address
}

func (b *Backend) Shred() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cookies = nil
	b.loggedIn = false
	b.connected = false
	b.session = nil
}
