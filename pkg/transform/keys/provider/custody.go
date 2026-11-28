package provider

import (
	"bytes"
	"context"
	"crypto/cipher"
	"fmt"
	"io"

	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// custody is where a provider's master key lives and what it does with it: a
// key file, whose key the provider holds, or a key service, which holds the
// key and wraps and unwraps for the provider (RFC 5 Appendix B.1).
type custody interface {
	// kind is the provider kind of the stored wraps the custody writes.
	kind() byte
	// wrap wraps each key of in under the master key's primary version,
	// returning each one's stored wrap in order.
	wrap(ctx context.Context, in ...sealIn) ([][]byte, error)
	// unwrap opens stored, one of the custody's stored wraps, under its
	// associated data aad.
	unwrap(ctx context.Context, stored, aad []byte) ([]byte, error)
	// primary reads the master key's primary version, as the
	// version-qualified master-key ID a wrap made now would record.
	primary(ctx context.Context) ([]byte, error)
	// state reads the state of the master key masterKeyID names: nil when
	// it is active, keys.ErrMaterialDestroyed or keys.ErrMaterialUnavailable
	// when the custody answers another state, and an error wrapping neither
	// when the read gets no answer, so the caller keeps the state it last
	// read.
	state(ctx context.Context, masterKeyID []byte) error
}

var (
	_ custody = (*fileCustody)(nil)
	_ custody = (*keyService)(nil)
)

// fileCustody is the key-file custody: the master key read from the store
// configuration's key file, held in this process.
type fileCustody struct {
	records KeyRecords
	rand    io.Reader

	// masterKeyID names the master key; aead holds the key itself, which
	// never leaves this process.
	masterKeyID [16]byte
	aead        cipher.AEAD
	// masterKeyFP is the master key's fingerprint, which an escrow pass
	// must name.
	masterKeyFP [32]byte
}

func (*fileCustody) kind() byte { return kindKeyFile }

// primary is the key file's one master key ID: a key file has no other
// version.
func (k *fileCustody) primary(context.Context) ([]byte, error) {
	return bytes.Clone(k.masterKeyID[:]), nil
}

// state is nil for the key file's master key, which is active while the
// provider holds it, and unavailable for any other ID.
func (k *fileCustody) state(_ context.Context, masterKeyID []byte) error {
	if !bytes.Equal(masterKeyID, k.masterKeyID[:]) {
		return fmt.Errorf("provider: master key %x is not the key file's %x: %w", masterKeyID, k.masterKeyID, keys.ErrMaterialUnavailable)
	}
	return nil
}
