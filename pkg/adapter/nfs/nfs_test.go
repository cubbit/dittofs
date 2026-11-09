package nfs_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
	"github.com/cubbit/dittofs/pkg/adapter/nfs"
)

func valid() config.NFS {
	s := config.NFS{Adapter: config.Adapter{
		Addrs:             []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		DrainDeadline:     90 * time.Second,
		IdleTimeout:       15 * time.Minute,
		WriteDeadline:     60 * time.Second,
		HandshakeDeadline: 30 * time.Second,
		FrameDeadline:     30 * time.Second,
	}}
	s.TLS.MinVersion, s.TLS.Mode = "1.3", "opportunistic"
	return s
}

// New refuses an invalid setting with the validator's error, naming the field,
// and answers a valid one as not built yet.
func TestNewValidatesFirst(t *testing.T) {
	bad := valid()
	bad.TLS.Mode = "always"
	if a, err := nfs.New(adapter.Deps{}, bad); a != nil || err == nil || !strings.Contains(err.Error(), "TLS.Mode") || errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with a bad TLS.Mode: %v, %v; want the validator's error naming TLS.Mode", a, err)
	}
	if a, err := nfs.New(adapter.Deps{}, valid()); a != nil || !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("New with valid settings: %v, %v; want errors.ErrUnsupported", a, err)
	}
}
