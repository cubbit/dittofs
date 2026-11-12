// Package memory is the filesystem over memory: internal/vfs.New over the
// in-memory store, real open state and the in-memory Content, each held to
// its own conformance suite, behind vfstest's recording wrapper of the calls
// that may change state. It is the Service a handler is tested against.
//
// decision: it is a package of its own below vfstest rather than part of
// vfstest, because it imports internal/vfs, whose own tests import vfstest:
// inside vfstest it would close an import cycle. Fold it into vfstest if the
// filesystem's tests ever stop importing vfstest.
package memory

import (
	"context"
	"crypto/rand"
	"net/netip"
	"testing"
	"time"

	ivfs "github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/internal/vfs/contenttest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// Service is a serving filesystem over memory. Its Service methods are the
// recording wrapper's, so Ops is each call that may change state it served.
type Service struct {
	*vfstest.Mutations
	Store   *kvstoretest.FS     // the store, with one share, its administrator and its root
	Content *contenttest.Memory // the share's content, bound
	Clock   *sim.TestClock      // the clock open state and Content read
}

// Option sets one input of New.
type Option func(*options)

type options struct {
	clock  *sim.TestClock
	events vfs.Events
}

// WithClock has open state and Content read c; without it they read a test
// clock at the Unix epoch.
func WithClock(c *sim.TestClock) Option { return func(o *options) { o.clock = c } }

// WithEvents is the filesystem's event sink; without it, the null sink.
func WithEvents(e vfs.Events) Option { return func(o *options) { o.events = e } }

// New is a serving filesystem over memory, drained and stopped when the test
// ends.
func New(t *testing.T, opts ...Option) *Service {
	t.Helper()
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.clock == nil {
		o.clock = sim.NewTestClock(time.Unix(0, 0))
	}
	fs := kvstoretest.NewFS(t, memKV)
	// The administrator holds a uid, so that it signs in as itself rather
	// than as the share's anonymous principal.
	admin := entity.User{Principal: fs.Admin.Principal, Name: "admin", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "100"}}}
	must(t, fs.Store.ControlPlane().PutUser(t.Context(), admin))
	tab, err := openstate.New(openstate.Deps{Records: fs.Store, Clock: o.clock, Rand: rand.Reader})
	must(t, err)
	content := contenttest.NewOver(o.clock, fs.Store)
	content.Bind(fs.Share.ID)
	f, err := ivfs.New(ivfs.Deps{Meta: fs.Store, Open: tab, Engine: content, Events: o.events})
	must(t, err)
	must(t, tab.Start(t.Context(), f, f.Admission))
	t.Cleanup(tab.Stop)
	must(t, f.Start(t.Context()))
	t.Cleanup(func() {
		_ = f.Drain(context.Background())
		_ = f.Stop()
	})
	return &Service{Mutations: vfstest.RecordMutations(f), Store: fs, Content: content, Clock: o.clock}
}

// Admin is a call by the share's administrator, proven by a ticket under
// privacy, from 127.0.0.1: one the share admits for anything.
func (s *Service) Admin() vfs.Call {
	return vfs.Call{Identity: vfs.Identity{
		Proof: vfs.ProofTicket, Protection: vfs.ProtectionPrivacy,
		Evidence: vfs.Evidence{Principal: s.Store.Admin.Principal}, Client: netip.AddrFrom4([4]byte{127, 0, 0, 1}),
	}}
}

func memKV(tb testing.TB) kv.KV {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return db
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
