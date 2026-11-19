package node

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// TestDecide pins the start check's verdict: one error per row, judged by
// sentinel and by the facts the message names. Rows asserting absence guard
// the two operator traps the not-initialised and disagree messages must not
// fall into.
func TestDecide(t *testing.T) {
	instA := journal.InstallationID{0xAA, 0x01}
	instB := journal.InstallationID{0xBB, 0x02}
	corruptFormat := fmt.Errorf("%w: the format file is damaged", journal.ErrCorrupt)
	ident := func(inst journal.InstallationID, dir string, segments bool) deviceFacts {
		return deviceFacts{Dir: dir, OpenErr: &journal.IdentityError{Installation: inst}, Segments: segments}
	}
	noJournal := func(dir string) deviceFacts {
		return deviceFacts{Dir: dir, OpenErr: journal.ErrNoJournal}
	}

	rows := []struct {
		name     string
		facts    startFacts
		want     error
		contains []string
		misses   []string
		neverIs  []error
	}{
		{
			name:  "v1 state found",
			facts: startFacts{V1: "/x/blocks/shares/s1/journal"},
			want:  errV1State,
			contains: []string{
				"/x/blocks/shares/s1/journal",
			},
		},
		{
			name: "v1 state outranks a lost store",
			facts: startFacts{
				V1:       "/x/blocks/shares/s1/journal",
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{ident(instA, "/d1", false)},
			},
			want:     errV1State,
			contains: []string{"/x/blocks/shares/s1/journal"},
			neverIs:  []error{errStoreLost},
		},
		{
			name: "store unidentifiable",
			facts: startFacts{
				Store:    storeUnidentifiable,
				StoreErr: errors.New("permission denied"),
				StoreDir: "/s",
				Devices:  []deviceFacts{noJournal("/d1")},
			},
			want: errUnidentifiable,
			contains: []string{
				"/s",
				"permission denied",
			},
			misses: []string{"dfs init"},
		},
		{
			name: "unidentifiable store outranks a lost one",
			facts: startFacts{
				Store:    storeUnidentifiable,
				StoreErr: errors.New("permission denied"),
				StoreDir: "/s",
				Devices:  []deviceFacts{ident(instA, "/d1", false)},
			},
			want:     errUnidentifiable,
			contains: []string{"/s", "permission denied"},
			neverIs:  []error{errStoreLost},
		},
		{
			name: "device unidentifiable",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{{Dir: "/d1", OpenErr: journal.ErrCorrupt}},
			},
			want: errUnidentifiable,
			contains: []string{
				"/d1",
				journal.ErrCorrupt.Error(),
			},
		},
		{
			name: "corrupt format beside absent store",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{{Dir: "/d1", OpenErr: corruptFormat}},
			},
			want:    errUnidentifiable,
			neverIs: []error{errNotInitialised},
		},
		{
			name: "segments without a format file",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{{Dir: "/d1", OpenErr: journal.ErrUnidentified}},
			},
			want: errUnidentifiable,
			contains: []string{
				"/d1",
			},
		},
		{
			name: "devices disagree",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{ident(instA, "/d1", false), ident(instB, "/d2", false)},
			},
			want: errDevicesDisagree,
			contains: []string{
				"aa01",
				"bb02",
				"/d1",
				"/d2",
			},
			neverIs: []error{errStoreLost},
		},
		{
			name: "mixed set, unidentifiable beats the rest",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{ident(instA, "/d1", false), {Dir: "/d2", OpenErr: journal.ErrCorrupt}, ident(instB, "/d3", false)},
			},
			want:    errUnidentifiable,
			neverIs: []error{errDevicesDisagree, errStoreLost},
		},
		{
			name: "mixed set reversed, same verdict",
			facts: startFacts{
				Store:   storeAbsent,
				Devices: []deviceFacts{ident(instB, "/d3", false), {Dir: "/d2", OpenErr: journal.ErrCorrupt}, ident(instA, "/d1", false)},
			},
			want:    errUnidentifiable,
			neverIs: []error{errDevicesDisagree, errStoreLost},
		},
		{
			name: "store lost, devices hold segments",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{ident(instA, "/d1", true), ident(instA, "/d2", false)},
			},
			want: errStoreLost,
			contains: []string{
				"/s",
				"aa01",
				"check the store volume's mount first",
				"restore",
				"loses every write since",
				"segment",
			},
		},
		{
			name: "store lost, no segments",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{ident(instA, "/d1", false), ident(instA, "/d2", false)},
			},
			want: errStoreLost,
			contains: []string{
				"/s",
				"aa01",
				"check the store volume's mount first",
				"restore",
				"loses every write since",
			},
			misses: []string{"segment"},
		},
		{
			name: "some devices hold no journal, one names the installation",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{noJournal("/d1"), ident(instA, "/d2", false)},
			},
			want: errStoreLost,
		},
		{
			name: "nothing initialised",
			facts: startFacts{
				Store:    storeAbsent,
				StoreDir: "/s",
				Devices:  []deviceFacts{noJournal("/d1"), noJournal("/d2")},
			},
			want: errNotInitialised,
			contains: []string{
				"mount",
				"dfs init",
			},
			misses: []string{"/s", "installation"},
		},
		{
			name:  "store present",
			facts: startFacts{Store: storePresent},
			want:  nil,
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got := decide(r.facts)
			if !errors.Is(got, r.want) {
				t.Fatalf("decide's verdict: got %v, want %v", got, r.want)
			}
			if got == nil {
				return
			}
			for _, never := range r.neverIs {
				if errors.Is(got, never) {
					t.Errorf("decide's verdict: got %v, which must never wrap %v", got, never)
				}
			}
			for _, want := range r.contains {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("verdict message must contain %q: got %q", want, got.Error())
				}
			}
			for _, miss := range r.misses {
				if strings.Contains(got.Error(), miss) {
					t.Errorf("verdict message must not contain %q: got %q", miss, got.Error())
				}
			}
		})
	}
}
