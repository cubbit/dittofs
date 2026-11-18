package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// A sealed value is stored as an envelope: the format byte, the wrapped data
// key behind its four-byte big-endian length, then the GCM nonce, the
// ciphertext and its tag. A copy of the store holds the data key only wrapped
// by a role's key, which the store never holds (RFC 13 §7).
const (
	envelopeFormat = 1  // the only format; any other byte is refused, never guessed
	lengthSize     = 4  // every length prefix, in the envelope and the additional data
	dataKeySize    = 32 // AES-256
	nonceSize      = 12 // GCM's standard nonce
	tagSize        = 16 // GCM's tag
	envelopeHeader = 1 + lengthSize
)

var (
	// errEnvelope is a sealed value that is not a whole envelope of a known
	// format.
	errEnvelope = errors.New("secrets: malformed sealed value")
	// errOpen is a sealed value that does not open under the data key and
	// additional data given.
	errOpen = errors.New("secrets: sealed value does not open")
)

// binding is what a sealed value is bound to: a value moved onto another
// role, kind, owner, reference or version, or with its HandedOver mark
// flipped, fails to open (RFC 13 §7, RFC 16 §2.3).
type binding struct {
	role       Role
	kind       entity.SecretKind
	owner      entity.SecretOwner
	ref        entity.SecretRef
	version    uint64
	handedOver bool
}

// additionalData encodes b for GCM. Each field, and each part of the owner,
// carries its own length, so two owners whose parts concatenate to the same
// bytes still bind differently (RFC 13 §7). Every stored secret was sealed
// with this order and these widths, so a change makes every sealed value
// fail to open.
func (b binding) additionalData() []byte {
	var version [8]byte
	binary.BigEndian.PutUint64(version[:], b.version)

	var handedOver byte
	if b.handedOver {
		handedOver = 1
	}

	fields := [][]byte{
		{byte(b.role)},
		{byte(b.kind)},
		{byte(b.owner.Record.Kind)},
		b.owner.Record.ID,
		{byte(b.owner.Scope.Kind)},
		b.owner.Scope.ID[:],
		[]byte(b.owner.Key),
		b.ref[:],
		version[:],
		{handedOver},
	}

	var ad []byte
	for _, f := range fields {
		ad = binary.BigEndian.AppendUint32(ad, uint32(len(f)))
		ad = append(ad, f...)
	}

	return ad
}

// sealValue encrypts value under a fresh data key, so no two versions share
// a key and a random nonce never repeats under one. It returns the key, for
// the caller to wrap under the role's key, and the nonce, ciphertext and tag.
func sealValue(value, ad []byte) (dataKey, sealed []byte, err error) {
	dataKey = make([]byte, dataKeySize)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, nil, fmt.Errorf("secrets: draw data key: %w", err)
	}

	aead, err := newAEAD(dataKey)
	if err != nil {
		return nil, nil, err
	}

	sealed = make([]byte, nonceSize, nonceSize+len(value)+tagSize)
	if _, err := rand.Read(sealed); err != nil {
		return nil, nil, fmt.Errorf("secrets: draw nonce: %w", err)
	}

	return dataKey, aead.Seal(sealed, sealed, value, ad), nil
}

// openValue decrypts what sealValue returned. A key of the wrong size, a
// short value, or a value sealed with other additional data is errOpen,
// which carries neither the value nor the key.
func openValue(dataKey, sealed, ad []byte) ([]byte, error) {
	if len(dataKey) != dataKeySize || len(sealed) < nonceSize+tagSize {
		return nil, errOpen
	}

	aead, err := newAEAD(dataKey)
	if err != nil {
		return nil, err
	}

	value, err := aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], ad)
	if err != nil {
		return nil, errOpen
	}

	return value, nil
}

func newAEAD(dataKey []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, fmt.Errorf("secrets: data key: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: data key: %w", err)
	}

	return aead, nil
}

// buildEnvelope lays a wrapped data key and a sealed value out as an
// envelope of the current format.
func buildEnvelope(wrapped, sealed []byte) []byte {
	env := make([]byte, 0, envelopeHeader+len(wrapped)+len(sealed))
	env = append(env, envelopeFormat)
	env = binary.BigEndian.AppendUint32(env, uint32(len(wrapped)))
	env = append(env, wrapped...)

	return append(env, sealed...)
}

// parseEnvelope splits an envelope back into its wrapped data key and sealed
// value. It refuses an unknown format and any envelope too short for what
// its length and the nonce and tag need, rather than read it short.
func parseEnvelope(env []byte) (wrapped, sealed []byte, err error) {
	if len(env) < envelopeHeader {
		return nil, nil, fmt.Errorf("%w: %d bytes", errEnvelope, len(env))
	}

	if env[0] != envelopeFormat {
		return nil, nil, fmt.Errorf("%w: format %d", errEnvelope, env[0])
	}

	n := uint64(binary.BigEndian.Uint32(env[1:envelopeHeader]))
	rest := env[envelopeHeader:]

	if n == 0 || n > uint64(len(rest)) {
		return nil, nil, fmt.Errorf(
			"%w: wrapped key of %d bytes in %d",
			errEnvelope,
			n,
			len(rest),
		)
	}

	wrapped, sealed = rest[:n:n], rest[n:]
	if len(sealed) < nonceSize+tagSize {
		return nil, nil, fmt.Errorf("%w: sealed part of %d bytes", errEnvelope, len(sealed))
	}

	return wrapped, sealed, nil
}
