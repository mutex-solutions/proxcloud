package textcrypt

// The blob API is deliberately separate from the legacy, interoperable text API.
// It never returns plaintext on an encryption/decryption failure.
import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"regexp"
)

var (
	ErrBlobKey = errors.New("textcrypt: blob key unavailable")
	ErrBlob    = errors.New("textcrypt: invalid authenticated blob")
	blobKeyID  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

// BlobKeys holds an active key and retained decryption keys. Password material is
// copied and domain-separated through HMAC-SHA256; existing configured encryption
// secrets are usable without inventing a second password format. Supply high
// entropy secrets. Key IDs are public identifiers, NOT secret material.
// Keep old IDs and their original keys until their outbox rows are reconciled.
// Missing keys, empty secrets and unknown IDs fail closed.
type BlobKeys struct {
	active string
	keys   map[string][]byte
}

func NewBlobKeys(active string, keys map[string]string) (*BlobKeys, error) {
	b := &BlobKeys{active: active, keys: make(map[string][]byte, len(keys))}
	for id, secret := range keys {
		if !blobKeyID.MatchString(id) || len(secret) < 32 {
			return nil, ErrBlobKey
		}
		b.keys[id] = []byte(secret)
	}
	if !blobKeyID.MatchString(active) || len(b.keys[active]) == 0 {
		return nil, ErrBlobKey
	}
	return b, nil
}
func (b *BlobKeys) ActiveID() string {
	if b == nil {
		return ""
	}
	return b.active
}
func (b *BlobKeys) LogValue() slog.Value { return slog.StringValue("[blob keys redacted]") }
func (b *BlobKeys) String() string       { return "[blob keys redacted]" }

func (b *BlobKeys) derived(id, purpose string) ([]byte, error) {
	if b == nil || len(b.keys[id]) == 0 {
		return nil, ErrBlobKey
	}
	h := hmac.New(sha256.New, b.keys[id])
	_, _ = h.Write([]byte("proxcloud/strict-blob/v1/" + purpose))
	return h.Sum(nil), nil
}
func (b *BlobKeys) aead(id string) (cipher.AEAD, error) {
	key, err := b.derived(id, "aes-gcm")
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrBlob
	}
	return cipher.NewGCM(block)
}

// AAD is unambiguous even when fields contain separators. The key ID and format
// version are authenticated along with the caller's outbox ID/kind/version.
func blobAAD(id string, aad []byte) []byte {
	v := make([]byte, 9+len(id)+len(aad))
	v[0] = 1
	binary.BigEndian.PutUint32(v[1:5], uint32(len(id)))
	copy(v[5:], id)
	i := 5 + len(id)
	binary.BigEndian.PutUint32(v[i:i+4], uint32(len(aad)))
	copy(v[i+4:], aad)
	return v
}

// Seal stores format-version || random nonce || ciphertext/tag, suitable for a
// bytea column. The caller stores the returned public key ID separately.
func (b *BlobKeys) Seal(plaintext, aad []byte) (id string, blob []byte, err error) {
	if b == nil {
		return "", nil, ErrBlobKey
	}
	id = b.active
	a, err := b.aead(id)
	if err != nil {
		return "", nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", nil, ErrBlob
	}
	blob = append([]byte{1}, nonce...)
	blob = a.Seal(blob, nonce, plaintext, blobAAD(id, aad))
	return id, blob, nil
}
func (b *BlobKeys) Open(id string, blob, aad []byte) ([]byte, error) {
	a, err := b.aead(id)
	if err != nil {
		return nil, err
	}
	if len(blob) < 1+a.NonceSize()+a.Overhead() || blob[0] != 1 {
		return nil, ErrBlob
	}
	pt, err := a.Open(nil, blob[1:1+a.NonceSize()], blob[1+a.NonceSize():], blobAAD(id, aad))
	if err != nil {
		return nil, ErrBlob
	}
	return pt, nil
}

// Fingerprint authenticates canonical PLAINTEXT, never the randomized ciphertext.
// Specify the original row's key ID for deduplication across key rotation.
func (b *BlobKeys) Fingerprint(id string, plaintext []byte) ([]byte, error) {
	key, err := b.derived(id, "fingerprint")
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(plaintext)
	return h.Sum(nil), nil
}
