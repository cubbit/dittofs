package chain_test

import (
	"context"
	"errors"
	"fmt"
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

// sink is a Recorder that keeps what the chain delivers to it, as text.
type sink struct{ got *[]string }

func (s sink) For(name string) chain.TransformRecorder {
	*s.got = append(*s.got, "for "+name)
	return s
}
func (s sink) Encoded(applied bool, in, out int, _ time.Duration) {
	*s.got = append(*s.got, fmt.Sprint("encoded ", applied, in, out))
}
func (s sink) Decoded(in, out int, _ time.Duration) {
	*s.got = append(*s.got, fmt.Sprint("decoded ", in, out))
}
func (s sink) DecodeFailed(r chain.FailureReason) {
	*s.got = append(*s.got, "decode failed "+r.String())
}

// A chain built without a member of Deps, or without its provider, would fail
// open (a nil Encrypts) or panic at first use, so NewChain refuses it, names
// the member, and records nothing. The cases pass a nil provider: NewChain
// checks Deps first, and no provider implementation exists here to pass.
func TestNewChainRefusesMissingDeps(t *testing.T) {
	var got []string
	full := func() chain.Deps {
		return chain.Deps{
			Logger:   slog.New(slog.DiscardHandler),
			Recorder: sink{&got},
			Encrypts: func(entity.NamespaceID) bool { return true },
		}
	}
	for _, tc := range []struct {
		member string
		drop   func(*chain.Deps)
	}{
		{"Deps.Logger", func(d *chain.Deps) { d.Logger = nil }},
		{"Deps.Recorder", func(d *chain.Deps) { d.Recorder = nil }},
		{"Deps.Encrypts", func(d *chain.Deps) { d.Encrypts = nil }},
		{"keys provider", func(*chain.Deps) {}},
	} {
		deps := full()
		tc.drop(&deps)
		_, err := chain.NewChain(context.Background(), nil, nil, deps)
		if err == nil || errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), tc.member) {
			t.Errorf("NewChain without %s: err = %v, want a refusal naming %s", tc.member, err, tc.member)
		}
	}
	if len(got) != 0 {
		t.Errorf("refused NewChain calls recorded %q, want nothing", got)
	}
}
