package smb_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/adapter/smb"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

func valid() settings.SMB {
	s := settings.DefaultSMB()
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
	for _, c := range []struct {
		name         string
		d            adapter.Deps
		missing, set []string
	}{
		{"empty", adapter.Deps{}, []string{"Deps.VFS", "Deps.Budgets"}, nil},
		{"no VFS", adapter.Deps{Budgets: &resources.Budgets{}}, []string{"Deps.VFS"}, []string{"Deps.Budgets"}},
		{"no Budgets", adapter.Deps{VFS: deps(t).VFS}, []string{"Deps.Budgets"}, []string{"Deps.VFS"}},
	} {
		a, err := smb.New(c.d, valid())
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
	if a, err := smb.New(deps(t), valid()); a == nil || err != nil {
		t.Errorf("New with only VFS and Budgets: %v, %v; want an adapter", a, err)
	}
}

// New refuses an invalid setting with the validator's error, naming the
// field, and a nil adapter; with valid settings it returns the adapter
// configured, no address bound.
func TestNewValidatesFirst(t *testing.T) {
	bad := valid()
	bad.Dialects.Min = "2.0.2"
	if a, err := smb.New(deps(t), bad); a != nil || err == nil || !strings.Contains(err.Error(), "Dialects.Min") {
		t.Errorf("New with a bad Dialects.Min: %v, %v; want a nil adapter and the validator's error naming Dialects.Min", a, err)
	}
	a, err := smb.New(deps(t), valid())
	if a == nil || err != nil {
		t.Fatalf("New with valid settings: %v, %v; want an adapter", a, err)
	}
	if st, addrs := a.Health().State, a.Addrs(); st != adapter.StateConfigured || len(addrs) != 0 {
		t.Errorf("New with valid settings: state %v, bound on %v; want configured, bound nowhere", st, addrs)
	}
}
