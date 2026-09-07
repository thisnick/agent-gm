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

// The revocation sentences of spec section 9.5's admin routes.
//
// They are a SEPARATE group from Effects() above, and deliberately so.
// Effects() holds the three sentences spec sections 7.7 and 4.7 write out
// verbatim, and a test asserts each of them appears in the spec word for word;
// these three are the same KIND of thing -- one sentence shared by the route's
// `effect` field and the `agm` confirmation prompt, so a human cannot confirm
// different words from the ones the server acted on -- but the spec describes
// the routes rather than quoting a sentence, so there is nothing to match
// against and adding them to Effects() would make that test assert something
// it cannot check.
//
// Like the other three, none of them carries its own full stop: a surface that
// adds one would otherwise get two.
const (
	// EffectAuthorizationRevoke is DELETE /v1/admin/authorizations/{id}.
	EffectAuthorizationRevoke = "revokes every token of this authorization. The client's next call is a 401, and it cannot refresh its way back"

	// EffectEnrollmentCodeRevoke is DELETE /v1/admin/enrollment-codes/{id}.
	EffectEnrollmentCodeRevoke = "stops this enrollment code being redeemable. Repeating it is not an error"

	// EffectClientRevoke is DELETE /v1/admin/clients/{id}.
	EffectClientRevoke = "removes this registration and revokes every authorization it holds. The client must register again"
)

// RevocationEffects returns the three sentences of spec section 9.5's admin
// revocations, for a test that has to enumerate them.
func RevocationEffects() []string {
	return []string{EffectAuthorizationRevoke, EffectEnrollmentCodeRevoke, EffectClientRevoke}
}
