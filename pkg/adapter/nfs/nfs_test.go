package nfs_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/nfs"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

func valid() settings.NFS {
	s := settings.DefaultNFS()
	s.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	return s
}

// deps is the Deps New requires, VFS and Budgets, and nothing else. VFS is the
// filesystem over memory.
func deps(t *testing.T) adapter.Deps {
	return adapter.Deps{VFS: memory.New(t), Budgets: &resources.Budgets{}}
}

// New refuses Deps missing VFS or Budgets with a nil adapter, naming every
// field missing and no other, and accepts every other field nil.
func TestNewRequiresVFSAndBudgets(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)
	for _, c := range []struct {
		name         string
		d            adapter.Deps
		missing, set []string
	}{
		{"empty", adapter.Deps{}, []string{"Deps.VFS", "Deps.Budgets"}, nil},
		{"no VFS", adapter.Deps{Budgets: &resources.Budgets{}}, []string{"Deps.VFS"}, []string{"Deps.Budgets"}},
		{"no Budgets", adapter.Deps{VFS: deps(t).VFS}, []string{"Deps.Budgets"}, []string{"Deps.VFS"}},
	} {
		a, err := nfs.New(c.d, valid())
		if a != nil || err == nil {
			t.Errorf("%s: New: %v, %v; want a nil adapter, refused", c.name, a, err)
			continue
		}
		for _, f := range c.missing {
			if !strings.Contains(err.Error(), f) {
				t.Errorf("%s: New: %q, want it to name %s", c.name, err, f)
			}
		}
		for _, f := range c.set {
			if strings.Contains(err.Error(), f) {
				t.Errorf("%s: New: %q, want it not to name %s, which is set", c.name, err, f)
			}
		}
	}
	if a, err := nfs.New(deps(t), valid()); a == nil || err != nil {
		t.Errorf("New with only VFS and Budgets: %v, %v; want an adapter", a, err)
	}
}

// New refuses an invalid setting with a nil adapter and the validator's error,
// naming the field, and builds the adapter, configured, from a valid one.
func TestNewValidatesFirst(t *testing.T) {
	adaptertest.CheckLeaks(t, nil)
	bad := valid()
	bad.TLS.Mode = "always"
	if a, err := nfs.New(deps(t), bad); a != nil || err == nil || !strings.Contains(err.Error(), "TLS.Mode") {
		t.Errorf("New with a bad TLS.Mode: %v, %v; want a nil adapter and the validator's error naming TLS.Mode", a, err)
	}
	if a, err := nfs.New(deps(t), valid()); err != nil || a == nil || a.Health().State != adapter.StateConfigured {
		t.Errorf("New with valid settings: %v, %v; want an adapter, configured", a, err)
	}
}
