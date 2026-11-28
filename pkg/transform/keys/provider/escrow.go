package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/keyfile"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// kindEscrowCheck is the kind in an escrow check's test-record AAD.
const kindEscrowCheck = "escrow-check"

// setConfig makes cfg the provider's configuration, keeping its own copy of
// the escrow passes so the caller's slice can change after.
func (p *Provider) setConfig(cfg Config) {
	cfg.Escrowed = slices.Clone(cfg.Escrowed)
	p.cfg.Store(&cfg)
}

// VerifyEscrow checks copy, an off-host copy of the master key's file handed
// back by the operator (RFC 5 Appendix B.2): it opens the copy with the
// configured passphrase, wraps a fresh test record under the master key it
// holds, unwraps it with the copy's key under the copy's ID, and compares the
// copy's fingerprint with the master key's. On a pass it returns the ID and
// fingerprint; a copy that fails any step is ErrNotEscrowed, naming the
// master key. The test record's wrap raises the master key's wrap count by
// one. The check proves the operator holds the key's bytes, not where the
// copy is kept.
//
// decision: VerifyEscrow records nothing and opens no gate. The pass is kept
// by the store configuration, which hands it back through Config.Escrowed at
// New or Reconfigure, so only a committed pass opens the gate and a pass
// survives a restart; RFC 16 has no row for it. Withdraw this split if RFC 16
// gains a row the provider writes itself.
func (p *Provider) VerifyEscrow(ctx context.Context, copy []byte) (Escrow, error) {
	notEscrowed := func(why error) error {
		return fmt.Errorf("%w: master key %x: %w", keys.ErrNotEscrowed, p.masterKeyID, why)
	}

	id, key, err := keyfile.Open(copy, p.cfg.Load().Passphrase)
	if err != nil {
		return Escrow{}, notEscrowed(fmt.Errorf("the copy does not open: %w", err))
	}
	defer clear(key)
	copyID, err := parseMasterKeyID(id)
	if err != nil {
		return Escrow{}, notEscrowed(fmt.Errorf("the copy: %w", err))
	}
	// copied serves only its unwrap and its fingerprint.
	copied, err := build(copyID, key, Deps{})
	if err != nil {
		return Escrow{}, notEscrowed(fmt.Errorf("the copy: %w", err))
	}

	testKey := make([]byte, 32)
	defer clear(testKey)
	if _, err := io.ReadFull(p.rand, testKey); err != nil {
		return Escrow{}, fmt.Errorf("provider: draw an escrow test key: %w", err)
	}
	checkID, err := uuid.NewRandomFromReader(p.rand)
	if err != nil {
		return Escrow{}, fmt.Errorf("provider: mint an escrow check ID: %w", err)
	}
	aad := wrapAAD(nil, kindEscrowCheck, string(checkID[:]))
	stored, err := p.wrap(ctx, sealIn{aad: aad, key: testKey})
	if err != nil {
		return Escrow{}, err
	}

	// The unwrap refuses a wrap under another master key ID, and its AEAD
	// other key bytes; the fingerprint is RFC 5's own check of the bytes.
	got, err := copied.unwrap(stored[0], aad)
	if err != nil {
		return Escrow{}, notEscrowed(fmt.Errorf("the copy does not unwrap the test record: %w", err))
	}
	clear(got)
	if copied.masterKeyFP != p.masterKeyFP {
		return Escrow{}, notEscrowed(errors.New("the copy's fingerprint is not the master key's"))
	}

	return Escrow{MasterKeyID: id, Fingerprint: copied.masterKeyFP}, nil
}

// checkEscrow is the escrow gate: nil when a pass naming the master key's ID
// and fingerprint is in the Config last given to New or Reconfigure, and
// ErrNotEscrowed naming the key otherwise.
func (p *Provider) checkEscrow() error {
	// ValidateConfig admits only a UUID's canonical text, so text compares.
	id := uuid.UUID(p.masterKeyID).String()
	for _, e := range p.cfg.Load().Escrowed {
		if e.MasterKeyID == id && e.Fingerprint == p.masterKeyFP {
			return nil
		}
	}
	return fmt.Errorf("%w: master key %x has no passed escrow check", keys.ErrNotEscrowed, p.masterKeyID)
}

// Reconfigure takes a changed configuration without a restart: it checks
// cfg's shape, reads the key file cfg names, and replaces the passphrase and
// the escrow passes. A key file holding another master key is refused,
// naming both IDs, and a refused call changes nothing. Calls take effect in
// the order they return, so the caller makes them in its commits' order.
//
// decision: Reconfigure never changes the master key. Every key record is
// wrapped under the one the provider holds, so a swap here would leave them
// under a key it no longer has; a master-key rotation, which re-wraps them,
// is the one path to another. Withdraw the refusal only for that rotation.
func (p *Provider) Reconfigure(_ context.Context, cfg Config) error {
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	_, masterKeyID, key, err := openKeyFile(cfg)
	if err != nil {
		return err
	}
	defer clear(key)
	switch {
	case masterKeyID != p.masterKeyID:
		return fmt.Errorf("provider: key file %s holds master key %x, not %x, the one the key records are wrapped under; a rotation changes master key", cfg.KeyFile, masterKeyID, p.masterKeyID)
	case fingerprint(key) != p.masterKeyFP:
		return fmt.Errorf("provider: key file %s holds other bytes under master key %x, whose fingerprint they do not match; a rotation changes master key", cfg.KeyFile, p.masterKeyID)
	}

	p.setConfig(cfg)
	return nil
}
