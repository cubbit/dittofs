package chain_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

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

// discard is a Recorder that keeps nothing: no test here reaches a transform.
type discard struct{}

func (discard) For(string) chain.TransformRecorder    { return discard{} }
func (discard) Encoded(bool, int, int, time.Duration) {}
func (discard) Decoded(int, int, time.Duration)       {}
func (discard) DecodeFailed(chain.FailureReason)      {}

// A chain built without a member of Deps would fail open where it uses it, a
// nil Encrypts most of all, so NewChain refuses it and names the member.
func TestNewChainRefusesMissingDeps(t *testing.T) {
	full := func() chain.Deps {
		return chain.Deps{
			Logger:   slog.New(slog.DiscardHandler),
			Recorder: discard{},
			Encrypts: func(entity.NamespaceID) bool { return true },
		}
	}
	for _, tc := range []struct {
		member string
		drop   func(*chain.Deps)
	}{
		{"Logger", func(d *chain.Deps) { d.Logger = nil }},
		{"Recorder", func(d *chain.Deps) { d.Recorder = nil }},
		{"Encrypts", func(d *chain.Deps) { d.Encrypts = nil }},
	} {
		deps := full()
		tc.drop(&deps)
		_, err := chain.NewChain(context.Background(), nil, nil, deps)
		if err == nil || errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "Deps."+tc.member) {
			t.Errorf("NewChain without Deps.%s: err = %v, want a refusal naming Deps.%s", tc.member, err, tc.member)
		}
	}
}
