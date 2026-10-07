// Package crypto implements envelope encryption.
//
//	master key --HMAC--> tenant key --AES-256-GCM wraps--> random per-object DEK --AES-256-GCM--> data
//
// Blob layout: wrapNonce(12) | wrappedDEK(48) | dataNonce(12) | ciphertext.
// The additional authenticated data (tenant id + content hash) binds a blob to its owner.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

func TenantKey(master []byte, tenantID string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte("m365rescue/tenant-key/" + tenantID))
	return m.Sum(nil)
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func Seal(tenantKey, plaintext, aad []byte) ([]byte, error) {
	dek := make([]byte, 32)
	wn := make([]byte, 12)
	dn := make([]byte, 12)
	for _, b := range [][]byte{dek, wn, dn} {
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
	}
	kw, err := gcm(tenantKey)
	if err != nil {
		return nil, err
	}
	kd, err := gcm(dek)
	if err != nil {
		return nil, err
	}
	wrapped := kw.Seal(nil, wn, dek, aad)
	out := make([]byte, 0, len(wn)+len(wrapped)+len(dn)+len(plaintext)+16)
	out = append(out, wn...)
	out = append(out, wrapped...)
	out = append(out, dn...)
	return kd.Seal(out, dn, plaintext, aad), nil
}

func Open(tenantKey, blob, aad []byte) ([]byte, error) {
	if len(blob) < 12+48+12+16 {
		return nil, errors.New("crypto: blob too short")
	}
	kw, err := gcm(tenantKey)
	if err != nil {
		return nil, err
	}
	dek, err := kw.Open(nil, blob[:12], blob[12:60], aad)
	if err != nil {
		return nil, errors.New("crypto: cannot unwrap data key (wrong tenant key or tampering)")
	}
	kd, err := gcm(dek)
	if err != nil {
		return nil, err
	}
	pt, err := kd.Open(nil, blob[60:72], blob[72:], aad)
	if err != nil {
		return nil, errors.New("crypto: authentication failed (blob modified)")
	}
	return pt, nil
}
