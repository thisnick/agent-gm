package api_test

import (
	"errors"
	"testing"
	"time"

	"github.com/thisnick/agent-gm/internal/core"
	"github.com/thisnick/agent-gm/internal/gm"
)

func TestReadRoutesRefreshGoogleDataBeforeAnswering(t *testing.T) {
	s := newServer(t)
	account := s.addAccount(addressA)
	s.Deps.Freshness = &core.Freshener{}
	// Exists only on the phone; no push was delivered to the server.
	c := gm.Conversation{SourceID: "phone-only", Name: "New contact", Type: gm.ConversationTypeRCS, Folder: gm.FolderInbox, LastActivity: s.Clock.Now(), Participants: []gm.Participant{{SourceID: "peer", PhoneE164: fictionalA, DisplayName: "New contact"}}}
	s.backend(account).SeedConversation(c)
	e := s.call("GET", "/v1/conversations?participant="+fictionalA[1:], nil)
	if e.Error != nil || len(e.items()) != 1 {
		t.Fatalf("read did not discover phone conversation: %+v", e)
	}
	id := e.items()[0].(map[string]any)["id"].(string)
	s.backend(account).SeedMessage(gm.Message{SourceID: "phone-message", ConversationID: c.SourceID, Text: "fresh message", Timestamp: s.Clock.Now(), StatusRaw: 100})
	e = s.call("GET", "/v1/conversations/"+id+"/messages", nil)
	if e.Error != nil || len(e.items()) != 1 {
		t.Fatalf("thread read did not refresh: %+v", e)
	}
	s.Clock.Advance(61 * time.Second)
	s.backend(account).ScriptErrors(errors.New("refresh unavailable"))
	e = s.call("GET", "/v1/conversations", nil)
	if e.Error == nil {
		t.Fatal("silently returned stale data after refresh failure")
	}
}
