package provider

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

var _ keys.DestroyedNotice = (*Provider)(nil)

// noticeBatch is the most IDs one DestroyedSince returns.
const noticeBatch = 256

// notice lists, once each, the material this provider has reported
// destroyed, in the order it found it.
type notice struct {
	mu          sync.Mutex
	incarnation uint32
	ids         []keys.MaterialID
	listed      map[keys.MaterialID]struct{} // by namespace, kind and ID, no fingerprint
}

// drawIncarnation draws n's incarnation from rnd.
func (n *notice) drawIncarnation(rnd io.Reader) error {
	var b [4]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return fmt.Errorf("provider: draw the notice's incarnation: %w", err)
	}
	n.incarnation = binary.BigEndian.Uint32(b[:])
	return nil
}

// add lists id unless its namespace, kind and ID are listed already.
func (n *notice) add(id keys.MaterialID) {
	named := id
	named.Fingerprint = [32]byte{}

	n.mu.Lock()
	defer n.mu.Unlock()

	if _, ok := n.listed[named]; ok {
		return
	}
	n.listed[named] = struct{}{}
	n.ids = append(n.ids, id)
}

// DestroyedSince returns, at most 256 at a time, the material this provider
// has found destroyed after cursor, and the cursor to pass next: every ID an
// Open found in a destroyed record, each once.
//
// decision: the cursor is not durable. Its high 32 bits are an incarnation
// drawn at New, its low 32 bits an index into what this provider has found;
// a cursor of another incarnation, 0 included, or one past the list's end,
// reads from the start, so a cursor from before a restart skips nothing this
// provider finds. What was destroyed before it started is in the key records,
// which the engine reseeds from as it starts (RFC 8 §10.4). A new incarnation
// drawn equal to the old one, with probability 2⁻³² per restart, would skip;
// a durable incarnation, one more row, is the upgrade once a provider can
// outlive its engine.
func (p *Provider) DestroyedSince(_ context.Context, cursor uint64) ([]keys.MaterialID, uint64, error) {
	n := &p.notice
	n.mu.Lock()
	defer n.mu.Unlock()

	ids, next := since(n.ids, n.incarnation, cursor)
	return ids, next, nil
}

// since is the batch of ids after cursor, by the incarnation inc, and the
// cursor that follows it.
func since(ids []keys.MaterialID, inc uint32, cursor uint64) ([]keys.MaterialID, uint64) {
	i := uint64(0)
	if uint32(cursor>>32) == inc && uint32(cursor) <= uint32(len(ids)) {
		i = uint64(uint32(cursor))
	}
	j := min(i+noticeBatch, uint64(len(ids)))
	return slices.Clone(ids[i:j]), uint64(inc)<<32 | j
}

// listDestroyed lists in the notice every destroyed record of set, the
// records of ns an Open loaded, in material order, by the fingerprint the
// record kept; a record that no longer decodes is listed with none.
func (p *Provider) listDestroyed(ns entity.NamespaceID, set map[slot]*entry) {
	var found []keys.MaterialID
	for s, e := range set {
		if e.row.State != stateDestroyed {
			continue
		}
		id := keys.MaterialID{Namespace: ns, Kind: s.kind, ID: s.id}
		if rec, err := decodeKeyRecord(e.row.Record); err == nil {
			id.Fingerprint = rec.fingerprint
		}
		found = append(found, id)
	}
	slices.SortFunc(found, compareMaterial)
	for _, id := range found {
		p.notice.add(id)
	}
}
