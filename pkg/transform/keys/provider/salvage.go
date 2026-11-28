package provider

import (
	"context"
	"fmt"
	"io"

	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// kindSalvageKey is the kind a salvage key's associated data names (RFC 5
// Appendix B.2).
const kindSalvageKey = "salvage-key"

var _ keys.SalvageWrapper = (*Provider)(nil)

// WrapSalvageKey draws a fresh 32-byte key for the salvage export named
// export and wraps it under the master key, in the stored-wrap layout, with
// the key wrap's associated data over an empty namespace ID: namespace ID
// (length 0) ‖ salvage-key ‖ export. Every namespace ID is 16 bytes, so the
// wrap never unwraps as a namespace key's, and it unwraps only with its own
// export's bytes. It raises the master key's wrap count by one and writes
// no key record, since a salvage key belongs to no namespace.
//
// decision: the master key is this provider's, its store configuration's
// current one, where RFC 5 Appendix B.2 says "the installation's current
// master key": master keys belong to a store configuration, and the content
// wiring picks the configuration whose provider wraps a stranded tag's
// salvage key: that of the shares the tag's journal serves, the lowest
// configuration ID when they span several, none when none has a master key.
// Revisit when RFC 5 states which master key it means.
func (p *Provider) WrapSalvageKey(ctx context.Context, export []byte) (key, wrap []byte, err error) {
	key = make([]byte, 32)
	if _, err := io.ReadFull(p.rand, key); err != nil {
		clear(key)
		return nil, nil, fmt.Errorf("provider: draw a salvage key: %w", err)
	}

	wraps, err := p.wrap(ctx, sealIn{aad: wrapAAD(nil, kindSalvageKey, string(export)), key: key})
	if err != nil {
		clear(key)
		return nil, nil, err
	}

	return key, wraps[0], nil
}
