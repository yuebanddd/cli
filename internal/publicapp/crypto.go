// Package publicapp implements the Public ChatGPT App control plane. It never
// reads local keyring credentials or XYQ_ACCESS_KEY.
package publicapp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func digest(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(digest(a)), []byte(digest(b))) == 1
}
func challenge(s string) string {
	b := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func pkceValid(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~", c) {
			return false
		}
	}
	return true
}

// Version prefix supports an explicit key rotation migration. AAD prevents a
// database operator from moving ciphertext between user accounts/purposes.
type vault struct{ aead cipher.AEAD }

func newVault(key []byte) (*vault, error) {
	if len(key) != 32 {
		return nil, errors.New("credential key must be 32 bytes")
	}
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(b)
	return &vault{a}, e
}
func (v *vault) seal(user string, b []byte) []byte {
	n := make([]byte, v.aead.NonceSize())
	if _, e := rand.Read(n); e != nil {
		panic(e)
	}
	return append([]byte{1}, v.aead.Seal(n, n, b, []byte("xyq:"+user))...)
}
func (v *vault) open(user string, b []byte) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(b) < 1+n+v.aead.Overhead() || b[0] != 1 {
		return nil, errors.New("invalid encrypted credential")
	}
	return v.aead.Open(nil, b[1:1+n], b[1+n:], []byte("xyq:"+user))
}

func uuid() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
