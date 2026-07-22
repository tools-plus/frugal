// Package secret encrypts credential values at rest with AES-256-GCM, keyed by
// FRUGAL_SECRET_KEY. Ciphertext is stored as "enc:v1:<base64(nonce||ct)>", so
// plaintext (legacy or non-secret) values pass through Decrypt unchanged.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const prefix = "enc:v1:"

// Cipher encrypts/decrypts secret strings. A zero Cipher (no key) is usable but
// reports Available()==false: Encrypt fails and Decrypt of ciphertext fails,
// while plaintext passes through.
type Cipher struct {
	gcm cipher.AEAD
	ok  bool
}

// New derives a cipher from the key (SHA-256 → AES-256). An empty key yields an
// unavailable cipher.
func New(key string) *Cipher {
	if key == "" {
		return &Cipher{}
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return &Cipher{}
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return &Cipher{}
	}
	return &Cipher{gcm: gcm, ok: true}
}

func (c *Cipher) Available() bool { return c != nil && c.ok }

// IsEncrypted reports whether s is a value produced by Encrypt.
func IsEncrypted(s string) bool { return strings.HasPrefix(s, prefix) }

// Encrypt returns ciphertext for a non-empty plaintext. Empty stays empty.
func (c *Cipher) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if IsEncrypted(plain) {
		return plain, nil // already encrypted
	}
	if !c.Available() {
		return "", errors.New("no secret key set (FRUGAL_SECRET_KEY) — cannot store credentials")
	}
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := c.gcm.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverses Encrypt. Plaintext (no prefix) is returned as-is.
func (c *Cipher) Decrypt(s string) (string, error) {
	if s == "" || !IsEncrypted(s) {
		return s, nil
	}
	if !c.Available() {
		return "", errors.New("cannot decrypt: no secret key set (FRUGAL_SECRET_KEY)")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return "", err
	}
	ns := c.gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := c.gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
