package chain_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/transform/chain"
)

// Without Encrypts a chain cannot tell an encrypting namespace, and would
// accept a plaintext body there, so it is never built.
func TestNewChainRefusesNilEncrypts(t *testing.T) {
	c, err := chain.NewChain(context.Background(), nil, nil, chain.Deps{})
	if err == nil || c != nil {
		t.Fatalf("NewChain with nil Encrypts = (%v, %v), want (nil, an error)", c, err)
	}
}
