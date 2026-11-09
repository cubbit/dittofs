package smb_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
	"github.com/cubbit/dittofs/pkg/adapter/smb"
)

func valid() config.SMB {
	s := config.SMB{Adapter: config.Adapter{
		Addrs:             []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		DrainDeadline:     90 * time.Second,
		IdleTimeout:       15 * time.Minute,
		WriteDeadline:     60 * time.Second,
		HandshakeDeadline: 30 * time.Second,
		FrameDeadline:     30 * time.Second,
	}}
	s.Dialects.Min, s.Dialects.Max = "2.1", "3.1.1"
	return s
}

// New refuses an invalid setting with the validator's error, naming the field,
// and answers a valid one as not built yet.
func TestNewValidatesFirst(t *testing.T) {
	bad := valid()
	bad.Dialects.Min = "2.0.2"
	if a, err := smb.New(adapter.Deps{}, bad); a != nil || err == nil || !strings.Contains(err.Error(), "Dialects.Min") || errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with a bad Dialects.Min: %v, %v; want the validator's error naming Dialects.Min", a, err)
	}
	if a, err := smb.New(adapter.Deps{}, valid()); a != nil || !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with valid settings: %v, %v; want errors.ErrUnsupported", a, err)
	}
}
