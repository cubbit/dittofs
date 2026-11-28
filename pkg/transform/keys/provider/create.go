package provider

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// kindExportKey is the export key's kind (RFC 5 Appendix B.2), which no
// consumer outside the provider names.
const kindExportKey = "export-key"

// firstExportGeneration is a namespace's first export key's generation.
const firstExportGeneration uint64 = 1

// CreateKeys gives ns its keys at creation (RFC 5 Appendix B.2): a chunk-ID
// key and an export key of generation 1, and, when encrypts, a data key and a
// chunking key, each 32 bytes from Deps.Rand under a minted ID, written
// current. An encrypting namespace's keys are refused with ErrNotEscrowed,
// before any wrap, until the master key's escrow check has passed.
//
// It is idempotent: a namespace that already holds key records keeps them and
// CreateKeys writes nothing, returning nil when they are of the kinds encrypts
// asks for and an error otherwise. Keys and IDs are drawn first, then the
// wrap count is raised once by the number of wrapped keys and every wrap made,
// all before the records are written, so a creation that fails or loses a race
// only overcounts, and no wrap is in use without its committed record.
func (p *Provider) CreateKeys(ctx context.Context, ns entity.NamespaceID, encrypts bool) error {
	if encrypts {
		if err := p.checkEscrow(); err != nil {
			return err
		}
	}

	kinds := []string{keys.KindChunkIDKey, kindExportKey}
	if encrypts {
		kinds = append(kinds, keys.KindDataKey, keys.KindChunkingKey)
	}

	made := make([][]byte, len(kinds))
	defer func() {
		for _, key := range made {
			clear(key)
		}
	}()
	rows := make([]Row, len(kinds))
	var seals []sealIn
	for i, kind := range kinds {
		made[i] = make([]byte, 32)
		if _, err := io.ReadFull(p.rand, made[i]); err != nil {
			return fmt.Errorf("provider: draw a %s for namespace %s: %w", kind, ns, err)
		}
		u, err := uuid.NewRandomFromReader(p.rand)
		if err != nil {
			return fmt.Errorf("provider: mint a %s ID for namespace %s: %w", kind, ns, err)
		}
		id := u[:]
		if kind == kindExportKey {
			id = append(binary.BigEndian.AppendUint64(nil, firstExportGeneration), u[:]...)
		}
		rows[i] = Row{Kind: kind, ID: string(id), State: stateCurrent}

		// decision: a non-encrypting namespace's chunk-ID key is held in the
		// clear, with no master key ID (RFC 5 Appendix B.2): its bodies are
		// plaintext, so a keyed ID hides nothing from a bucket reader, and a
		// wrap would only add a way to lose every chunk's identity with the
		// host. Wrap it if a non-encrypting namespace's chunk IDs must ever
		// stop telling a bucket reader which chunks hold equal content.
		if kind == keys.KindChunkIDKey && !encrypts {
			rows[i].Record = p.record(fingerprint(made[i]), nil, made[i])
			continue
		}
		seals = append(seals, sealIn{aad: wrapAAD(ns[:], kind, rows[i].ID), key: made[i]})
	}

	wraps, err := p.wrap(ctx, seals...)
	if err != nil {
		return err
	}
	w := 0
	for i := range rows {
		if rows[i].Record == nil {
			rows[i].Record = p.record(fingerprint(made[i]), p.masterKeyID[:], wraps[w])
			w++
		}
	}

	if p.beforeCreate != nil {
		p.beforeCreate()
	}
	err = p.update(ctx, ns, func(existing []Row) ([]Row, error) {
		if len(existing) == 0 {
			return rows, nil
		}
		// decision: a namespace that already holds key records keeps them, so
		// a chunk-ID key is never replaced; a repeated creation asking for the
		// kinds it holds succeeds, and one asking for others, as a namespace
		// created non-encrypting and then asked to encrypt, is refused rather
		// than reported created without the keys it asked for. Withdraw the
		// refusal if a namespace may ever gain encryption after creation, by
		// adding its data and chunking keys here.
		held := make(map[string]bool, len(kinds))
		for _, row := range existing {
			held[row.Kind] = true
		}
		for _, kind := range kinds {
			if !held[kind] {
				return nil, fmt.Errorf("provider: namespace %s already holds keys without a %s; a namespace's keys are given only at its creation", ns, kind)
			}
			delete(held, kind)
		}
		for kind := range held {
			return nil, fmt.Errorf("provider: namespace %s already holds a %s, which a namespace created with encrypts %v has not", ns, kind, encrypts)
		}
		return nil, nil
	})
	if err != nil {
		return fmt.Errorf("provider: write the keys of namespace %s: %w", ns, err)
	}
	return nil
}
