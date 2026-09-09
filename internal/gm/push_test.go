package gm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func pushFixture(t *testing.T) (PushSubscription, []byte) {
	t.Helper()
	d := func(s string) []byte {
		v, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	return PushSubscription{Private: d("q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"), Auth: d("BTBZMqHH6r4Tts7J_aSIgg"), Token: strings.Repeat("a", 64)}, d("4qwpMU-5mPnxve5M__3SgvQtbG9ryB0DXjf1-QmQeTh5JFJs0Q")
}

// Ciphertext independently produced with Python cryptography using the RFC's
// public test keys and the legacy Web Push derivation, not our Go encryptor.
func TestLegacyPushKnownAnswer(t *testing.T) {
	s, body := pushFixture(t)
	enc := "salt=DGv6ra1nlYgDCS1FRnbzlw"
	dh := "dh=BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	plain, err := DecryptLegacyWebPush(s.Private, s.Auth, body, enc, dh)
	if err != nil || string(plain) != "push legacy fixture" {
		t.Fatalf("known answer: %q %v", plain, err)
	}
	for i := range body {
		bad := append([]byte(nil), body...)
		bad[i] ^= 1
		if _, err = DecryptLegacyWebPush(s.Private, s.Auth, bad, enc, dh); err == nil {
			t.Fatalf("accepted corruption at %d", i)
		}
	}
	for _, header := range []string{"", enc + ";rs=1", enc + ";salt=duplicate"} {
		if _, err = DecryptLegacyWebPush(s.Private, s.Auth, body, header, dh); err == nil {
			t.Fatal("accepted malformed header")
		}
	}
}

func TestPushAcknowledgementRequiresDurableWake(t *testing.T) {
	s, body := pushFixture(t)
	saved, _ := json.Marshal(s)
	failSave := false
	b := New(zerolog.Nop())
	err := b.EnablePush("https://example.test/push/account", saved, func(data []byte) error {
		if failSave {
			return errors.New("disk full")
		}
		saved = append([]byte(nil), data...)
		return nil
	}, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	b.push.started.Store(true)
	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/push/account/"+token, bytes.NewReader(body))
		r.Header.Set("Content-Encoding", "aesgcm")
		r.Header.Set("Encryption", "salt=DGv6ra1nlYgDCS1FRnbzlw")
		r.Header.Set("Crypto-Key", "dh=BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
		w := httptest.NewRecorder()
		b.HandlePush(w, r, token)
		return w
	}
	if w := request("wrong"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	failSave = true
	if w := request(s.Token); w.Code != 503 || len(b.push.wake) != 0 || b.push.state.Generation != 0 {
		t.Fatal("acknowledged unsaved wake")
	}
	failSave = false
	for range 2 {
		if w := request(s.Token); w.Code != 201 {
			t.Fatal(w.Code)
		}
	}
	var restored PushSubscription
	if err = json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Generation != 2 || restored.Delivered != 0 || len(b.push.wake) != 1 {
		t.Fatal("pending wakes not persisted/coalesced")
	}
	b.push.started.Store(false)
	if w := request(s.Token); w.Code != 404 {
		t.Fatal("stopped client accepted push")
	}
}

func TestPushWorkerRetriesOnlyPendingWork(t *testing.T) {
	s, _ := pushFixture(t)
	s.Generation = 1
	saved, _ := json.Marshal(s)
	b := New(zerolog.Nop())
	if err := b.EnablePush("https://example.test/push/account", saved, func([]byte) error { return nil }, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	p := b.push
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.done = make(chan struct{})
	p.started.Store(true)
	var calls atomic.Int32
	p.run = func(context.Context, func(context.Context) error) error {
		if calls.Add(1) == 1 {
			return errors.New("transient failure")
		}
		return nil
	}
	go b.pushWorker()
	p.wake <- struct{}{}
	defer func() { p.cancel(); <-p.done }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		delivered := p.state.Delivered
		p.mu.Unlock()
		if delivered == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending push was not retried")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if calls.Load() != 2 {
		t.Fatalf("unexpected polling after delivery: %d calls", calls.Load())
	}
}
