package smb_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
	"github.com/cubbit/dittofs/pkg/adapter/smb"
	"github.com/cubbit/dittofs/pkg/resources"
)

func valid() config.SMB {
	s := config.DefaultSMB()
	s.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	return s
}

// deps is the Deps New requires, VFS and Budgets, and nothing else. New only
// checks VFS is set, so a typed nil of the real filesystem serves.
func deps() adapter.Deps {
	return adapter.Deps{VFS: (*vfs.Filesystem)(nil), Budgets: &resources.Budgets{}}
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
		{"no Budgets", adapter.Deps{VFS: deps().VFS}, []string{"Deps.Budgets"}, []string{"Deps.VFS"}},
	} {
		_, err := smb.New(c.d, valid())
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
	if _, err := smb.New(deps(), valid()); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with only VFS and Budgets: %v, want accepted as far as errors.ErrUnsupported", err)
	}
}

// New refuses an invalid setting with the validator's error, naming the field,
// and answers a valid one as not built yet.
func TestNewValidatesFirst(t *testing.T) {
	bad := valid()
	bad.Dialects.Min = "2.0.2"
	if a, err := smb.New(deps(), bad); a != nil || err == nil || !strings.Contains(err.Error(), "Dialects.Min") || errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with a bad Dialects.Min: %v, %v; want the validator's error naming Dialects.Min", a, err)
	}
	if a, err := smb.New(deps(), valid()); a != nil || !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with valid settings: %v, %v; want errors.ErrUnsupported", a, err)
	}
}
