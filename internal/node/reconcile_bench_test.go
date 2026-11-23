package node_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/settings"
)

// benchShares is the share count RFC 23 §8's bound on a live change is
// measured at.
const benchShares = 10_000

// benchChurn is how many other shares change in each pass beside the one
// timed.
//
// decision: steady churn is ten share state changes a second, each its own
// write and so its own move of the share list version. No rate is specified;
// raise it once an installation's audit shows a higher one.
const benchChurn = 10

// BenchmarkReconcile times the reconcile pass a live change waits on, over
// benchShares shares on a node whose store is on disk: "cold", one pass
// binding every share with none bound, and "churn", one share's state change
// applied in a pass beside benchChurn others, with every share bound. Each
// is timed from the commit that moves the share list version to the end of
// the pass that binds the change. The pass is woken as a write on this node
// wakes it, so the figure leaves out the wait for the next turn: RFC 23 §8's
// 5 s holds while the figure plus reconcileInterval stays under it.
//
// Run it as its suite does, with -benchtime=1x: the cold case builds a node
// and writes its shares outside the timed region of every iteration.
func BenchmarkReconcile(b *testing.B) {
	b.Run("cold", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			b.StopTimer()
			h := nodetest.New(b)
			if err := h.Node().Start(b.Context()); err != nil {
				b.Fatalf("Start: %v", err)
			}
			configure(b, h, storeA)
			shares := benchCreate(b, h)
			want := counters(b, h).ShareList
			b.StartTimer()
			benchPass(b, h, want)
			b.StopTimer()
			for _, s := range shares {
				benchBound(b, h, s.ID, entity.ShareEnabled)
			}
			if err := h.Node().Stop(b.Context()); err != nil {
				b.Fatalf("Stop: %v", err)
			}
		}
	})

	b.Run("churn", func(b *testing.B) {
		h := nodetest.New(b)
		started(b, h)
		configure(b, h, storeA)
		shares := benchCreate(b, h)
		benchPass(b, h, counters(b, h).ShareList)
		state := make([]entity.ShareState, len(shares))
		for i := range state {
			state[i] = entity.ShareEnabled
		}
		next := 0
		// flip moves the next share in turn to the other state, and returns
		// it in the state it moved to.
		flip := func() (entity.ShareID, entity.ShareState) {
			i := next % len(shares)
			next++
			to := entity.ShareQuiesced
			if state[i] == entity.ShareQuiesced {
				to = entity.ShareEnabled
			}
			if err := h.Store().ControlPlane().SetShareState(b.Context(), shares[i].ID, state[i], to); err != nil {
				b.Fatalf("share %s state %v to %v: %v", shares[i].Name, state[i], to, err)
			}
			state[i] = to
			return shares[i].ID, to
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			b.StopTimer()
			for range benchChurn {
				flip()
			}
			id, to := flip()
			want := counters(b, h).ShareList
			b.StartTimer()
			benchPass(b, h, want)
			b.StopTimer()
			benchBound(b, h, id, to)
		}
	})

	// "settings" times the whole settings reconcile one share's live change
	// waits on: the shares pass a settings move also makes, the walk over the
	// sparse rows of benchShares shares, the diff, the secrets pass over the
	// sixteen references and the applies, from the commit that moves the
	// settings generation to the end of the pass. The whole fixture is
	// written before Start, so the passes before the guards are the start's
	// alone, and each iteration fails itself unless the timed pass refused
	// nothing, reconciled through the commit it moved and published the new
	// value: a figure for a pass that refused, never reached the commit or
	// skipped the change answers none of what a live change waits on. The
	// figure includes the shares pass, since due marks the shares when the
	// settings move, and is judged against the walk's ceiling, a tenth of the
	// settings poll's second; reading only the records above the reconciled
	// position is the upgrade path once a figure passes it, as the walk's own
	// marker holds.
	b.Run("settings", func(b *testing.B) {
		h := nodetest.New(b)
		benchDeclare(h)
		configure(b, h, storeA)
		shares := benchCreate(b, h)
		benchFixture(b, h, shares)
		started(b, h)

		if got := h.Node().Refused(); len(got) != 0 {
			b.Fatalf("the bench fixture after Start: refused %+v, want none", got)
		}

		if got := h.Node().Settings().Secrets(); len(got) != benchSecrets {
			b.Fatalf(
				"the bench fixture's secret references: got %d, want %d",
				len(got),
				benchSecrets,
			)
		}

		scope := entity.SettingScope{
			Kind: entity.ScopeShare,
			ID:   shares[1].ID,
		}
		key := benchPrefixes[0] + ".replicas"

		b.ReportAllocs()
		b.ResetTimer()

		for range b.N {
			b.StopTimer()

			prev := h.Node().Settings()
			benchFlip(b, h, scope, key)
			commit := counters(b, h).Settings

			b.StartTimer()

			moved := h.Node().Moved()

			h.Node().Wake()

			select {
			case <-moved:

			case <-time.After(time.Hour): // a backstop, as benchPass's; a slow one is the figure
				b.Fatalf("reconcile pass: not ended after an hour")
			}

			b.StopTimer()

			if got := h.Node().Refused(); len(got) != 0 {
				b.Fatalf("the timed pass refused %+v, want none", got)
			}

			if got := h.Node().Positions().Settings; got < commit {
				b.Fatalf("settings position after the pass: got %d, want at least %d", got, commit)
			}

			want := []settings.Change{{
				Section: benchPrefixes[0],
				Key:     key,
				Class:   settings.Live,
				Scope:   scope,
			}}
			if got := settings.Diff(prev, h.Node().Settings()); !slices.Equal(got, want) {
				b.Fatalf(
					"the change the snapshot holds: got %+v, want only share %s's new replicas",
					got,
					shares[1].ID,
				)
			}
		}
	})
}

// benchCreate creates benchShares shares, each in a namespace of its own on
// storeA, a thousand to a write.
func benchCreate(b *testing.B, h *nodetest.Harness) []entity.Share {
	b.Helper()
	shares := make([]entity.Share, 0, benchShares)
	for len(shares) < benchShares {
		names := make([]string, min(1000, benchShares-len(shares)))
		for i := range names {
			names[i] = fmt.Sprintf("s%d", len(shares)+i)
		}
		shares = append(shares, createShares(b, h, storeA, names...)...)
	}
	return shares
}

// benchPass wakes reconcile, as a write on this node does, and waits for the
// pass to end, failing unless it reconciled the share list through want.
func benchPass(b *testing.B, h *nodetest.Harness, want uint64) {
	b.Helper()
	moved := h.Node().Moved()
	h.Node().Wake()
	select {
	case <-moved:
	case <-time.After(time.Hour): // a backstop for a pass that never ends; a slow one is the figure
		b.Fatalf("reconcile pass: not ended after an hour")
	}
	if got := h.Node().Positions().ShareList; got < want {
		b.Fatalf("share list position after the pass: got %d, want at least %d", got, want)
	}
}

// benchBound fails unless reconcile bound share id in state.
func benchBound(b *testing.B, h *nodetest.Harness, id entity.ShareID, state entity.ShareState) {
	b.Helper()
	if got, ok := h.Node().Bound(id); !ok || got != state {
		b.Fatalf("share %s after the pass: bound %v in %v, want bound in %v", id, ok, got, state)
	}
}

// benchPrefixes are the fixture's four share sections, in the order a share's
// index chooses among them.
var benchPrefixes = []string{"bench.alpha", "bench.beta", "bench.gamma", "bench.delta"}

// The fixture's other sections: the installation's, the node's, the
// per-journal one and the share-scope one whose single key holds a password
// reference.
const (
	benchInstall = "bench.install"
	benchNode    = "bench.node"
	benchJournal = "bench.journal"
	benchSecret  = "bench.secret"
)

// benchJournals are the two journal things the per-journal section seeds, in
// hex as their rows hold them.
var benchJournals = []string{
	"3f2a00000000000000000000000000a1",
	"5c2000000000000000000000000000b2",
}

// benchSecrets is how many shares the password reference is set on.
const benchSecrets = 16

// benchFive is the one struct the fixture's six fielded sections declare,
// five fields each: integers, strings and booleans, no size or duration.
type benchFive struct {
	Replicas int    `json:"replicas" class:"live"`
	Quorum   int    `json:"quorum"   class:"live"`
	Ratio    uint32 `json:"ratio"    class:"live"`
	Tag      string `json:"tag"      class:"live"`
	Strict   bool   `json:"strict"   class:"live"`
}

// Validate refuses a value out of range, one check per field that has a range;
// a boolean's two values are both inside any range.
func (f benchFive) Validate() error {
	switch {
	case f.Replicas > 64:
		return fmt.Errorf("replicas must be at most 64, got %d", f.Replicas)
	case f.Quorum > 9:
		return fmt.Errorf("quorum must be at most 9, got %d", f.Quorum)
	case f.Ratio > 999:
		return fmt.Errorf("ratio must be at most 999, got %d", f.Ratio)
	case len(f.Tag) > 32:
		return fmt.Errorf("tag must hold at most 32 bytes, got %d", len(f.Tag))
	}

	return nil
}

// benchCred is the secret section's value: one reference to a password of the
// share the row runs at.
type benchCred struct {
	Token entity.SecretRef `json:"token" class:"live"`
}

func (benchCred) Validate() error {
	return nil
}

// benchFields are benchFive's json names, the keys a seeded row holds.
var benchFields = []string{"replicas", "quorum", "ratio", "tag", "strict"}

// benchValue is the value the field named holds for the share with index i,
// as its row stores it.
func benchValue(name string, i int) string {
	switch name {
	case "replicas":
		return strconv.Itoa(1 + i%4)
	case "quorum":
		return strconv.Itoa(i % 5)
	case "ratio":
		return strconv.Itoa(i % 1000)
	case "strict":
		return strconv.FormatBool(i%2 == 0)
	case "tag":
		return `"tag-` + strconv.Itoa(i%16) + `"`
	}

	return "null"
}

// benchSeeded are the sections the share with index i seeds: the first i%5
// of the four, so a share holds 10 rows on average and 20 at most.
func benchSeeded(i int) []string {
	return benchPrefixes[:i%5]
}

// benchDeclare declares the fixture's sections beside the node's own, each
// with an apply that takes every change, as a component whose reach the node
// knows does. Call it before Start.
func benchDeclare(h *nodetest.Harness) {
	apply := func(*settings.Snapshot) error {
		return nil
	}

	secs := []node.Declared{
		{
			Prefix:  benchInstall,
			Section: settings.Of(benchInstall, settings.At(entity.ScopeInstallation), benchFive{}),
			Apply:   apply,
		},
		{
			Prefix:  benchNode,
			Section: settings.Of(benchNode, settings.At(entity.ScopeNode), benchFive{}),
			Apply:   apply,
		},
		{
			Prefix: benchJournal,
			Section: settings.Of(
				benchJournal,
				settings.PerThing(entity.ScopeNode, "journal"),
				benchFive{},
			),
			Apply: apply,
		},
		{
			Prefix: benchSecret,
			Section: settings.Of(benchSecret, settings.At(entity.ScopeShare), benchCred{},
				settings.SecretRef("Token", entity.SecretPassword)),
			Apply: apply,
		},
	}

	for _, prefix := range benchPrefixes {
		secs = append(secs, node.Declared{
			Prefix:  prefix,
			Section: settings.Of(prefix, settings.At(entity.ScopeShare), benchFive{}),
			Apply:   apply,
		})
	}

	h.Node().DeclareSections(secs...)
}

// benchSeal seals a share's password as the settings route would: under the
// storage role, of the password kind, owned by the share's token row, and
// commits it, answering the reference row's value.
func benchSeal(
	b *testing.B,
	h *nodetest.Harness,
	p *secrets.Provider,
	share entity.Share,
) string {
	b.Helper()

	sealed, err := p.Seal(
		b.Context(),
		secrets.Storage,
		entity.SecretPassword,
		entity.SecretOwner{
			Scope: entity.SettingScope{
				Kind: entity.ScopeShare,
				ID:   share.ID,
			},
			Key: benchSecret + ".token",
		},
		entity.SecretRef(entity.NewShareID()),
		[]byte("bench-password"),
		secrets.Meta{},
	)
	if err != nil {
		b.Fatalf("seal the bench password: %v", err)
	}

	err = h.Store().
		ControlPlane().
		Write(b.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			return tx.PutSecret(sealed, sealed.Version-1)
		})
	if err != nil {
		b.Fatalf("commit the bench password: %v", err)
	}

	value, err := json.Marshal(sealed.ID)
	if err != nil {
		b.Fatalf("write the bench password's reference: %v", err)
	}

	return string(value)
}

// benchFixture seeds the settings rows the walks read, as a node's own writes
// make them: each share's rows in one write, the first 16 also carrying the
// share-scope password reference, then the installation's, the node's and the
// journals' rows in one write, all before Start. Call it before Start.
func benchFixture(b *testing.B, h *nodetest.Harness, shares []entity.Share) {
	b.Helper()

	p := h.Secrets(b)

	for i, share := range shares {
		scope := entity.SettingScope{
			Kind: entity.ScopeShare,
			ID:   share.ID,
		}
		seeded := benchSeeded(i)

		var ref string
		if i < benchSecrets {
			ref = benchSeal(b, h, p, share)
		}

		if len(seeded) == 0 && ref == "" {
			continue
		}

		err := h.Store().ControlPlane().Write(
			b.Context(),
			entity.WriteMeta{},
			func(tx entity.ControlPlaneTx) error {
				for _, prefix := range seeded {
					for _, name := range benchFields {
						row := entity.Setting{
							Scope:  scope,
							Key:    prefix + "." + name,
							Value:  []byte(benchValue(name, i)),
							Schema: 1,
						}

						if _, err := tx.PutSetting(row, 0); err != nil {
							return err
						}
					}
				}

				if ref != "" {
					row := entity.Setting{
						Scope:  scope,
						Key:    benchSecret + ".token",
						Value:  []byte(ref),
						Schema: 1,
					}

					if _, err := tx.PutSetting(row, 0); err != nil {
						return err
					}
				}

				return nil
			},
		)
		if err != nil {
			b.Fatalf("seed share %d's settings: %v", i, err)
		}
	}

	nodeScope := entity.SettingScope{
		Kind: entity.ScopeNode,
		ID:   h.NodeID(),
	}
	scopes := []struct {
		scope   entity.SettingScope
		section string
	}{
		{entity.SettingScope{Kind: entity.ScopeInstallation}, benchInstall},
		{nodeScope, benchNode},
	}

	err := h.Store().
		ControlPlane().
		Write(b.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			for _, at := range scopes {
				for _, name := range benchFields {
					row := entity.Setting{
						Scope:  at.scope,
						Key:    at.section + "." + name,
						Value:  []byte(benchValue(name, 0)),
						Schema: 1,
					}
					if _, err := tx.PutSetting(row, 0); err != nil {
						return err
					}
				}
			}

			for _, journal := range benchJournals {
				for _, name := range benchFields {
					row := entity.Setting{
						Scope:  nodeScope,
						Key:    benchJournal + "." + journal + "." + name,
						Value:  []byte(benchValue(name, 0)),
						Schema: 1,
					}
					if _, err := tx.PutSetting(row, 0); err != nil {
						return err
					}
				}
			}

			return nil
		})
	if err != nil {
		b.Fatalf("seed the installation's, node's and journals' settings: %v", err)
	}
}

// benchFlip rewrites the settings row at scope and key to the other of two
// values, as a settings change on a share does, keeping the row's generation.
func benchFlip(b *testing.B, h *nodetest.Harness, scope entity.SettingScope, key string) {
	b.Helper()

	err := h.Store().
		ControlPlane().
		Write(b.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			old, found, err := tx.Setting(scope, key)
			if err != nil {
				return err
			}

			next := []byte("3")
			if found && bytes.Equal(old.Value, next) {
				next = []byte("2")
			}

			row := entity.Setting{
				Scope:  scope,
				Key:    key,
				Value:  next,
				Schema: old.Schema,
			}

			_, err = tx.PutSetting(row, old.Generation)

			return err
		})
	if err != nil {
		b.Fatalf("flip %s: %v", key, err)
	}
}
