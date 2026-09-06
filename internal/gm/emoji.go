package gm

import "golang.org/x/text/unicode/norm"

// EmojiType is Google's closed 14-value reaction enum (spec section 3.7,
// gmproto/conversations.proto:70-85). Reactions are not free-form emoji:
// anything outside the eleven canonical values becomes CUSTOM and may not
// render on the recipient's phone.
type EmojiType string

const (
	EmojiTypeUnspecified EmojiType = "unspecified"
	EmojiTypeLike        EmojiType = "like"
	EmojiTypeLove        EmojiType = "love"
	EmojiTypeLaugh       EmojiType = "laugh"
	EmojiTypeSurprised   EmojiType = "surprised"
	EmojiTypeSad         EmojiType = "sad"
	EmojiTypeAngry       EmojiType = "angry"
	EmojiTypeDislike     EmojiType = "dislike"
	EmojiTypeCustom      EmojiType = "custom"
	EmojiTypeQuestioning EmojiType = "questioning"
	EmojiTypeCryingFace  EmojiType = "crying_face"
	EmojiTypePoutingFace EmojiType = "pouting_face"
	EmojiTypeRedHeart    EmojiType = "red_heart"
	EmojiTypeEmotify     EmojiType = "emotify"
)

// emojiTypeByRaw maps the proto's numeric values onto Agent GM's names.
var emojiTypeByRaw = map[int32]EmojiType{
	0:  EmojiTypeUnspecified,
	1:  EmojiTypeLike,
	2:  EmojiTypeLove,
	3:  EmojiTypeLaugh,
	4:  EmojiTypeSurprised,
	5:  EmojiTypeSad,
	6:  EmojiTypeAngry,
	7:  EmojiTypeDislike,
	8:  EmojiTypeCustom,
	9:  EmojiTypeQuestioning,
	10: EmojiTypeCryingFace,
	11: EmojiTypePoutingFace,
	12: EmojiTypeRedHeart,
	13: EmojiTypeEmotify,
}

var rawByEmojiType = func() map[EmojiType]int32 {
	out := make(map[EmojiType]int32, len(emojiTypeByRaw))
	for k, v := range emojiTypeByRaw {
		out[v] = k
	}
	return out
}()

// canonicalUnicode mirrors gmproto.EmojiType.Unicode() exactly. CUSTOM,
// EMOTIFY and REACTION_TYPE_UNSPECIFIED render as "" upstream, so they are
// absent here and CanonicalEmoji returns nil for them.
var canonicalUnicode = map[EmojiType]string{
	EmojiTypeLike:        "\U0001F44D",
	EmojiTypeLove:        "\U0001F60D",
	EmojiTypeLaugh:       "\U0001F602",
	EmojiTypeSurprised:   "\U0001F62E",
	EmojiTypeSad:         "\U0001F625",
	EmojiTypeAngry:       "\U0001F620",
	EmojiTypeDislike:     "\U0001F44E",
	EmojiTypeQuestioning: "\U0001F914",
	EmojiTypeCryingFace:  "\U0001F622",
	EmojiTypePoutingFace: "\U0001F621",
	// UnicodeToEmojiType accepts both "❤" and "❤️" as
	// RED_HEART, but Unicode() always renders the variation-selector form.
	EmojiTypeRedHeart: "❤️",
}

// EmojiTypeForRaw converts a raw proto value. An unrecognised value is
// reported as CUSTOM, which is what upstream's UnicodeToEmojiType default
// arm does for an unrecognised emoji.
func EmojiTypeForRaw(raw int32) (EmojiType, bool) {
	t, ok := emojiTypeByRaw[raw]
	if !ok {
		return EmojiTypeCustom, false
	}
	return t, true
}

// RawEmojiType converts back to the proto value.
func RawEmojiType(t EmojiType) (int32, bool) {
	raw, ok := rawByEmojiType[t]
	return raw, ok
}

// EmojiTypeForUnicode mirrors gmproto.UnicodeToEmojiType: anything that is
// not one of the eleven canonical code points is CUSTOM. The input is NFC
// normalised first so that the two spellings of a heart cannot produce two
// different reactions (spec section 3.7 consequence 1).
func EmojiTypeForUnicode(emoji string) EmojiType {
	e := norm.NFC.String(emoji)
	switch e {
	case "\U0001F44D":
		return EmojiTypeLike
	case "\U0001F60D":
		return EmojiTypeLove
	case "\U0001F602":
		return EmojiTypeLaugh
	case "\U0001F62E":
		return EmojiTypeSurprised
	case "\U0001F625":
		return EmojiTypeSad
	case "\U0001F620":
		return EmojiTypeAngry
	case "\U0001F44E":
		return EmojiTypeDislike
	case "\U0001F914":
		return EmojiTypeQuestioning
	case "\U0001F622":
		return EmojiTypeCryingFace
	case "\U0001F621":
		return EmojiTypePoutingFace
	case "❤", "❤️":
		return EmojiTypeRedHeart
	default:
		return EmojiTypeCustom
	}
}

// CanonicalEmoji is the value every surface uses and every react_ ID is
// derived from (spec section 4.1). For one of the eleven it is
// EmojiType.Unicode(); for CUSTOM it is the caller's own unicode after NFC
// normalisation; for a type with no unicode of its own it is nil.
func CanonicalEmoji(t EmojiType, raw string) *string {
	if u, ok := canonicalUnicode[t]; ok {
		return &u
	}
	if t == EmojiTypeCustom && raw != "" {
		n := norm.NFC.String(raw)
		return &n
	}
	return nil
}

// CanonicaliseEmojiInput takes whatever a caller sent and returns the type
// and the canonical emoji Agent GM will use. Every inbound emoji goes
// through here before a react_ ID is derived or a path segment is matched.
func CanonicaliseEmojiInput(input string) (EmojiType, *string) {
	t := EmojiTypeForUnicode(input)
	return t, CanonicalEmoji(t, input)
}
