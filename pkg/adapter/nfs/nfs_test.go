package nfs_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

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

// New refuses Deps missing VFS or Budgets, naming every field missing and
// no other, and accepts every other field nil.
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
		_, err := nfs.New(c.d, valid())
		if err == nil || errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s: New: %v, want refused", c.name, err)
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
	if _, err := nfs.New(deps(t), valid()); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with only VFS and Budgets: %v, want accepted as far as errors.ErrUnsupported", err)
	}
}

// New refuses an invalid setting with the validator's error, naming the field,
// and answers a valid one as not built yet.
func TestNewValidatesFirst(t *testing.T) {
	bad := valid()
	bad.TLS.Mode = "always"
	if a, err := nfs.New(deps(t), bad); a != nil || err == nil || !strings.Contains(err.Error(), "TLS.Mode") || errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with a bad TLS.Mode: %v, %v; want the validator's error naming TLS.Mode", a, err)
	}
	if a, err := nfs.New(deps(t), valid()); a != nil || !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with valid settings: %v, %v; want errors.ErrUnsupported", a, err)
	}
}
