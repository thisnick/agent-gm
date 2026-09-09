package gm

import "time"

// Event is one thing that happened on one account's stream. Every event
// below is one account's event, on one account's client; there is one handler
// per account and no event from one account ever changes another's state
// (spec section 3.4).
type Event any

// --- connection lifecycle (pkg/libgm/events/ready.go) -----------------------

// EventClientReady means the long poll is established, with the initial
// conversation list.
type EventClientReady struct {
	SessionID     string
	Conversations []Conversation
}

// EventAuthTokenRefreshed means the tachyon token rotated; persist AuthData.
type EventAuthTokenRefreshed struct{}

// EventListenTemporaryError means the poll dropped and will retry.
type EventListenTemporaryError struct{ Err error }

// EventListenRecovered means the poll is back.
type EventListenRecovered struct{}

// EventListenFatalError means the poll cannot continue. CredentialsDead is
// the result of the matching rule in spec section 3.4 -- an HTTP 401 or 403,
// or ErrInvalidCredentials -- computed once at this boundary so that no
// consumer is tempted to match on the error string.
type EventListenFatalError struct {
	Err             error
	CredentialsDead bool
}

// EventPingFailed means a ditto ping failed. EntityNotFound means the phone no
// longer knows this pairing. Upstream deliberately ignores the first failure,
// so ErrorCount matters.
type EventPingFailed struct {
	Err            error
	ErrorCount     int
	EntityNotFound bool
}

// EventPhoneNotResponding is emitted on the first unanswered ping, or after
// alertTimeoutCount (default 4) missed pings in a row. It is not a single
// fixed threshold (spec section 3.4).
type EventPhoneNotResponding struct{}

// EventPhoneRespondingAgain means the phone came back.
type EventPhoneRespondingAgain struct{}

// EventNoDataReceived means nothing arrived for dataReceiveCheckInterval
// (default 2h55m). It triggers a reconciliation sweep.
type EventNoDataReceived struct{}

// EventHackySetActiveMayFail means the skip count was non-zero at connect;
// re-issue SetActiveSession after a delay.
type EventHackySetActiveMayFail struct{}

// --- pairing (pkg/libgm/pair.go, pkg/libgm/pair_google.go) ------------------

// EventPairSuccessful means pairing completed. events.PairSuccessful.QRData
// is always nil on the gaia path -- DoGaiaPairing constructs the event with
// PhoneID and nothing else (pair_google.go:310) -- so the field is not
// carried here at all and cannot be dereferenced.
type EventPairSuccessful struct {
	PhoneID string
}

// EventRevokePairData means the phone revoked this pairing: set this account
// signed_out. It deletes nothing (spec section 4.7).
type EventRevokePairData struct{}

// EventGaiaLoggedOut means the Google cookies are dead: set this account
// signed_out and offer a cookie refresh. It deletes nothing.
type EventGaiaLoggedOut struct{}

// EventAccountChange means the phone's active Google account changed.
// IsFake=true means it was synthesised at startup from EncryptedData2, not a
// real change (event_handler.go:105-118).
type EventAccountChange struct {
	Account string
	IsFake  bool
}

// --- data (pkg/libgm/event_handler.go handleUpdatesEvent) -------------------

// EventMessage carries one message from the live stream. IsOld means it was
// replayed from the server's backlog after a reconnect.
type EventMessage struct {
	Message Message
	IsOld   bool
}

// EventConversation carries one conversation upsert.
type EventConversation struct {
	Conversation Conversation
}

// EventUserAlert carries one gmproto.UserAlertEvent.
type EventUserAlert struct {
	Alert AlertType
}

// EventSettings carries the phone's settings: SIM list, RCS enablement and
// the default-SMS-app flag.
type EventSettings struct {
	PushEnabled     *bool
	IsDefaultSMSApp bool
	RCSEnabled      bool
	SIMCount        int
}

// EventTyping is ephemeral: it is surfaced as peer_typing_until and never
// persisted.
type EventTyping struct {
	ConversationID string
	Number         string
	Until          time.Time
}

// EventUnknown is anything the catalogue above does not name. It is logged at
// debug and dropped, and it increments the account's unknown_events counter.
type EventUnknown struct {
	TypeName string
}

// --- alerts -----------------------------------------------------------------

// AlertType is gmproto.AlertType, which has 28 values (0-27) at this pin.
type AlertType int32

const (
	AlertUnknown                       AlertType = 0
	AlertBrowserInactive               AlertType = 1
	AlertBrowserActive                 AlertType = 2
	AlertMobileDataConnection          AlertType = 3
	AlertMobileWifiConnection          AlertType = 4
	AlertMobileBatteryLow              AlertType = 5
	AlertMobileBatteryRestored         AlertType = 6
	AlertBrowserInactiveFromTimeout    AlertType = 7
	AlertBrowserInactiveFromInactivity AlertType = 8
	AlertRCSConnection                 AlertType = 9
	AlertObserverRegistered            AlertType = 10
	AlertMobileDatabaseSyncing         AlertType = 11
	AlertMobileDatabaseSyncComplete    AlertType = 12
	AlertMobileDatabaseSyncStarted     AlertType = 13
)

// AlertTypeCount is the number of values gmproto.AlertType declares at the
// pin. The fixture-validation job asserts it is still 28.
const AlertTypeCount = 28

// TriggersResync reports whether an alert means "our session changed,
// resync". Only BROWSER_ACTIVE does; it does not mean another device took
// over (spec section 3.4).
func (a AlertType) TriggersResync() bool { return a == AlertBrowserActive }

// PausesBackfill reports whether an alert means the phone is resyncing its
// own database, so this account's backfill must be deferred.
func (a AlertType) PausesBackfill() bool {
	return a == AlertMobileDatabaseSyncStarted || a == AlertMobileDatabaseSyncing
}

// MarksSessionIdle reports whether an alert means this session went idle.
func (a AlertType) MarksSessionIdle() bool {
	return a == AlertBrowserInactive ||
		a == AlertBrowserInactiveFromTimeout ||
		a == AlertBrowserInactiveFromInactivity
}
