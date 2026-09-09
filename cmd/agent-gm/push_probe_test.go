package main

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPushReceiverAuthenticatesBeforeWake(t *testing.T) {
	decode := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	private := decode("q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94")
	auth := decode("BTBZMqHH6r4Tts7J_aSIgg")
	body := append(decode("DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"), decode("8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ")...)
	wake := make(chan struct{}, 1)
	h := probePushHandler(private, auth, wake)
	for _, tc := range []struct {
		body     []byte
		encoding string
		status   int
	}{
		{body, "", 415},
		{[]byte("forged"), "aes128gcm", 400},
		{make([]byte, 65537), "aes128gcm", 400},
		{body, "aes128gcm", 201},
		{body, "aes128gcm", 201},
	} {
		r := httptest.NewRequest(http.MethodPost, "/push/test", bytes.NewReader(tc.body))
		r.Header.Set("Content-Encoding", tc.encoding)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != tc.status {
			t.Fatalf("got %d want %d", w.Code, tc.status)
		}
		if tc.status != 201 && len(wake) != 0 {
			t.Fatal("unauthenticated request woke client")
		}
	}
	if len(wake) != 1 {
		t.Fatal("valid pushes were not coalesced into one wake")
	}
}
