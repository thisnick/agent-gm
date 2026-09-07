package mcp

// The `instructions` block of spec section 8.3, returned from `initialize`.
//
// It is written for an agent that has never seen this server, has no memory of
// prior calls, and cannot ask a human. Everything a cold model needs in order
// not to do harm is in it: that this is one real person's phone, that a send
// is irreversible, how accounts work, what a `client_request_id` is for, and
// what a refusal looks like.
//
// It is reproduced in `docs/mcp.md` under "First five minutes", and section 16
// Slice 3 test 20 asserts the two are byte-identical apart from the markdown
// quoting -- so a client that surfaces instructions to its model has already
// told it that page, and a fix to one cannot silently leave the other behind.
//
// It is a single constant rather than an assembled string because it is a
// document, and a document assembled from parts is a document nobody can diff.
const Instructions = "**This server is one person's Google Messages — possibly more than one\n" +
	"account of it.** Each account is paired directly with an Android phone, the\n" +
	"way the Google Messages web client pairs. Everything you can see here, they\n" +
	"can see in the Messages app on that phone, and everything you send leaves as\n" +
	"a real SMS, MMS or RCS message to a real person. There is no sandbox and no\n" +
	"undo.\n" +
	"\n" +
	"**Accounts.** Call `list_accounts` first if you do not already know which\n" +
	"account you are working in. Each has an ID starting `acct_`, the Google\n" +
	"address it belongs to, a label the owner chose, and a `state`. **If there is\n" +
	"exactly one account you can leave `account_id` out of every call and it will\n" +
	"be used.** If there is more than one, reads without `account_id` cover all of\n" +
	"them, but **a write must name one** — omit it and you get `invalid_request`\n" +
	"listing the accounts to choose from, which you can retry against\n" +
	"immediately. A `conv_` or `msg_` ID already belongs to one account, so you\n" +
	"never need to pass both.\n" +
	"\n" +
	"An account whose `state` is not `connected` is still fully readable — its\n" +
	"history is here — but writes to it are refused with\n" +
	"`unsupported_capability` and `reason: \"not_signed_in\"`. That means the owner\n" +
	"signed it out or its credentials expired; only they can fix it, and no\n" +
	"amount of retrying will.\n" +
	"\n" +
	"**The thing you address is a conversation.** A conversation is a Google\n" +
	"Messages thread: one other person, or a group. It has an ID that starts\n" +
	"`conv_`. Messages in it have IDs that start `msg_`. Every ID here is an\n" +
	"Agent GM ID with a typed prefix — `conv_` conversations, `msg_` messages,\n" +
	"`att_` attachments, `react_` reactions, `part_` people in a thread,\n" +
	"`contact_` contacts, `upl_` uploads, `op_` operations. Google's own IDs are\n" +
	"not accepted in their place.\n" +
	"\n" +
	"Words mean what they mean in Google Messages. A *conversation* is a thread.\n" +
	"A *contact* is somebody in the phone's contact list. *RCS* is the modern\n" +
	"protocol; *SMS/MMS* is the fallback. *Delete* means delete from this\n" +
	"account only — the other person keeps their copy, always.\n" +
	"\n" +
	"1. **Find the conversation.** `list_conversations` with `participant` set\n" +
	"   to a phone number (`+15105550123`, the bare digits, or a national form)\n" +
	"   returns the threads that number is in, newest activity first. Add\n" +
	"   `account_id` to look in one account, or leave it out to look in all of\n" +
	"   them — results carry `account_id` either way. With only a name, use\n" +
	"   `query`, a substring match over the thread name and every participant's\n" +
	"   name and number. `sender: \"me\"` means the owner in whichever account a\n" +
	"   message belongs to, so it works across accounts as well as within one.\n" +
	"   `list_contacts` maps names to\n" +
	"   numbers.\n" +
	"2. **Read it.** `list_messages` with that `conversation_id`. Newest first,\n" +
	"   so the first item is the latest message. Pass the result's `next_cursor`\n" +
	"   back as `cursor` for the next page; page size defaults to 50 and caps at\n" +
	"   100. `search_messages` needs `q`; it searches **every** account unless\n" +
	"   you pass `account_id`.\n" +
	"3. **Reply.** `send_message` with the `conversation_id`, `text`, and a\n" +
	"   `client_request_id` you invent. Add `reply_to_message_id` to thread a\n" +
	"   reply — **but replies are an RCS feature; on an `sms_mms` conversation\n" +
	"   that argument is refused with `unsupported_capability` and\n" +
	"   `reason: \"reply_not_supported\"`.** Check the conversation's `type` first,\n" +
	"   or read `capabilities.reply`.\n" +
	"4. **Know whether it arrived.** The result carries a `message_id` and an\n" +
	"   `operation`. Then watch the message's `delivery.state`, which walks\n" +
	"   `sending → sent → delivered → read`. **On SMS it usually stops\n" +
	"   at `sent`, and on group threads it usually stops at `sent`. Delivery and\n" +
	"   read receipts are an RCS feature and a carrier feature; waiting for\n" +
	"   `delivered` on an SMS thread can wait forever.** `sent` means the\n" +
	"   carrier took it, and that is as much as SMS will ever tell you.\n" +
	"5. **Send a photo or a file.** `create_upload` with the filename, mime type\n" +
	"   and byte length; run the `curl` command it returns, with `FILE` replaced\n" +
	"   by the path; then `send_message` with `upload_ids: [\"upl_…\"]` and\n" +
	"   optionally `text` as a caption. An upload is **not** tied to an account —\n" +
	"   the conversation you send it into decides that — but it can be sent only\n" +
	"   once. You cannot attach a file through this protocol any other way, and\n" +
	"   base64 through the model is not acceptable.\n" +
	"6. **Start a new thread.** `start_conversation` with `recipients` as E.164\n" +
	"   phone numbers. One recipient is a direct chat; two or more is a group,\n" +
	"   and `name` is only accepted for a group. **If a thread with exactly\n" +
	"   those recipients already exists you get that thread back and nothing is\n" +
	"   sent** — starting is safe, sending is not.\n" +
	"7. **React, or take something back.** `add_reaction` with `emoji` set to a\n" +
	"   bare emoji — Google offers eleven (👍 😍 😂 😮 😥 😠 👎 🤔 😢 😡 ❤️) and\n" +
	"   anything else is sent as a custom reaction that may not render on the\n" +
	"   recipient's phone. One reaction per person per message: adding a second\n" +
	"   replaces the first. `remove_reaction` takes the same `emoji`, or the\n" +
	"   `reaction_id` you read. `delete_message` and\n" +
	"   `delete_conversation` delete from **this account only** — the recipient\n" +
	"   keeps their copy. There is no delete-for-everyone and no mode to choose.\n" +
	"\n" +
	"**Every write needs a `client_request_id` that you invent.** Repeating a\n" +
	"call with the same one returns the same operation and sends nothing\n" +
	"further. A **fresh** `client_request_id` is a different call, not a repeat\n" +
	"— reusing this to \"retry\" is how a person gets the same text twice.\n" +
	"**Changing `account_id` while keeping the same `client_request_id` is also a\n" +
	"different call**, not a retry: it would send a second real message from the\n" +
	"other account. The server refuses that combination with `invalid_request`\n" +
	"rather than obeying it, so if a send fails, retry it **against the same\n" +
	"account** with the same key, or use a new key. If a\n" +
	"send times out with `phone_not_responding`, the operation is `pending`, not\n" +
	"failed: the server accepted it and the phone may still send it when it\n" +
	"wakes. **Poll `get_operation`; do not resend.**\n" +
	"\n" +
	"The phone has to be awake and online for anything to happen.\n" +
	"`list_accounts` and `get_session` tell you whether it is: `state` and\n" +
	"`phone_responding`, per account. `get_health` tells\n" +
	"you whether the index is complete (`backfill`) and whether the two things\n" +
	"that break sending are right (`is_default_sms_app`,\n" +
	"`config_version_stale`). If `state` is not `connected`, reads still work\n" +
	"from the local index but writes will fail, and only the owner can fix it.\n" +
	"\n" +
	"A call that is refused comes back as an ordinary result with\n" +
	"`isError: true` and\n" +
	"`structuredContent.error = {code, message, retryable, details}`. Read it\n" +
	"and correct the call rather than repeating it. `not_found` means no such\n" +
	"object. `invalid_request` names the parameter you got wrong in\n" +
	"`details.parameter` or `details.field` — this server refuses a misspelled\n" +
	"filter rather than silently ignoring it. `unsupported_capability` means the\n" +
	"action cannot apply here and `details.reason` says why.\n" +
	"`phone_not_responding` and `rate_limited` are retryable; almost nothing\n" +
	"else is.\n" +
	"\n" +
	"Scopes: `messages:read` gives you `list_accounts`, `list_conversations`,\n" +
	"`get_conversation`, `list_messages`, `get_message`, `message_context`,\n" +
	"`search_messages`, `get_attachment`, `list_contacts`, `get_session` and\n" +
	"`get_health`. A scope covers **every** account this server holds; there is\n" +
	"no per-account permission.\n" +
	"`messages:write` adds `send_message`, `start_conversation`, `mark_read`,\n" +
	"`add_reaction`, `remove_reaction`, `update_conversation`, `create_upload`\n" +
	"and `get_operation`. `messages:delete` adds `delete_message` and\n" +
	"`delete_conversation`. You only see the tools your token allows.\n"
