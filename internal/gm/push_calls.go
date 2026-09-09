package gm

import "context"

// Protocol calls share one passive listener at a time in push mode.

func (b *LibGM) FetchConfig(ctx context.Context) (ConfigInfo, error) {
	var r0 ConfigInfo
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawFetchConfig(ctx)
		return callErr
	})
	return r0, err
}

func (b *LibGM) IsDefaultSMSApp(ctx context.Context) (bool, error) {
	var r0 bool
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawIsDefaultSMSApp(ctx)
		return callErr
	})
	return r0, err
}

func (b *LibGM) ListConversations(ctx context.Context, folder Folder, count int) ([]Conversation, error) {
	var r0 []Conversation
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawListConversations(ctx, folder, count)
		return callErr
	})
	return r0, err
}

func (b *LibGM) GetConversation(ctx context.Context, convID string) (*Conversation, error) {
	var r0 *Conversation
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawGetConversation(ctx, convID)
		return callErr
	})
	return r0, err
}

func (b *LibGM) GetConversationType(ctx context.Context, convID string) (ConversationType, error) {
	var r0 ConversationType
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawGetConversationType(ctx, convID)
		return callErr
	})
	return r0, err
}

func (b *LibGM) ListMessages(ctx context.Context, convID string, count int, cursor *Cursor) ([]Message, *Cursor, error) {
	var r0 []Message
	var r1 *Cursor
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, r1, callErr = b.rawListMessages(ctx, convID, count, cursor)
		return callErr
	})
	return r0, r1, err
}

func (b *LibGM) ListContacts(ctx context.Context) ([]Contact, error) {
	var r0 []Contact
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawListContacts(ctx)
		return callErr
	})
	return r0, err
}

func (b *LibGM) ListTopContacts(ctx context.Context) ([]Contact, error) {
	var r0 []Contact
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawListTopContacts(ctx)
		return callErr
	})
	return r0, err
}

func (b *LibGM) ContactAvatars(ctx context.Context, ids []string) (map[string][]byte, error) {
	var r0 map[string][]byte
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawContactAvatars(ctx, ids)
		return callErr
	})
	return r0, err
}

func (b *LibGM) DownloadAvatar(ctx context.Context, url string) ([]byte, error) {
	var r0 []byte
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawDownloadAvatar(ctx, url)
		return callErr
	})
	return r0, err
}

func (b *LibGM) ResolveConversation(ctx context.Context, numbers []string, groupName string) (ResolveResult, error) {
	var r0 ResolveResult
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawResolveConversation(ctx, numbers, groupName)
		return callErr
	})
	return r0, err
}

func (b *LibGM) SendText(ctx context.Context, req SendTextRequest) (SendResult, error) {
	var r0 SendResult
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawSendText(ctx, req)
		return callErr
	})
	return r0, err
}

func (b *LibGM) SendMedia(ctx context.Context, req SendMediaRequest) (SendResult, error) {
	var r0 SendResult
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawSendMedia(ctx, req)
		return callErr
	})
	return r0, err
}

func (b *LibGM) React(ctx context.Context, msgID string, emoji string, action ReactionAction) error {
	return b.withPush(ctx, func(ctx context.Context) error { return b.rawReact(ctx, msgID, emoji, action) })
}

func (b *LibGM) DeleteMessage(ctx context.Context, msgID string) error {
	return b.withPush(ctx, func(ctx context.Context) error { return b.rawDeleteMessage(ctx, msgID) })
}

func (b *LibGM) MarkRead(ctx context.Context, convID, msgID string) error {
	return b.withPush(ctx, func(ctx context.Context) error { return b.rawMarkRead(ctx, convID, msgID) })
}

func (b *LibGM) SetTyping(ctx context.Context, convID string) error {
	return b.withPush(ctx, func(ctx context.Context) error { return b.rawSetTyping(ctx, convID) })
}

func (b *LibGM) UpdateConversation(ctx context.Context, convID string, ch ConversationChange) error {
	return b.withPush(ctx, func(ctx context.Context) error { return b.rawUpdateConversation(ctx, convID, ch) })
}

func (b *LibGM) DeleteConversation(ctx context.Context, convID, phone string) error {
	return b.withPush(ctx, func(ctx context.Context) error { return b.rawDeleteConversation(ctx, convID, phone) })
}

func (b *LibGM) Upload(ctx context.Context, data []byte, filename, mime string) (MediaRef, error) {
	var r0 MediaRef
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawUpload(ctx, data, filename, mime)
		return callErr
	})
	return r0, err
}

func (b *LibGM) Download(ctx context.Context, mediaID string, key []byte) ([]byte, error) {
	var r0 []byte
	err := b.withPush(ctx, func(ctx context.Context) error {
		var callErr error
		r0, callErr = b.rawDownload(ctx, mediaID, key)
		return callErr
	})
	return r0, err
}

func (b *LibGM) RequestFullSizeImage(ctx context.Context, msgID, actionMsgID string) error {
	return b.withPush(ctx, func(ctx context.Context) error { return b.rawRequestFullSizeImage(ctx, msgID, actionMsgID) })
}
