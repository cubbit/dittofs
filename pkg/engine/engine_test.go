package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// fullDeps is a Deps with every member present. The interfaces are left
// unimplemented: New only checks that each is there.
func fullDeps() Deps {
	type (
		existence struct{ Existence }
		content   struct{ Content }
		lifecycle struct{ Lifecycle }
		holds     struct{ CloneHold }
		clock     struct{ Clock }
	)
	return Deps{
		Node:    entity.NodeID{1},
		Devices: []string{"/dev0"},
		Open: func(context.Context, string, journal.Version, journal.JournalID, []entity.SyncMark) (Journal, error) {
			return nil, nil
		},
		Existence: existence{},
		Content:   content{},
		Lifecycle: lifecycle{},
		Holds:     holds{},
		Clock:     clock{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, nil
		},
	}
}

func TestNewRefusesEachMissingMemberByName(t *testing.T) {
	if _, err := New(fullDeps()); err != nil {
		t.Fatalf("New(full Deps) = %v", err)
	}
	typ := reflect.TypeFor[Deps]()
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if name == "VersionBlock" || name == "FreeSpace" { // optional: zero is the default
			continue
		}
		t.Run(name, func(t *testing.T) {
			d := fullDeps()
			reflect.ValueOf(&d).Elem().Field(i).SetZero()
			e, err := New(d)
			if err == nil || e != nil {
				t.Fatalf("New without %s = %v, %v; want refused", name, e, err)
			}
			if !strings.Contains(err.Error(), "Deps."+name+" ") {
				t.Fatalf("New without %s: error %q does not name it", name, err)
			}
		})
	}
}

func TestNewRefusesEmptyDevices(t *testing.T) {
	d := fullDeps()
	d.Devices = []string{}
	if _, err := New(d); err == nil || !strings.Contains(err.Error(), "Deps.Devices ") {
		t.Fatalf("New with no device = %v; want Devices refused", err)
	}
}

// An unbuilt call answers one of the filesystem's errors, which its caller
// already maps, never one of the engine's own.
func TestUnbuiltIsUnavailable(t *testing.T) {
	if !errors.Is(ErrUnbuilt, vfserr.ErrUnavailable) {
		t.Errorf("the unbuilt answer %v is not vfserr.ErrUnavailable", ErrUnbuilt)
	}

	if ErrUnbuilt == vfserr.ErrUnavailable {
		t.Error("the unbuilt answer is vfserr.ErrUnavailable itself, so a test cannot tell it from an outage")
	}
}
