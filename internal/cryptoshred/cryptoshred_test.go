package cryptoshred

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// memKeyStore is a minimal KeyStore for testing the Cipher facade.
type memKeyStore struct {
	keys map[uuid.UUID][]byte
	dead map[uuid.UUID]bool
}

func newMemKeyStore() *memKeyStore {
	return &memKeyStore{keys: map[uuid.UUID][]byte{}, dead: map[uuid.UUID]bool{}}
}

func (m *memKeyStore) PutKey(id uuid.UUID, key []byte) error {
	if m.dead[id] {
		return ErrKeyDestroyed
	}
	m.keys[id] = append([]byte(nil), key...)
	return nil
}
func (m *memKeyStore) GetKey(id uuid.UUID) ([]byte, error) {
	if m.dead[id] {
		return nil, ErrKeyDestroyed
	}
	k, ok := m.keys[id]
	if !ok {
		return nil, ErrKeyMissing
	}
	return k, nil
}
func (m *memKeyStore) DestroyKey(id uuid.UUID) error {
	delete(m.keys, id)
	m.dead[id] = true
	return nil
}

func TestSealOpenRoundTrip(t *testing.T) {
	c := New(newMemKeyStore())
	id := uuid.New()
	require.NoError(t, c.EnsureKey(id))
	plain := []byte(`{"display_name":"Alice","timezone":"Asia/Shanghai"}`)
	sealed, err := c.SealFor(id, plain)
	require.NoError(t, err)
	require.NotContains(t, string(sealed), "Alice", "ciphertext must not reveal plaintext")

	got, err := c.OpenFor(id, sealed)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}

func TestShredMakesDataUnrecoverable(t *testing.T) {
	c := New(newMemKeyStore())
	id := uuid.New()
	require.NoError(t, c.EnsureKey(id))
	sealed, err := c.SealFor(id, []byte("personal"))
	require.NoError(t, err)

	require.NoError(t, c.Shred(id))

	// Decryption is now impossible.
	_, err = c.OpenFor(id, sealed)
	require.ErrorIs(t, err, ErrKeyDestroyed)

	// Re-creating a key is refused, so the data stays unrecoverable forever.
	require.ErrorIs(t, c.EnsureKey(id), ErrKeyDestroyed)
}

func TestSubjectBindingPreventsTransplant(t *testing.T) {
	ks := newMemKeyStore()
	c := New(ks)
	a, b := uuid.New(), uuid.New()
	require.NoError(t, c.EnsureKey(a))
	require.NoError(t, c.EnsureKey(b))

	sealedForA, err := c.SealFor(a, []byte("secret"))
	require.NoError(t, err)

	// Attempting to open A's ciphertext under B's key must fail (AAD mismatch
	// and different key).
	_, err = c.OpenFor(b, sealedForA)
	require.Error(t, err)
}
