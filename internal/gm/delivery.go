package gm

// DeliveryState is Agent GM's own closed vocabulary for what Google said
// about a message (spec section 4.4). The mapping below covers every value
// declared in MessageStatusType at the pin. A value the mapping does not name
// fails the table test in section 13.2 rather than falling silently to
// unknown.
type DeliveryState string

const (
	DeliveryStateSending        DeliveryState = "sending"
	DeliveryStateSent           DeliveryState = "sent"
	DeliveryStateDelivered      DeliveryState = "delivered"
	DeliveryStateRead           DeliveryState = "read"
	DeliveryStateFailed         DeliveryState = "failed"
	DeliveryStateCanceled       DeliveryState = "canceled"
	DeliveryStateDeleted        DeliveryState = "deleted"
	DeliveryStateReceived       DeliveryState = "received"
	DeliveryStateDownloading    DeliveryState = "downloading"
	DeliveryStateDownloadFailed DeliveryState = "download_failed"
	DeliveryStateUnknown        DeliveryState = "unknown"
)

// SystemEventStatusMin and SystemEventStatusMax bound the in-thread system
// event band. Values 200-279 are stored with kind='system' and carry
// delivery_state='received' for schema uniformity (spec section 4.4).
const (
	SystemEventStatusMin int32 = 200
	SystemEventStatusMax int32 = 279
)

// MessageDeletedStatus is MESSAGE_DELETED, which sits outside every other
// range (spec section 3.7).
const MessageDeletedStatus int32 = 300

// deliveryStates maps every MessageStatusType value outside 200-279 that is
// declared at the pin. Each entry cites its proto name so a reviewer can
// check it against pkg/libgm/gmproto/conversations.proto:295-427.
var deliveryStates = map[int32]DeliveryState{
	0: DeliveryStateUnknown, // STATUS_UNKNOWN

	// sending -- accepted locally or in flight
	3:  DeliveryStateSending, // OUTGOING_DRAFT
	4:  DeliveryStateSending, // OUTGOING_YET_TO_SEND
	5:  DeliveryStateSending, // OUTGOING_SENDING
	6:  DeliveryStateSending, // OUTGOING_RESENDING
	7:  DeliveryStateSending, // OUTGOING_AWAITING_RETRY
	10: DeliveryStateSending, // OUTGOING_SEND_AFTER_PROCESSING
	16: DeliveryStateSending, // OUTGOING_SCHEDULED
	20: DeliveryStateSending, // OUTGOING_VALIDATING

	// sent -- the carrier took it
	1:  DeliveryStateSent, // OUTGOING_COMPLETE
	14: DeliveryStateSent, // OUTGOING_NOT_DELIVERED_YET

	// delivered / read
	2:  DeliveryStateDelivered, // OUTGOING_DELIVERED
	11: DeliveryStateRead,      // OUTGOING_DISPLAYED

	// failed -- terminal
	8:  DeliveryStateFailed, // OUTGOING_FAILED_GENERIC
	9:  DeliveryStateFailed, // OUTGOING_FAILED_EMERGENCY_NUMBER
	13: DeliveryStateFailed, // OUTGOING_FAILED_TOO_LARGE
	17: DeliveryStateFailed, // OUTGOING_FAILED_RECIPIENT_LOST_RCS
	18: DeliveryStateFailed, // OUTGOING_FAILED_NO_RETRY_NO_FALLBACK
	19: DeliveryStateFailed, // OUTGOING_FAILED_RECIPIENT_DID_NOT_DECRYPT
	21: DeliveryStateFailed, // OUTGOING_FAILED_RECIPIENT_LOST_ENCRYPTION
	22: DeliveryStateFailed, // OUTGOING_FAILED_RECIPIENT_DID_NOT_DECRYPT_NO_MORE_RETRY
	24: DeliveryStateFailed, // OUTGOING_FAILED_RECIPIENT_NEGATIVE_DELIVERY
	25: DeliveryStateFailed, // MESSAGE_STATUS_OUTGOING_FAILED_EMERGENCY_PROTOCOL_DETERMINATION_MESSAGE
	26: DeliveryStateFailed, // OUTGOING_RESTRICTED
	27: DeliveryStateFailed, // OUTGOING_FAILED_TO_ENCRYPT

	// canceled -- withdrawn before sending
	12: DeliveryStateCanceled, // OUTGOING_CANCELED
	15: DeliveryStateCanceled, // OUTGOING_REVOCATION_PENDING

	// deleted
	23:  DeliveryStateDeleted, // OUTGOING_DELETED
	117: DeliveryStateDeleted, // INCOMING_DELETED
	300: DeliveryStateDeleted, // MESSAGE_DELETED

	// received -- complete, or content Agent GM will never get
	100: DeliveryStateReceived, // INCOMING_COMPLETE
	108: DeliveryStateReceived, // INCOMING_DELIVERED
	109: DeliveryStateReceived, // INCOMING_DISPLAYED
	116: DeliveryStateReceived, // INCOMING_UNKNOWN_CONTENT_TYPE

	// downloading -- incoming media not yet fetched
	101: DeliveryStateDownloading, // INCOMING_YET_TO_MANUAL_DOWNLOAD
	102: DeliveryStateDownloading, // INCOMING_RETRYING_MANUAL_DOWNLOAD
	103: DeliveryStateDownloading, // INCOMING_MANUAL_DOWNLOADING
	104: DeliveryStateDownloading, // INCOMING_RETRYING_AUTO_DOWNLOAD
	105: DeliveryStateDownloading, // INCOMING_AUTO_DOWNLOADING
	115: DeliveryStateDownloading, // INCOMING_AWAITING_AUTO_DOWNLOAD

	// download_failed -- incoming media unavailable
	106: DeliveryStateDownloadFailed, // INCOMING_DOWNLOAD_FAILED
	107: DeliveryStateDownloadFailed, // INCOMING_EXPIRED_OR_NOT_AVAILABLE
	110: DeliveryStateDownloadFailed, // INCOMING_DOWNLOAD_CANCELED
	111: DeliveryStateDownloadFailed, // INCOMING_DOWNLOAD_FAILED_TOO_LARGE
	112: DeliveryStateDownloadFailed, // INCOMING_DOWNLOAD_FAILED_SIM_HAS_NO_DATA
	113: DeliveryStateDownloadFailed, // INCOMING_FAILED_TO_DECRYPT
	114: DeliveryStateDownloadFailed, // INCOMING_DECRYPTION_ABORTED
	118: DeliveryStateDownloadFailed, // INCOMING_DOWNLOAD_RESTRICTED
}

// IsSystemEventStatus reports whether a raw status is an in-thread system
// event (spec section 3.7: values 200-279).
func IsSystemEventStatus(raw int32) bool {
	return raw >= SystemEventStatusMin && raw <= SystemEventStatusMax
}

// KindForStatus classifies a raw MessageStatusType into a message kind.
func KindForStatus(raw int32) MessageKind {
	if IsSystemEventStatus(raw) {
		return MessageKindSystem
	}
	return MessageKindMessage
}

// DeliveryStateFor maps a raw MessageStatusType onto Agent GM's vocabulary.
// The second return value is false when the value is not mapped -- which for
// a value declared at the pin is a bug the section 13.2 table test catches,
// and for a value added upstream after the pin means `unknown`.
func DeliveryStateFor(raw int32) (DeliveryState, bool) {
	if IsSystemEventStatus(raw) {
		// System events carry `received` for schema uniformity; they are not
		// a delivery state and are excluded from messages.list unless
		// include_system=true.
		return DeliveryStateReceived, true
	}
	st, ok := deliveryStates[raw]
	if !ok {
		return DeliveryStateUnknown, false
	}
	return st, true
}

// MappedStatusValues returns every raw status the mapping names, excluding
// the 200-279 band. Tests use it to assert the mapping and the pinned proto
// enumerate the same set.
func MappedStatusValues() []int32 {
	out := make([]int32, 0, len(deliveryStates))
	for k := range deliveryStates {
		out = append(out, k)
	}
	return out
}

// deliveryRank orders the states a message moves through. A move to a higher
// rank is accepted, including a forward skip; a move to a lower rank is
// refused (spec section 4.4).
var deliveryRank = map[DeliveryState]int{
	DeliveryStateSending:   1,
	DeliveryStateSent:      2,
	DeliveryStateDelivered: 3,
	DeliveryStateRead:      4,
}

// TransitionAllowed reports whether an outgoing message may move from `from`
// to `to`, per the transition table of spec section 4.4:
//
//	sending -> sent -> delivered -> read
//	sending|sent      -> failed
//	sending           -> canceled
//	any               -> deleted
//	unknown           -> any
//
// A forward skip is accepted; a backward move is refused.
func TransitionAllowed(from, to DeliveryState) bool {
	if from == to {
		return true
	}
	if to == DeliveryStateDeleted {
		return true // any -> deleted
	}
	if from == DeliveryStateUnknown {
		return true // a late authoritative status corrects it
	}
	if from == DeliveryStateDeleted {
		return false
	}
	switch to {
	case DeliveryStateFailed:
		return from == DeliveryStateSending || from == DeliveryStateSent
	case DeliveryStateCanceled:
		return from == DeliveryStateSending
	case DeliveryStateSending, DeliveryStateSent, DeliveryStateDelivered, DeliveryStateRead:
		fr, okFrom := deliveryRank[from]
		tr, okTo := deliveryRank[to]
		if !okFrom || !okTo {
			return false
		}
		return tr > fr
	default:
		// Incoming-only states are not part of the outgoing ladder. They are
		// only ever reached from unknown or from themselves, both handled
		// above.
		return false
	}
}

// IsOutgoingLadderState reports whether a state participates in the outgoing
// transition ladder of spec section 4.4.
func IsOutgoingLadderState(s DeliveryState) bool {
	switch s {
	case DeliveryStateSending, DeliveryStateSent, DeliveryStateDelivered,
		DeliveryStateRead, DeliveryStateFailed, DeliveryStateCanceled,
		DeliveryStateDeleted, DeliveryStateUnknown:
		return true
	default:
		return false
	}
}

// ignoreInDM is the set of statuses upstream ignores in a direct
// conversation, carried over verbatim from
// connector/handlegmessages.go:913-940 (shouldIgnoreStatus).
var ignoreInDM = map[int32]struct{}{
	214: {}, // TOMBSTONE_PROTOCOL_SWITCH_TO_TEXT
	215: {}, // TOMBSTONE_PROTOCOL_SWITCH_TO_RCS
	216: {}, // TOMBSTONE_PROTOCOL_SWITCH_TO_ENCRYPTED_RCS
	219: {}, // TOMBSTONE_PROTOCOL_SWITCH_TO_ENCRYPTED_RCS_INFO
	206: {}, // TOMBSTONE_ONE_ON_ONE_SMS_CREATED
	207: {}, // TOMBSTONE_ONE_ON_ONE_RCS_CREATED
	213: {}, // TOMBSTONE_ENCRYPTED_ONE_ON_ONE_RCS_CREATED
	235: {}, // MESSAGE_STATUS_TOMBSTONE_PROTOCOL_SWITCH_TEXT_TO_E2EE
	236: {}, // MESSAGE_STATUS_TOMBSTONE_PROTOCOL_SWITCH_E2EE_TO_TEXT
	237: {}, // MESSAGE_STATUS_TOMBSTONE_PROTOCOL_SWITCH_RCS_TO_E2EE
	238: {}, // MESSAGE_STATUS_TOMBSTONE_PROTOCOL_SWITCH_E2EE_TO_RCS
}

// ignoreAlways is the set upstream ignores outright, in a group as well as a
// direct conversation.
var ignoreAlways = map[int32]struct{}{
	229: {}, // MESSAGE_STATUS_TOMBSTONE_ENCRYPTED_GROUP_CREATED
	234: {}, // MESSAGE_STATUS_TOMBSTONE_GROUP_PROTOCOL_SWITCH_E2EE_TO_RCS
	233: {}, // MESSAGE_STATUS_TOMBSTONE_GROUP_PROTOCOL_SWITCH_RCS_TO_E2EE
	203: {}, // TOMBSTONE_RCS_GROUP_CREATED
	204: {}, // TOMBSTONE_MMS_GROUP_CREATED
	205: {}, // TOMBSTONE_SMS_BROADCAST_CREATED
	245: {}, // MESSAGE_STATUS_TOMBSTONE_PARTICIPANT_THEME_CHANGE
	210: {}, // TOMBSTONE_SHOW_LINK_PREVIEWS
	259: {}, // MESSAGE_STATUS_TOMBSTONE_ACTIVE_SELF_IDENTITY_CHANGED
}

// ShouldIgnoreStatus is spec section 5.3 step 1: the ingest path drops these
// and counts them. The set is carried over verbatim from upstream, and the
// fixture-validation job asserts the two still agree.
func ShouldIgnoreStatus(raw int32, isDM bool) bool {
	if _, ok := ignoreAlways[raw]; ok {
		return true
	}
	if _, ok := ignoreInDM[raw]; ok {
		return isDM
	}
	return false
}

// IgnoredStatuses returns the two ignore sets, for the fixture-validation
// job.
func IgnoredStatuses() (dmOnly []int32, always []int32) {
	for k := range ignoreInDM {
		dmOnly = append(dmOnly, k)
	}
	for k := range ignoreAlways {
		always = append(always, k)
	}
	return dmOnly, always
}
