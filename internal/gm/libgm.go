package gm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
)

// LibGM is the real Google Messages backend: one instance is one account.
//
// It is the only place in Agent GM that imports libgm or gmproto
// (spec section 2.3).
type LibGM struct {
	pushLog *zerolog.Logger
	push    *pushRuntime
	client  *libgm.Client
	auth    *libgm.AuthData
	log     zerolog.Logger

	eventsCh chan Event

	dropped     atomic.Uint64
	unknown     atomic.Uint64
	lastPhoneID atomicString

	mu           sync.Mutex
	lastSessions []byte
}

var _ Backend = (*LibGM)(nil)
var _ SessionPersister = (*LibGM)(nil)
var _ DroppedEventCounter = (*LibGM)(nil)

// New builds a backend for a brand-new pairing.
//
// Agent GM always passes pk == nil to libgm.NewClient: there is no web push
// (spec section 3.1). PairCallback is deliberately left nil -- it fires only
// from the withdrawn QR flow's completePairing and is unreachable on the gaia
// path, where DoGaiaPairing emits PairSuccessful directly.
func New(logger zerolog.Logger) *LibGM {
	return newFromAuth(libgm.NewAuthData(), logger)
}

// NewFromSession rebuilds a backend from a persisted session, which is
// libgm.AuthData marshalled to JSON (spec section 3.3).
func NewFromSession(data []byte, logger zerolog.Logger) (*LibGM, error) {
	var auth libgm.AuthData
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("session is not valid AuthData JSON: %w", err)
	}
	b := newFromAuth(&auth, logger)
	b.lastSessions = append([]byte(nil), data...)
	return b, nil
}

func newFromAuth(auth *libgm.AuthData, logger zerolog.Logger) *LibGM {
	b := &LibGM{
		auth:     auth,
		log:      logger,
		eventsCh: make(chan Event, EventBufferSize),
	}
	b.client = libgm.NewClient(auth, nil, logger, exhttp.SensibleClientSettings)
	b.client.SetEventHandler(b.handleEvent)
	return b
}

// --- lifecycle --------------------------------------------------------------

// Connect refreshes the tachyon token synchronously so bad credentials
// surface immediately, then starts long polling. It is the only connect path
// Agent GM uses; ConnectBackground is deliberately not called.
func (b *LibGM) Connect(ctx context.Context) error {
	if b.push != nil {
		return b.connectPush(ctx)
	}
	if err := b.client.Connect(ctx); err != nil {
		return Classify(translateError(err))
	}
	return nil
}

func (b *LibGM) Disconnect() {
	if b.push != nil {
		b.disconnectPush()
		return
	}
	b.client.Disconnect()
}

// IsConnected is advisory only: upstream notes it is imprecise during
// reconnects (spec section 3.1).
func (b *LibGM) IsConnected() bool {
	if b.push != nil {
		return b.push.started.Load()
	}
	return b.client.IsConnected()
}

// IsLoggedIn is the authoritative "are we paired" check.
func (b *LibGM) IsLoggedIn() bool { return b.client.IsLoggedIn() }

func (b *LibGM) SessionID() string { return b.client.CurrentSessionID() }

func (b *LibGM) DroppedEvents() uint64 { return b.dropped.Load() }
func (b *LibGM) UnknownEvents() uint64 { return b.unknown.Load() }

// --- health -----------------------------------------------------------------

func (b *LibGM) rawFetchConfig(ctx context.Context) (ConfigInfo, error) {
	if err := b.client.FetchConfig(ctx); err != nil {
		return ConfigInfo{}, Classify(translateError(err))
	}
	cfg := b.client.Config
	if cfg == nil {
		return ConfigInfo{}, Classify(errors.New("no config after FetchConfig"))
	}
	version, err := cfg.ParsedClientVersion()
	if err != nil {
		return ConfigInfo{}, Classify(fmt.Errorf("failed to parse live client version: %w", err))
	}
	return ConfigInfo{
		Live:        convertConfigVersion(version),
		DeviceEmail: cfg.GetDeviceInfo().GetEmail(),
		DeviceID:    cfg.GetDeviceInfo().GetDeviceID(),
	}, nil
}

// CompiledConfigVersion is util.ConfigMessage, a property of this binary.
func (b *LibGM) CompiledConfigVersion() ConfigVersion {
	return convertConfigVersion(util.ConfigMessage)
}

// CompiledConfigVersion is exposed as a package function too, because
// GET /v1/health carries the compiled value once at the top level, without an
// account (spec section 3.7).
func CompiledConfigVersion() ConfigVersion {
	return convertConfigVersion(util.ConfigMessage)
}

func (b *LibGM) rawIsDefaultSMSApp(ctx context.Context) (bool, error) {
	resp, err := b.client.IsBugleDefault(ctx)
	if err != nil {
		return false, Classify(translateError(err))
	}
	return resp.GetSuccess(), nil
}

// --- pairing ----------------------------------------------------------------

// GaiaRequiredCookies is the exact required set (connector/login.go:200-210).
// SAPISID alone is not sufficient; its separate job is computing the
// SAPISIDHASH Authorization header.
var GaiaRequiredCookies = []string{"SID", "HSID", "OSID", "SSID", "APISID", "SAPISID"}

// GaiaOptionalCookies is the one cookie that is read but not required.
var GaiaOptionalCookies = []string{"__Secure-1PSIDTS"}

// GaiaCookieDomains gives each cookie the host it is scoped to. OSID is
// host-scoped to messages.google.com, so a capture that reads only
// .google.com silently returns an unusable set -- the most common way the
// gaia flow fails (spec section 3.2).
var GaiaCookieDomains = map[string]string{
	"OSID":             "messages.google.com",
	"SID":              ".google.com",
	"HSID":             ".google.com",
	"SSID":             ".google.com",
	"APISID":           ".google.com",
	"SAPISID":          ".google.com",
	"__Secure-1PSIDTS": ".google.com",
}

// GaiaHTTPOnlyCookies names the five of the seven that are httpOnly, so
// document.cookie and any injected-JS approach cannot reach them. This is why
// spec section 11.4 reads the browser's own cookie store over CDP.
var GaiaHTTPOnlyCookies = []string{"SID", "HSID", "SSID", "OSID", "__Secure-1PSIDTS"}

// GaiaCaptureURL is upstream's own capture URL (connector/login.go:229).
const GaiaCaptureURL = "https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config"

// MissingRequiredCookies names the required cookies a capture did not
// produce, so the caller can say which ones and which domain each comes from.
func MissingRequiredCookies(cookies map[string]string) []string {
	var missing []string
	for _, name := range GaiaRequiredCookies {
		if cookies[name] == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// StartGooglePairing runs the gaia flow start to finish through
// DoGaiaPairing, which is the call Agent GM uses (spec section 3.1). Agent GM
// does not reconnect on its own afterwards: DoGaiaPairing reconnects in its
// own goroutine on success (pair_google.go:311-318).
func (b *LibGM) StartGooglePairing(ctx context.Context, cookies map[string]string, deviceIndex int, emoji func(string)) (PairedDevice, error) {
	if len(cookies) == 0 {
		return PairedDevice{}, Classify(ErrNoCookies)
	}
	if missing := MissingRequiredCookies(cookies); len(missing) > 0 {
		e := Classify(ErrNoCookies)
		e.Details = map[string]any{"missing_cookies": missing}
		return PairedDevice{}, e
	}
	b.auth.SetCookies(copyCookies(cookies))
	// GaiaHackyDeviceSwitcher selects among several primary-looking devices
	// as primaryDevices[switcher % len] after a newest-first sort. Surfaced
	// as --device-index N (spec section 3.1).
	b.client.GaiaHackyDeviceSwitcher = deviceIndex

	// AuthData.Mobile is populated inside StartGaiaPairing, before the owner
	// confirms the emoji (pair_google.go:325 -> :101-104), and the emoji
	// callback fires between StartGaiaPairing and FinishGaiaPairing. So the
	// account identity is knowable at emoji time, which is what gives the
	// pairing row of spec section 3.2 its bounded lifecycle: a caller's emoji
	// callback can read AccountAddress() and create or resume the row before
	// the pairing is known to succeed.
	if err := b.client.DoGaiaPairing(ctx, emoji); err != nil {
		return PairedDevice{}, Classify(translateError(err))
	}

	// AuthData.Mobile.SourceID is the account identifier: the Google account
	// address, lowercased by signInGaiaGetToken (pair_google.go:102-105).
	address, err := AccountAddressFromPairing(b.auth.Mobile.GetSourceID())
	if err != nil {
		return PairedDevice{}, err
	}

	dev := PairedDevice{
		// FinishGaiaPairing's return value, "<mobile sourceID>/<destRegDevice
		// int>", reaches Agent GM on events.PairSuccessful, which
		// DoGaiaPairing emits synchronously before it returns
		// (pair_google.go:310).
		PhoneID:        b.lastPhoneID.Load(),
		AccountAddress: address,
		DeviceIndex:    deviceIndex,
	}
	if b.auth.DestRegID != uuid.Nil {
		dev.DestRegUUID = b.auth.DestRegID.String()
	}
	// DeviceLastSeen and DeviceCount are left zero: StartGaiaPairing logs the
	// chosen device's LastSeen and the candidate count but does not return
	// them, and DoGaiaPairing does not surface the PairingSession. Recorded
	// in the slice report as the one section 11.4 line this cannot serve.
	return dev, nil
}

// atomicString is a tiny atomic string cell: the pairing event handler runs
// on the long-poll goroutine while StartGooglePairing waits on the caller's.
type atomicString struct{ v atomic.Value }

func (a *atomicString) Store(s string) { a.v.Store(s) }
func (a *atomicString) Load() string {
	s, _ := a.v.Load().(string)
	return s
}

// AccountAddressFromPairing normalises and validates the address a pairing
// produced. It is the single place the refusal lives, so a test can reach it
// without a real libgm.Client.
//
// An empty or implausible address is refused BEFORE anything is created:
// acct_ is UUIDv5(ns, "account", address), so AccountID("") is a perfectly
// valid UUID and two different degenerate pairings would derive the same
// acct_ ID and adopt each other's conversations and messages (spec 3.2, 4.1).
// Agent GM never falls back to a generated ID.
func AccountAddressFromPairing(rawSourceID string) (string, error) {
	address := strings.ToLower(rawSourceID)
	if !PlausibleAccountAddress(address) {
		return "", Classify(ErrNoAccountAddress)
	}
	return address, nil
}

// PlausibleAccountAddress reports whether a value is a syntactically
// plausible Google account address.
func PlausibleAccountAddress(s string) bool { return plausibleAddress(s) }

func plausibleAddress(s string) bool {
	if s == "" {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n")
}

// RefreshGoogleCookies re-authenticates an existing pairing with fresh
// cookies, exactly as connector/login.go:246-289 does: it is accepted only
// when the tachyon token and the pairing ID are both present, the account is
// verified the same by comparing Config.DeviceInfo.Email against
// AuthData.Mobile.SourceID, and on any failure the previous cookies are
// restored so a failed refresh changes nothing.
//
// A refresh against a different Google account is refused with
// pairing_wrong_account. That is stricter than upstream on purpose: upstream
// falls through to a fresh pairing, which here would silently create a second
// account (spec section 3.2, declared deviation).
func (b *LibGM) RefreshGoogleCookies(ctx context.Context, cookies map[string]string) error {
	// Config validation is an ordinary HTTPS call and must also work while a
	// push client is stopped. Serialize cookie changes with passive batches.
	locked := false
	if b.push != nil {
		select {
		case b.push.gate <- struct{}{}:
			locked = true
		case <-ctx.Done():
			return ctx.Err()
		}
		defer func() {
			if locked {
				<-b.push.gate
			}
		}()
	}
	if b.auth.TachyonAuthToken == nil || b.auth.PairingID == uuid.Nil {
		return Classify(ErrRequestedEntityNotFound)
	}
	if missing := MissingRequiredCookies(cookies); len(missing) > 0 {
		e := Classify(ErrNoCookies)
		e.Details = map[string]any{"missing_cookies": missing}
		return e
	}

	b.auth.CookiesLock.RLock()
	previous := copyCookies(b.auth.Cookies)
	b.auth.CookiesLock.RUnlock()
	restore := func() { b.auth.SetCookies(previous) }

	b.auth.SetCookies(copyCookies(cookies))
	info, err := b.rawFetchConfig(ctx)
	if err != nil {
		restore()
		return err
	}
	if !strings.EqualFold(info.DeviceEmail, b.auth.Mobile.GetSourceID()) {
		restore()
		return Classify(ErrWrongAccount)
	}
	if locked {
		<-b.push.gate
		locked = false
	}
	if err := b.Connect(ctx); err != nil {
		restore()
		return err
	}
	return nil
}

func copyCookies(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// --- reads ------------------------------------------------------------------

// ListConversations must be called at least once per account's client before
// that account's live conversation events are trustworthy: the first call
// sends BUGLE_ANNOTATION and later calls BUGLE_MESSAGE, and the flag lives on
// the Client, of which there is one per account (spec section 3.7).
func (b *LibGM) rawListConversations(ctx context.Context, folder Folder, count int) ([]Conversation, error) {
	resp, err := b.client.ListConversations(ctx, count, gmproto.ListConversationsRequest_Folder(folder))
	if err != nil {
		return nil, Classify(translateError(err))
	}
	out := make([]Conversation, 0, len(resp.GetConversations()))
	for _, c := range resp.GetConversations() {
		out = append(out, ConvertConversation(c))
	}
	return out, nil
}

func (b *LibGM) rawGetConversation(ctx context.Context, convID string) (*Conversation, error) {
	c, err := b.client.GetConversation(ctx, convID)
	if err != nil {
		return nil, Classify(translateError(err))
	}
	if c == nil {
		return nil, nil
	}
	conv := ConvertConversation(c)
	return &conv, nil
}

func (b *LibGM) rawGetConversationType(ctx context.Context, convID string) (ConversationType, error) {
	resp, err := b.client.GetConversationType(ctx, convID)
	if err != nil {
		return ConversationTypeUnknown, Classify(translateError(err))
	}
	return convertConversationType(gmproto.ConversationType(resp.GetType())), nil
}

func (b *LibGM) rawListMessages(ctx context.Context, convID string, count int, cursor *Cursor) ([]Message, *Cursor, error) {
	resp, err := b.client.FetchMessages(ctx, convID, int64(count), toProtoCursor(cursor))
	if err != nil {
		return nil, nil, Classify(translateError(err))
	}
	out := make([]Message, 0, len(resp.GetMessages()))
	for _, m := range resp.GetMessages() {
		out = append(out, ConvertMessage(m))
	}
	return out, convertCursor(resp.GetCursor()), nil
}

func (b *LibGM) rawListContacts(ctx context.Context) ([]Contact, error) {
	resp, err := b.client.ListContacts(ctx)
	if err != nil {
		return nil, Classify(translateError(err))
	}
	out := make([]Contact, 0, len(resp.GetContacts()))
	for _, c := range resp.GetContacts() {
		out = append(out, convertContact(c, false))
	}
	return out, nil
}

func (b *LibGM) rawListTopContacts(ctx context.Context) ([]Contact, error) {
	resp, err := b.client.ListTopContacts(ctx)
	if err != nil {
		return nil, Classify(translateError(err))
	}
	out := make([]Contact, 0, len(resp.GetContacts()))
	for _, c := range resp.GetContacts() {
		out = append(out, convertContact(c, true))
	}
	return out, nil
}

func (b *LibGM) rawContactAvatars(ctx context.Context, ids []string) (map[string][]byte, error) {
	resp, err := b.client.GetParticipantThumbnail(ctx, ids...)
	if err != nil {
		return nil, Classify(translateError(err))
	}
	out := make(map[string][]byte, len(resp.GetThumbnail()))
	for _, t := range resp.GetThumbnail() {
		out[t.GetIdentifier()] = append([]byte(nil), t.GetData().GetImageBuffer()...)
	}
	return out, nil
}

func (b *LibGM) rawDownloadAvatar(ctx context.Context, url string) ([]byte, error) {
	data, err := b.client.DownloadAvatar(ctx, url)
	if err != nil {
		return nil, Classify(translateError(err))
	}
	return data, nil
}

// --- writes -----------------------------------------------------------------

// ResolveConversation is GetOrCreateConversation. CREATE_RCS means the phone
// wants the caller to retry with CreateRCSGroup=true and a non-nil
// RCSGroupName (empty string is acceptable); upstream retries exactly once,
// exactly that way (connector/startchat.go:214-219), and so does Agent GM. A
// second CREATE_RCS is reported to the caller as such, and core maps it to
// google_error.
func (b *LibGM) rawResolveConversation(ctx context.Context, numbers []string, groupName string) (ResolveResult, error) {
	return resolveConversation(ctx, b.client, numbers, groupName)
}

// conversationCreator is the one libgm call resolveConversation makes.
//
// It exists so the D34 divergence is TESTABLE without a phone. The divergence
// is invisible to every other test in this repository -- the fake takes a
// name and a number list, not a request -- so putting `RCSGroupName` back on
// the first call left the whole suite green while reintroducing the exact bug
// the live gate found. A comment is not a test, and the "fix" a future reader
// would reach for, seeing Agent GM omit a field upstream sets, is precisely
// the revert.
type conversationCreator interface {
	GetOrCreateConversation(ctx context.Context,
		req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error)
}

func resolveConversation(ctx context.Context, client conversationCreator,
	numbers []string, groupName string) (ResolveResult, error) {
	req := &gmproto.GetOrCreateConversationRequest{
		Numbers: make([]*gmproto.ContactNumber, len(numbers)),
	}
	for i, n := range numbers {
		req.Numbers[i] = &gmproto.ContactNumber{MysteriousInt: 2, Number: n, Number2: n}
	}

	// **The name is NOT sent on the first call.** This is a deliberate
	// divergence from upstream, and it is the one place in this file that
	// diverges (D34).
	//
	// `connector/startchat.go:186-189` at the pin sets `RCSGroupName` on the
	// first `GetOrCreateConversation`. Agent GM's Slice 2 live gate found
	// that a named start with SMS/MMS recipients FAILS there, and the same
	// start without a name succeeds seconds later against the same phone and
	// the same numbers. A name is an RCS group concept; Google refuses it on
	// the call that is still deciding whether this is an RCS group at all.
	//
	// So the first call asks the question -- is this an RCS group? -- and the
	// name goes on the retry, which is the call that actually creates one.
	// That is where upstream puts it too when the first call comes back
	// CREATE_RCS; the divergence is only about the first.
	resp, err := client.GetOrCreateConversation(ctx, req)
	if err != nil {
		return ResolveResult{}, Classify(translateError(err))
	}
	if resp.GetStatus() == gmproto.GetOrCreateConversationResponse_CREATE_RCS {
		// The retry IS the RCS group creation, so this is where the name
		// belongs. An empty string is acceptable and is what upstream sends
		// when there is no name.
		name := groupName
		req.RCSGroupName = &name
		create := true
		req.CreateRCSGroup = &create
		resp, err = client.GetOrCreateConversation(ctx, req)
		if err != nil {
			return ResolveResult{}, Classify(translateError(err))
		}
	}
	out := ResolveResult{Status: ResolveStatus(resp.GetStatus())}
	if resp.GetConversation() != nil {
		conv := ConvertConversation(resp.GetConversation())
		out.Conversation = &conv
	}
	return out, nil
}

// buildSendRequest mirrors connector/handlematrix.go:ConvertMatrixMessage.
// TmpID, MessagePayload.TmpID and MessagePayload.TmpID2 are all three set to
// the same bare UUID (D22), and ParticipantID must be the conversation's
// DefaultOutgoingID -- sending with the wrong participant ID is how messages
// end up attributed to the wrong SIM.
func buildSendRequest(convID, participantID, tmpID, replyTo string, forceRCS bool, sim []byte, infos []*gmproto.MessageInfo) *gmproto.SendMessageRequest {
	req := &gmproto.SendMessageRequest{
		ConversationID: convID,
		MessagePayload: &gmproto.MessagePayload{
			TmpID:          tmpID,
			TmpID2:         tmpID,
			ConversationID: convID,
			ParticipantID:  participantID,
			MessageInfo:    infos,
		},
		SIMPayload: toProtoSIMPayload(sim),
		TmpID:      tmpID,
		ForceRCS:   forceRCS,
	}
	if replyTo != "" {
		req.Reply = &gmproto.ReplyPayload{MessageID: replyTo}
	}
	return req
}

func (b *LibGM) rawSendText(ctx context.Context, req SendTextRequest) (SendResult, error) {
	infos := []*gmproto.MessageInfo{{
		Data: &gmproto.MessageInfo_MessageContent{
			MessageContent: &gmproto.MessageContent{Content: req.Text},
		},
	}}
	return b.send(ctx, buildSendRequest(req.ConversationID, req.ParticipantID, req.TmpID,
		req.ReplyToMessageID, req.ForceRCS, req.SIMPayload, infos))
}

func (b *LibGM) rawSendMedia(ctx context.Context, req SendMediaRequest) (SendResult, error) {
	format, ok := gmproto.MediaFormats_value[req.Media.Format]
	if !ok {
		format = int32(gmproto.MediaFormats_UNSPECIFIED_TYPE)
	}
	infos := []*gmproto.MessageInfo{{
		Data: &gmproto.MessageInfo_MediaContent{
			MediaContent: &gmproto.MediaContent{
				Format:        gmproto.MediaFormats(format),
				MediaID:       req.Media.MediaID,
				MediaName:     req.Media.Name,
				Size:          req.Media.SizeBytes,
				DecryptionKey: req.Media.DecryptionKey,
				MimeType:      req.Media.MimeType,
			},
		},
	}}
	// A caption is appended as a second MessageInfo entry carrying
	// MessageContent, exactly as upstream does (spec section 3.1).
	if req.Caption != "" {
		infos = append(infos, &gmproto.MessageInfo{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: req.Caption},
			},
		})
	}
	return b.send(ctx, buildSendRequest(req.ConversationID, req.ParticipantID, req.TmpID,
		req.ReplyToMessageID, req.ForceRCS, req.SIMPayload, infos))
}

func (b *LibGM) send(ctx context.Context, req *gmproto.SendMessageRequest) (SendResult, error) {
	resp, err := b.client.SendMessage(ctx, req)
	if err != nil {
		return SendResult{}, Classify(translateError(err))
	}
	return SendResult{
		Status:              SendStatus(resp.GetStatus()),
		GoogleAccountSwitch: resp.GetGoogleAccountSwitch().GetAccount(),
	}, nil
}

func (b *LibGM) rawReact(ctx context.Context, msgID string, emoji string, action ReactionAction) error {
	// Every inbound emoji is canonicalised before it reaches the wire, so
	// that adding the two spellings of a heart cannot be two reactions.
	t, canonical := CanonicaliseEmojiInput(emoji)
	raw, _ := RawEmojiType(t)
	unicode := emoji
	if canonical != nil {
		unicode = *canonical
	}
	resp, err := b.client.SendReaction(ctx, &gmproto.SendReactionRequest{
		MessageID: msgID,
		ReactionData: &gmproto.ReactionData{
			Unicode: unicode,
			Type:    gmproto.EmojiType(raw),
		},
		Action: gmproto.SendReactionRequest_Action(action),
	})
	if err != nil {
		return Classify(translateError(err))
	}
	if !resp.GetSuccess() {
		return Classify(RequestError{Message: "the phone rejected the reaction"})
	}
	return nil
}

func (b *LibGM) rawDeleteMessage(ctx context.Context, msgID string) error {
	resp, err := b.client.DeleteMessage(ctx, msgID)
	if err != nil {
		return Classify(translateError(err))
	}
	if !resp.GetSuccess() {
		return Classify(RequestError{Message: "the phone rejected the delete"})
	}
	return nil
}

func (b *LibGM) rawMarkRead(ctx context.Context, convID, msgID string) error {
	if err := b.client.MarkRead(ctx, convID, msgID); err != nil {
		return Classify(translateError(err))
	}
	return nil
}

func (b *LibGM) rawSetTyping(ctx context.Context, convID string) error {
	if err := b.client.SetTyping(ctx, convID, nil); err != nil {
		return Classify(translateError(err))
	}
	return nil
}

// UpdateConversation moves a conversation between folders.
//
// The pinned library exposes no action for pinning or for marking unread:
// UpdateConversationData's oneof declares only `status` and `mute`
// (gmproto/client.proto:200-206). Asking for either is refused rather than
// silently ignored.
func (b *LibGM) rawUpdateConversation(ctx context.Context, convID string, ch ConversationChange) error {
	if ch.Pinned != nil || ch.Unread != nil {
		e := newError(CodeUnsupportedCapability, 409,
			"Google Messages for web exposes no way to pin a conversation or mark it unread")
		e.Reason = "not_supported_by_google"
		return e
	}
	if ch.Folder == nil {
		return nil
	}
	var status gmproto.ConversationStatus
	switch *ch.Folder {
	case FolderInbox:
		status = gmproto.ConversationStatus_ACTIVE
	case FolderArchive:
		status = gmproto.ConversationStatus_ARCHIVED
	case FolderSpamBlocked:
		status = gmproto.ConversationStatus_SPAM_FOLDER
	default:
		e := newError(CodeUnsupportedCapability, 409, "unknown folder")
		e.Reason = "not_supported_by_google"
		return e
	}
	resp, err := b.client.UpdateConversation(ctx, &gmproto.UpdateConversationRequest{
		ConversationID: convID,
		Data: &gmproto.UpdateConversationRequest_UpdateData{
			UpdateData: &gmproto.UpdateConversationData{
				ConversationID: convID,
				Data:           &gmproto.UpdateConversationData_Status{Status: status},
			},
		},
	})
	if err != nil {
		return Classify(translateError(err))
	}
	if !resp.GetSuccess() {
		return Classify(RequestError{Message: "the phone rejected the conversation update"})
	}
	return nil
}

// DeleteConversation is Google's delete-for-me and the only delete Agent GM
// has (non-goal N6).
func (b *LibGM) rawDeleteConversation(ctx context.Context, convID, phone string) error {
	if err := b.client.DeleteConversation(ctx, convID, phone); err != nil {
		return Classify(translateError(err))
	}
	return nil
}

// --- media ------------------------------------------------------------------

// UploadMedia and DownloadMedia take no context.Context at the pinned commit
// and can block for the duration of a large transfer, so each is wrapped in a
// goroutine plus a select on ctx.Done(). Cancelling only abandons the result:
// the underlying HTTP request runs to completion (spec section 3.1).

func (b *LibGM) rawUpload(ctx context.Context, data []byte, filename, mime string) (MediaRef, error) {
	type result struct {
		mc  *gmproto.MediaContent
		err error
	}
	done := make(chan result, 1)
	go func() {
		mc, err := b.client.UploadMedia(data, filename, mime)
		done <- result{mc, err}
	}()
	select {
	case <-ctx.Done():
		return MediaRef{}, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return MediaRef{}, Classify(translateError(r.err))
		}
		return MediaRef{
			MediaID:       r.mc.GetMediaID(),
			Format:        r.mc.GetFormat().String(),
			Name:          r.mc.GetMediaName(),
			SizeBytes:     r.mc.GetSize(),
			DecryptionKey: append([]byte(nil), r.mc.GetDecryptionKey()...),
			MimeType:      r.mc.GetMimeType(),
		}, nil
	}
}

func (b *LibGM) rawDownload(ctx context.Context, mediaID string, key []byte) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := b.client.DownloadMedia(mediaID, key)
		done <- result{data, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return nil, Classify(translateError(r.err))
		}
		return r.data, nil
	}
}

func (b *LibGM) rawRequestFullSizeImage(ctx context.Context, msgID, actionMsgID string) error {
	if _, err := b.client.GetFullSizeImage(ctx, msgID, actionMsgID); err != nil {
		return Classify(translateError(err))
	}
	return nil
}

// --- events -----------------------------------------------------------------

func (b *LibGM) Events() <-chan Event { return b.eventsCh }

// emit does a non-blocking send. The libgm event handler is called
// synchronously from the long-poll loop and must not block, so an overflow
// increments dropped_events rather than stalling the poller
// (spec section 2.3).
func (b *LibGM) emit(ev Event) {
	select {
	case b.eventsCh <- ev:
	default:
		b.dropped.Add(1)
	}
}

func (b *LibGM) handleEvent(raw any) {
	switch e := raw.(type) {
	case *events.ClientReady:
		conv := make([]Conversation, 0, len(e.Conversations))
		for _, c := range e.Conversations {
			conv = append(conv, ConvertConversation(c))
		}
		b.emit(&EventClientReady{SessionID: e.SessionID, Conversations: conv})
	case *events.AuthTokenRefreshed:
		b.emit(&EventAuthTokenRefreshed{})
	case *events.ListenTemporaryError:
		b.emit(&EventListenTemporaryError{Err: translateError(e.Error)})
	case *events.ListenRecovered:
		b.emit(&EventListenRecovered{})
	case *events.ListenFatalError:
		err := translateError(e.Error)
		b.emit(&EventListenFatalError{Err: err, CredentialsDead: IsFatalListenError(err)})
	case *events.PingFailed:
		err := translateError(e.Error)
		b.emit(&EventPingFailed{
			Err:            err,
			ErrorCount:     e.ErrorCount,
			EntityNotFound: errors.Is(err, ErrRequestedEntityNotFound),
		})
	case *events.PhoneNotResponding:
		b.emit(&EventPhoneNotResponding{})
	case *events.PhoneRespondingAgain:
		b.emit(&EventPhoneRespondingAgain{})
	case *events.NoDataReceived:
		b.emit(&EventNoDataReceived{})
	case *events.HackySetActiveMayFail:
		b.emit(&EventHackySetActiveMayFail{})
	case *events.PairSuccessful:
		// QRData is always nil on this path; it is never dereferenced.
		b.lastPhoneID.Store(e.PhoneID)
		b.emit(&EventPairSuccessful{PhoneID: e.PhoneID})
	case *events.GaiaLoggedOut:
		b.emit(&EventGaiaLoggedOut{})
	case *events.AccountChange:
		b.emit(&EventAccountChange{Account: e.GetAccount(), IsFake: e.IsFake})
	case *gmproto.RevokePairData:
		b.emit(&EventRevokePairData{})
	case *libgm.WrappedMessage:
		b.emit(&EventMessage{Message: ConvertMessage(e.Message), IsOld: e.IsOld})
	case *gmproto.Conversation:
		b.emit(&EventConversation{Conversation: ConvertConversation(e)})
	case *gmproto.UserAlertEvent:
		b.emit(&EventUserAlert{Alert: AlertType(e.GetAlertType())})
	case *gmproto.Settings:
		var pushEnabled *bool
		if e.GetOpCodeData() != nil {
			pushEnabled = e.GetOpCodeData().PushEnabled
		}
		b.emit(&EventSettings{
			PushEnabled:     pushEnabled,
			IsDefaultSMSApp: e.GetRCSSettings().GetIsDefaultSMSApp(),
			RCSEnabled:      e.GetRCSSettings().GetIsEnabled(),
			SIMCount:        len(e.GetSIMCards()),
		})
	case *gmproto.TypingData:
		b.emit(&EventTyping{
			ConversationID: e.GetConversationID(),
			Number:         e.GetUser().GetNumber(),
			Until:          time.Now().Add(10 * time.Second),
		})
	default:
		b.unknown.Add(1)
		b.emit(&EventUnknown{TypeName: fmt.Sprintf("%T", raw)})
	}
}

// --- session persistence ----------------------------------------------------

// SaveSession keeps the snapshot and its disk write in the same passive-client
// critical section, so a timer cannot overwrite a newer batch's credentials.
func (b *LibGM) SaveSession(save func([]byte) error) error {
	if b.push != nil {
		b.push.gate <- struct{}{}
		defer func() { <-b.push.gate }()
	}
	data, err := b.marshalSession()
	if err != nil {
		return err
	}
	return save(data)
}

func (b *LibGM) MarshalSession() ([]byte, error) {
	if b.push != nil {
		b.push.gate <- struct{}{}
		defer func() { <-b.push.gate }()
	}
	return b.marshalSession()
}

func (b *LibGM) marshalSession() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.auth.CookiesLock.RLock()
	data, err := json.Marshal(b.auth)
	b.auth.CookiesLock.RUnlock()
	if err != nil {
		return nil, err
	}
	b.lastSessions = append([]byte(nil), data...)
	return data, nil
}

func (b *LibGM) LoadSession(data []byte) error {
	var auth libgm.AuthData
	if err := json.Unmarshal(data, &auth); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// AuthData carries a sync.RWMutex, so it is never copied by value: the
	// client is rebuilt around the new pointer instead, keeping this
	// backend's event channel and counters.
	b.auth = &auth
	b.client = libgm.NewClient(b.auth, nil, b.log, exhttp.SensibleClientSettings)
	b.client.SetEventHandler(b.handleEvent)
	b.lastSessions = append([]byte(nil), data...)
	return nil
}

// SessionDirty compares the in-memory AuthData against what was last
// marshalled. Cookies mutate in place via UpdateCookiesFromResponse with no
// event (client.go:73-81), so the 5-minute timer of spec section 3.3 is the
// only thing that captures a rotation, and this is how it decides.
func (b *LibGM) SessionDirty() bool {
	if b.push != nil {
		return true
	}
	b.auth.CookiesLock.RLock()
	data, err := json.Marshal(b.auth)
	b.auth.CookiesLock.RUnlock()
	if err != nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(data) != string(b.lastSessions)
}

func (b *LibGM) AccountAddress() string {
	return strings.ToLower(b.auth.Mobile.GetSourceID())
}

// Shred zeroes the in-memory AuthData so the Google account cookies are gone
// from the process as well as from disk (spec section 4.7).
func (b *LibGM) Shred() {
	b.auth.SetCookies(nil)
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.auth.TachyonAuthToken {
		b.auth.TachyonAuthToken[i] = 0
	}
	for i := range b.auth.WebEncryptionKey {
		b.auth.WebEncryptionKey[i] = 0
	}
	b.auth.TachyonAuthToken = nil
	b.auth.WebEncryptionKey = nil
	b.auth.RefreshKey = nil
	b.auth.RequestCrypto = nil
	b.auth.Browser = nil
	b.lastSessions = nil
}

// --- error translation ------------------------------------------------------

// translateError maps a libgm error onto the gm package's own sentinel, so
// that Classify never has to import libgm and the fake can raise exactly the
// same values with no phone.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, libgm.ErrPhoneNotResponding):
		return fmt.Errorf("%w: %w", ErrPhoneNotResponding, err)
	case errors.Is(err, libgm.ErrConnectionClosed):
		return fmt.Errorf("%w: %w", ErrConnectionClosed, err)
	case errors.Is(err, events.ErrInvalidCredentials):
		return fmt.Errorf("%w: %w", ErrInvalidCredentials, err)
	case errors.Is(err, events.ErrRequestedEntityNotFound):
		return fmt.Errorf("%w: %w", ErrRequestedEntityNotFound, err)
	case errors.Is(err, events.ErrCallerNoPermission):
		return fmt.Errorf("%w: %w", ErrCallerNoPermission, err)
	case errors.Is(err, libgm.ErrNoCookies):
		return fmt.Errorf("%w: %w", ErrNoCookies, err)
	case errors.Is(err, libgm.ErrNoDevicesFound):
		return fmt.Errorf("%w: %w", ErrNoDevicesFound, err)
	case errors.Is(err, libgm.ErrIncorrectEmoji):
		return fmt.Errorf("%w: %w", ErrIncorrectEmoji, err)
	case errors.Is(err, libgm.ErrPairingCancelled):
		return fmt.Errorf("%w: %w", ErrPairingCancelled, err)
	case errors.Is(err, libgm.ErrPairingTimeout):
		return fmt.Errorf("%w: %w", ErrPairingTimeout, err)
	case errors.Is(err, libgm.ErrPairingInitTimeout):
		if errors.Is(err, libgm.ErrHadMultipleDevices) {
			return fmt.Errorf("%w (%w): %w", ErrPairingInitTimeout, ErrHadMultipleDevices, err)
		}
		return fmt.Errorf("%w: %w", ErrPairingInitTimeout, err)
	}

	// events.RequestError.Is compares Type and Message only, so errors.Is
	// against the three sentinels above is reliable and is checked first
	// (spec section 3.5).
	var re events.RequestError
	if errors.As(err, &re) {
		return RequestError{Type: re.Data.GetType(), Message: re.Data.GetMessage()}
	}
	var he events.HTTPError
	if errors.As(err, &he) {
		status := 0
		if he.Resp != nil {
			status = he.Resp.StatusCode
		}
		return HTTPError{Action: he.Action, StatusCode: status}
	}
	return err
}
