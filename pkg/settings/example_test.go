package settings_test

import (
	"context"
	"errors"
	"iter"

	adaptersettings "github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// The adapters' sections: installation scope, with a node override for the
// port only (RFC 13 Appendix B). Compiled, never run: a changed signature of
// Of, Override or OverrideOnly fails the build.
func ExampleOf() {
	scope := settings.Override(entity.ScopeInstallation, entity.ScopeNode)

	_ = settings.Table{
		settings.Of(
			"adapters.nfs",
			scope,
			adaptersettings.DefaultNFS(),
			settings.OverrideOnly("Port"),
		),
		settings.Of(
			"adapters.smb",
			scope,
			adaptersettings.DefaultSMB(),
			settings.OverrideOnly("Port"),
		),
	}
}

// idRange is a section's settings with a range, a required key and a secret
// reference.
type idRange struct {
	Lo         uint32           `json:"lo"         class:"live"`
	Hi         uint32           `json:"hi"         class:"live"`
	Credential entity.SecretRef `json:"credential" class:"live"`
}

func (r idRange) Validate() error {
	if r.Lo > r.Hi {
		return errors.New("lo above hi")
	}

	return nil
}

// Every option on one section, and a per-thing scope. Compiled, never run: a
// changed signature of any option, of At or PerThing, or of the Write a
// CheckTx is handed fails the build.
func ExampleOf_options() {
	checkRange := func(tx entity.ControlPlaneTx, w settings.Write, effective idRange) error {
		if w.Key != "test.options.lo" {
			return nil
		}

		_, _, err := tx.Setting(w.Scope, w.Key)

		return err
	}

	_ = settings.Table{
		settings.Of(
			"test.options",
			settings.At(entity.ScopeInstallation),
			idRange{Hi: 1},
			settings.WriteRole(entity.RoleAdmin),
			settings.Required("Lo"),
			settings.CheckTx(checkRange),
			settings.SecretRef("Credential", entity.SecretRemoteCredential),
		),
		settings.Of(
			"test.journal",
			settings.PerThing(entity.ScopeNode, "journal"),
			idRange{Hi: 1},
		),
	}
}

// The node's read and the API's write check. Compiled, never run: a changed
// signature of Read, Check or HoldsContent, or a Change without its Class,
// fails the build.
func ExampleRead() {
	table := settings.Table{
		settings.Of(
			"adapters.smb",
			settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			adaptersettings.DefaultSMB(),
			settings.OverrideOnly("Port"),
		),
	}

	var scan func(context.Context) iter.Seq2[entity.Setting, error] // the node's cp.AllSettings

	snap, refused, err := settings.Read(context.Background(), scan, table, entity.NodeID{})
	if err != nil || len(refused) > 0 {
		return
	}

	for _, ch := range settings.Diff(nil, snap) {
		if ch.Class == settings.Restart {
			return
		}
	}

	var tx entity.ControlPlaneTx // the write's transaction

	holds := settings.HoldsContent(func(scope entity.SettingScope) (bool, error) {
		return false, nil
	})
	if _, err := holds(entity.SettingScope{Kind: entity.ScopeInstallation}); err != nil {
		return
	}

	w := settings.Write{
		Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
		Key:   "adapters.smb.port",
	}

	_ = table.Check(tx, entity.RoleOperator, w, holds)
}
