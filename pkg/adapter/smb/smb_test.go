package smb_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
	"github.com/cubbit/dittofs/pkg/adapter/smb"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

func valid() config.SMB {
	s := config.DefaultSMB()
	s.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	return s
}

// deps is the Deps New requires, Service and Budgets, and nothing else.
func deps() adapter.Deps {
	return adapter.Deps{Service: vfstest.Null{}, Budgets: &resources.Budgets{}}
}

// New refuses Deps missing Service or Budgets, naming the field, and accepts
// every other field nil.
func TestNewRequiresServiceAndBudgets(t *testing.T) {
	for field, d := range map[string]adapter.Deps{
		"Deps.Service": {Budgets: &resources.Budgets{}},
		"Deps.Budgets": {Service: vfstest.Null{}},
	} {
		if _, err := smb.New(d, valid()); err == nil || errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), field) {
			t.Errorf("New without %s: %v, want refused naming %s", field, err, field)
		}
	}
	if _, err := smb.New(deps(), valid()); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with only Service and Budgets: %v, want accepted as far as errors.ErrUnsupported", err)
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
