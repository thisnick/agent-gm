package gm

import (
	"encoding/base64"
	"testing"
)

// Independent known-answer vector from RFC 8291 section 5 and appendix A.
func TestDecryptWebPushRFC8291(t *testing.T) {
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
	plain, err := DecryptWebPush(private, auth, body)
	if err != nil || string(plain) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("known answer failed: %q %v", plain, err)
	}
	for i := range body {
		bad := append([]byte(nil), body...)
		bad[i] ^= 0x80
		// The record-size header is not authenticated by RFC 8188.
		if i >= 16 && i < 20 {
			continue
		}
		if _, err := DecryptWebPush(private, auth, bad); err == nil {
			t.Fatalf("accepted corruption at %d", i)
		}
	}
	for n := 0; n < 103; n++ {
		if _, err := DecryptWebPush(private, auth, body[:n]); err == nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
	auth[0] ^= 1
	if _, err := DecryptWebPush(private, auth, body); err == nil {
		t.Fatal("accepted wrong secret")
	}
}
