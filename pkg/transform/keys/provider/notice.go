package provider

import (
	"context"
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

// DestroyedSince returns, at most 256 at a time, the material this provider
// has found destroyed after cursor, and the cursor to pass next: every ID an
// Open found in a destroyed record, each once. Match an ID by its namespace,
// kind and ID: its fingerprint is the one the record kept, or zero for a
// record that no longer decodes.
//
// decision: the cursor is not durable. Its high 32 bits are an incarnation
// drawn at New, its low 32 bits an index into what this provider has found;
// a cursor of another incarnation, 0 included, or one past the list's end,
// reads from the start, so a cursor from before a restart skips nothing this
// provider finds. What was destroyed before it started is in the key records,
// which the engine reseeds from as it starts (RFC 8 §10.4). That holds while
// a provider and its engine are built together: a provider replaced under a
// running engine that draws its predecessor's incarnation, with probability
// 2⁻³², would skip. A durable incarnation, one more row, is the upgrade once
// a provider can be replaced under a running engine.
func (p *Provider) DestroyedSince(_ context.Context, cursor uint64) ([]keys.MaterialID, uint64, error) {
	n := &p.notice
	n.mu.Lock()
	defer n.mu.Unlock()

	i := uint64(0)
	if uint32(cursor>>32) == n.incarnation && uint32(cursor) <= uint32(len(n.ids)) {
		i = uint64(uint32(cursor))
	}
	j := min(i+noticeBatch, uint64(len(n.ids)))
	return slices.Clone(n.ids[i:j]), uint64(n.incarnation)<<32 | j, nil
}

// listDestroyed lists in the notice, unless listed already, every destroyed
// record of set, the records of ns an Open loaded, in material order, by the
// fingerprint the record kept, or none for a record that no longer decodes.
// It is the notice's one feed.
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

	n := &p.notice
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, id := range found {
		named := id
		named.Fingerprint = [32]byte{}
		if _, ok := n.listed[named]; !ok {
			n.listed[named] = struct{}{}
			n.ids = append(n.ids, id)
		}
	}
}
