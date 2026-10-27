// Package textcrypt is an AES-256 text encryptor (CBC, PBE-derived key) used to protect
// ExternalService secrets at rest — DefaultEncryptionImpl wraps it, keyed by
// proxcloud.encryption.default.key.
//
// The scheme is PBE with algorithm "PBEWithHMACSHA512AndAES_256": a random IV and a random
// salt (both prepended to the result) and the default 1000 key-obtention iterations. So the
// on-the-wire format is:
//
//	base64( salt[16] || iv[16] || AES-256-CBC(PKCS#7(plaintext)) )
//
// with the AES key derived as PBKDF2(HMAC-SHA512, passwordBytes, salt, 1000, 32). The key is
// derived via PBKDF2 over the password's raw bytes; for the ASCII encryption key ProxCloud uses,
// []byte(key) (UTF-8) == those bytes. (Round-trip + structure are covered by the tests.)
package textcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
)

const (
	saltLen    = 16
	ivLen      = 16
	iterations = 1000
	keyLen     = 32 // AES-256
)

// Encryptor is an AES-256 text encryptor bound to one password
// (the encryption key). The zero value is unusable; construct with New.
type Encryptor struct {
	password []byte
}

// New returns an encryptor for the given key (proxcloud.encryption.default.key). An empty key
// leaves Decrypt/Encrypt as pass-throughs — the input is returned unchanged (see the
// pass-through note).
func New(key string) *Encryptor { return &Encryptor{password: []byte(key)} }

// HasKey reports whether a key was configured.
func (e *Encryptor) HasKey() bool { return len(e.password) > 0 }

// Decrypt reverses Encrypt (DefaultEncryptionImpl.decrypt): ANY failure (no key,
// bad base64, wrong length, bad padding, …) returns the input string unchanged — this
// lets already-plaintext or non-encrypted values pass through.
func (e *Encryptor) Decrypt(encrypted string) string {
	out, err := e.decrypt(encrypted)
	if err != nil {
		return encrypted
	}
	return out
}

func (e *Encryptor) decrypt(encrypted string) (string, error) {
	if !e.HasKey() {
		return "", errors.New("textcrypt: encryption key is not set")
	}
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", err
	}
	if len(raw) < saltLen+ivLen+aes.BlockSize || (len(raw)-saltLen-ivLen)%aes.BlockSize != 0 {
		return "", fmt.Errorf("textcrypt: ciphertext length %d invalid", len(raw))
	}
	salt := raw[:saltLen]
	iv := raw[saltLen : saltLen+ivLen]
	ct := raw[saltLen+ivLen:]
	key, err := pbkdf2.Key(sha512.New, string(e.password), salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	unpadded, err := pkcs7Unpad(pt, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return string(unpadded), nil
}

// Encrypt produces an interoperable value (fresh random salt + IV each call, so output is
// non-deterministic). Returns the input unchanged if no key is set (pass-through in
// DefaultEncryptionImpl.encrypt).
func (e *Encryptor) Encrypt(plaintext string) string {
	out, err := e.encrypt(plaintext)
	if err != nil {
		return plaintext
	}
	return out
}

func (e *Encryptor) encrypt(plaintext string) (string, error) {
	if !e.HasKey() {
		return "", errors.New("textcrypt: encryption key is not set")
	}
	salt := make([]byte, saltLen)
	iv := make([]byte, ivLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha512.New, string(e.password), salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad([]byte(plaintext), aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	raw := make([]byte, 0, saltLen+ivLen+len(ct))
	raw = append(raw, salt...)
	raw = append(raw, iv...)
	raw = append(raw, ct...)
	return base64.StdEncoding.EncodeToString(raw), nil
}

// DecryptObject walks the object's fields (DefaultEncryptionImpl.decryptObject), decrypting
// every TEXTUAL leaf and recursing into nested OBJECTS. Arrays, numbers, booleans and nulls
// pass through untouched (only objects/text are handled). A non-map input is returned unchanged.
func (e *Encryptor) DecryptObject(v any) any { return e.walk(v, e.Decrypt) }

// EncryptObject is the inverse walk.
func (e *Encryptor) EncryptObject(v any) any { return e.walk(v, e.Encrypt) }

func (e *Encryptor) walk(v any, fn func(string) string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			switch vv := val.(type) {
			case map[string]any:
				out[k] = e.walk(vv, fn)
			case string:
				out[k] = fn(vv)
			default:
				out[k] = val
			}
		}
		return out
	default:
		return v
	}
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	n := blockSize - len(data)%blockSize
	pad := make([]byte, n)
	for i := range pad {
		pad[i] = byte(n)
	}
	return append(data, pad...)
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("textcrypt: padded data not a block multiple")
	}
	n := int(data[len(data)-1])
	if n == 0 || n > blockSize || n > len(data) {
		return nil, errors.New("textcrypt: bad PKCS#7 padding")
	}
	for _, b := range data[len(data)-n:] {
		if int(b) != n {
			return nil, errors.New("textcrypt: bad PKCS#7 padding")
		}
	}
	return data[:len(data)-n], nil
}
