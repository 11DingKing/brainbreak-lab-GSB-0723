// Package cryptoshred implements crypto-shredding for personal data. Each
// subject has a randomly generated 256-bit data key. Personal fields are stored
// only in AES-256-GCM ciphertext encrypted under that key. "彻底删除" (hard
// delete) destroys the key; without it the ciphertext in the events table and
// any derived tables is unrecoverable, so no personal data can be reconstructed
// even from backups of the derived rows.
package cryptoshred

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"

	"github.com/google/uuid"
)

// ErrKeyDestroyed is returned when decryption is attempted for a subject whose
// key has been shredded. Callers treat this as "personal data permanently
// unavailable", not as a transient error.
var ErrKeyDestroyed = errors.New("cryptoshred: data key destroyed")

// ErrKeyMissing is returned when no key exists for a subject.
var ErrKeyMissing = errors.New("cryptoshred: data key missing")

// KeyLen is the AES-256 key length in bytes.
const KeyLen = 32

// NewKey generates a fresh 256-bit data key.
func NewKey() ([]byte, error) {
	k := make([]byte, KeyLen)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, err
	}
	return k, nil
}

// KeyStore holds per-subject data keys. Implementations must make destruction
// irreversible relative to their durability guarantees (e.g. delete the row and
// rely on the absence of the key material, never a soft-delete flag).
type KeyStore interface {
	// PutKey stores a subject's data key, creating it if absent. It is
	// idempotent for an unchanged key.
	PutKey(subjectID uuid.UUID, key []byte) error
	// GetKey returns the subject's key, ErrKeyDestroyed if it was shredded, or
	// ErrKeyMissing if it never existed.
	GetKey(subjectID uuid.UUID) ([]byte, error)
	// DestroyKey irreversibly removes the subject's key material and records a
	// tombstone so future GetKey calls return ErrKeyDestroyed.
	DestroyKey(subjectID uuid.UUID) error
}

// Seal encrypts plaintext under key using AES-256-GCM with a random nonce. The
// nonce is prepended to the ciphertext. subjectID is bound as additional
// authenticated data so ciphertext cannot be transplanted between subjects.
func Seal(key, plaintext []byte, subjectID uuid.UUID) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	aad := subjectID[:]
	ct := gcm.Seal(nil, nonce, plaintext, aad)
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Open decrypts data produced by Seal. It returns an error if the key is wrong,
// the data is tampered, or the subject binding does not match.
func Open(key, sealed []byte, subjectID uuid.UUID) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("cryptoshred: ciphertext too short")
	}
	nonce, ct := sealed[:ns], sealed[ns:]
	return gcm.Open(nil, nonce, ct, subjectID[:])
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, errors.New("cryptoshred: invalid key length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Cipher is a convenience façade binding a KeyStore to seal/open operations so
// callers never touch raw key bytes. It fetches the subject key on demand and
// surfaces ErrKeyDestroyed transparently.
type Cipher struct {
	keys KeyStore
}

// New returns a Cipher backed by ks.
func New(ks KeyStore) *Cipher { return &Cipher{keys: ks} }

// EnsureKey creates a data key for the subject if one does not already exist.
// It is a no-op when a live key is present and returns ErrKeyDestroyed if the
// subject was previously shredded (re-creating a key would be a policy
// violation: revoked subjects must stay unrecoverable).
func (c *Cipher) EnsureKey(subjectID uuid.UUID) error {
	_, err := c.keys.GetKey(subjectID)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrKeyMissing):
		k, gerr := NewKey()
		if gerr != nil {
			return gerr
		}
		return c.keys.PutKey(subjectID, k)
	default:
		return err
	}
}

// SealFor encrypts plaintext for a subject using their live key.
func (c *Cipher) SealFor(subjectID uuid.UUID, plaintext []byte) ([]byte, error) {
	k, err := c.keys.GetKey(subjectID)
	if err != nil {
		return nil, err
	}
	return Seal(k, plaintext, subjectID)
}

// OpenFor decrypts sealed data for a subject; returns ErrKeyDestroyed if shredded.
func (c *Cipher) OpenFor(subjectID uuid.UUID, sealed []byte) ([]byte, error) {
	k, err := c.keys.GetKey(subjectID)
	if err != nil {
		return nil, err
	}
	return Open(k, sealed, subjectID)
}

// Shred destroys the subject's data key, permanently preventing decryption of
// their personal fields anywhere they are stored.
func (c *Cipher) Shred(subjectID uuid.UUID) error {
	return c.keys.DestroyKey(subjectID)
}
