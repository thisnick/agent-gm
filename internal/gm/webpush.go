package gm

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// DecryptWebPush validates one RFC 8291 aes128gcm record. Web Push requires
// a single record, unlike general RFC 8188 HTTP content encoding.
func DecryptWebPush(private, auth, body []byte) ([]byte, error) {
	invalid := errors.New("invalid encrypted push")
	if len(auth) != 16 || len(body) < 103 || len(body) > 65536 || body[20] != 65 {
		return nil, invalid
	}
	if uint64(binary.BigEndian.Uint32(body[16:20])) < uint64(len(body)-86) {
		return nil, invalid
	}
	key, err := ecdh.P256().NewPrivateKey(private)
	if err != nil {
		return nil, invalid
	}
	peer, err := ecdh.P256().NewPublicKey(body[21:86])
	if err != nil {
		return nil, invalid
	}
	shared, err := key.ECDH(peer)
	if err != nil {
		return nil, invalid
	}
	info := append([]byte("WebPush: info\x00"), key.PublicKey().Bytes()...)
	info = append(info, peer.Bytes()...)
	derive := func(secret, salt, info []byte, n int) []byte {
		out := make([]byte, n)
		// All derivations are below HKDF's output limit.
		_, _ = io.ReadFull(hkdf.New(sha256.New, secret, salt, info), out)
		return out
	}
	ikm := derive(shared, auth, info, 32)
	cek := derive(ikm, body[:16], []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := derive(ikm, body[:16], []byte("Content-Encoding: nonce\x00"), 12)
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, invalid
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, invalid
	}
	plain, err := gcm.Open(nil, nonce, body[86:], nil)
	if err != nil {
		return nil, invalid
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 2 {
		return nil, invalid
	}
	return plain[:len(plain)-1], nil
}

// DecryptLegacyWebPush supports the aesgcm draft format still emitted by
// Google Messages. Its salt and ECDH key are in HTTP headers, and padding
// is a two-byte length prefix rather than the RFC 8291 trailing delimiter.
func DecryptLegacyWebPush(private, auth, body []byte, encryption, cryptoKey string) ([]byte, error) {
	invalid := errors.New("invalid encrypted push")
	param := func(header, name string) (string, error) {
		var value string
		for _, part := range strings.FieldsFunc(header, func(r rune) bool { return r == ';' || r == ',' }) {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && k == name {
				if value != "" {
					return "", invalid
				}
				value = strings.Trim(v, "\"")
			}
		}
		return value, nil
	}
	saltText, err := param(encryption, "salt")
	if err != nil {
		return nil, invalid
	}
	dhText, err := param(cryptoKey, "dh")
	if err != nil {
		return nil, invalid
	}
	rsText, err := param(encryption, "rs")
	if err != nil {
		return nil, invalid
	}
	rs := uint64(4096)
	if rsText != "" {
		rs, err = strconv.ParseUint(rsText, 10, 32)
		if err != nil {
			return nil, invalid
		}
	}
	salt, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(saltText, "="))
	if err != nil {
		return nil, invalid
	}
	dh, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(dhText, "="))
	if err != nil {
		return nil, invalid
	}
	if len(auth) != 16 || len(salt) != 16 || len(dh) != 65 || len(body) < 18 || len(body) > 65536 || rs < 3 || uint64(len(body)) >= rs+16 {
		return nil, invalid
	}
	key, err := ecdh.P256().NewPrivateKey(private)
	if err != nil {
		return nil, invalid
	}
	peer, err := ecdh.P256().NewPublicKey(dh)
	if err != nil {
		return nil, invalid
	}
	shared, err := key.ECDH(peer)
	if err != nil {
		return nil, invalid
	}
	context := append([]byte("P-256\x00\x00\x41"), key.PublicKey().Bytes()...)
	context = append(context, 0, 65)
	context = append(context, dh...)
	derive := func(secret, salt, info []byte, n int) []byte {
		out := make([]byte, n)
		_, _ = io.ReadFull(hkdf.New(sha256.New, secret, salt, info), out)
		return out
	}
	ikm := derive(shared, auth, []byte("Content-Encoding: auth\x00"), 32)
	cek := derive(ikm, salt, append([]byte("Content-Encoding: aesgcm\x00"), context...), 16)
	nonce := derive(ikm, salt, append([]byte("Content-Encoding: nonce\x00"), context...), 12)
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, invalid
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, invalid
	}
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil || len(plain) < 2 {
		return nil, invalid
	}
	pad := int(binary.BigEndian.Uint16(plain[:2]))
	if pad > len(plain)-2 {
		return nil, invalid
	}
	for _, v := range plain[2 : 2+pad] {
		if v != 0 {
			return nil, invalid
		}
	}
	return plain[2+pad:], nil
}
