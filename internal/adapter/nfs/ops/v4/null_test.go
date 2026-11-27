package v4

import (
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsharness"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// NULL calls nothing and answers nothing: the table is labelled nfs4, its row
// at 0 needs nothing, the runner's record stays empty and the reply body
// carries no bytes. The header stage hands a non-COMPOUND procedure its
// minor version 0 (RFC 5531 section 9).
func TestNullMakesNoCall(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	tab := Table(&Handlers{VFS: rn.Mem})

	if tab.Protocol != "nfs4" {
		t.Errorf("the table is labelled %q, want nfs4", tab.Protocol)
	}

	row, ok := tab.Ops[0]

	if !ok || row.Name != "NULL" || row.Needs != core.NeedsNothing {
		t.Errorf("the table holds %v at 0, want its NULL row needing nothing", row)
	}

	got := nfsharness.Row(t, rn, tab, wire.ProgNFS, 0,
		&core.Request{Version: 0, State: &protocol.State{}}, nil, adaptertest.Expect{})

	if len(got) != 0 {
		t.Errorf("NULL answered %x, want no bytes", got)
	}

	if ops := rn.Mem.Ops(); len(ops) != 0 {
		t.Errorf("NULL recorded %d operations, want none", len(ops))
	}
}
