package provider

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// reload drops every cached record and key, then opens again each namespace
// the cache held, as a restart over the same records would. An Open refused
// for its material is left for the caller's next read to see, as a namespace
// that does not open would be.
func (p *Provider) reload(ctx context.Context) {
	p.cache.mu.Lock()
	held := make([]entity.NamespaceID, 0, len(p.cache.sets))
	for ns := range p.cache.sets {
		held = append(held, ns)
		p.cache.epochs[ns]++
	}
	clear(p.cache.sets)
	p.cache.mu.Unlock()

	for _, ns := range held {
		_ = p.Open(ctx, ns)
	}
}
