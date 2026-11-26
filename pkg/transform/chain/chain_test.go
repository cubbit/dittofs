package chain_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// The signatures the content wiring and every shipped transform are written
// against. A change to one fails this file's build before it fails theirs.
var (
	_ func(transform.ID, string, transform.Factory)                                                 = chain.Register
	_ func(context.Context, []transform.Config, keys.Provider, chain.Deps) (transform.Chain, error) = chain.NewChain
	_                                                                                               = chain.Deps{
		Logger:   (*slog.Logger)(nil),
		Recorder: chain.Recorder(nil),
		Require:  []string(nil),
		Encrypts: func(entity.NamespaceID) bool { return false },
	}
)

// An alert rule is written from RFC 5 §6's labels, so each reason must carry
// exactly the label the RFC spells.
func TestFailureReasonLabels(t *testing.T) {
	for _, tc := range []struct {
		reason chain.FailureReason
		want   string
	}{
		{chain.ReasonMalformed, "malformed"},
		{chain.ReasonMaterialUnavailable, "material_unavailable"},
		{chain.ReasonMaterialUnknown, "material_unknown"},
		{chain.ReasonMaterialDestroyed, "material_destroyed"},
		{chain.ReasonTooLarge, "too_large"},
	} {
		if got := tc.reason.String(); got != tc.want {
			t.Errorf("FailureReason(%d).String() = %q, want %q", tc.reason, got, tc.want)
		}
	}
}
