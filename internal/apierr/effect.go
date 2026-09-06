package apierr

// The effect sentences of spec sections 7.7 and 4.7, byte for byte.
//
// Each one is the `effect` field of the route's response, the closing
// sentence of the MCP tool description, and the text of the `agm`
// confirmation prompt. They are constants in this package so that those three
// surfaces are literally the same string: a model or a human must not be able
// to read a broader claim off one surface than another, and three hand-copied
// sentences drift the first time one is reworded.
//
// The sentences say what is NOT affected as well as what is, because "delete"
// on a messaging surface is ambiguous and the ambiguity is the dangerous part:
// an agent that believes it can unsend a message will act on that belief.
const (
	// EffectMessageDelete is DELETE /v1/messages/{message_id} (spec 7.7).
	EffectMessageDelete = "deletes this message from your Google Messages account only; the recipient keeps it"

	// EffectConversationDelete is DELETE /v1/conversations/{conversation_id}
	// (spec 7.7).
	EffectConversationDelete = "deletes this conversation from your Google Messages account only; the other people in it keep it"

	// EffectAccountRemove is DELETE /v1/accounts/{id} (spec 4.7). Account
	// removal is the ONLY thing in Agent GM that deletes stored rows.
	EffectAccountRemove = "permanently deletes Agent GM's copy of this account's conversations, messages, attachments and operations; your Google Messages account and the messages in it are untouched"
)

// Effects returns the three sentences, for a test or a documentation
// generator that has to enumerate them.
func Effects() []string {
	return []string{EffectMessageDelete, EffectConversationDelete, EffectAccountRemove}
}
