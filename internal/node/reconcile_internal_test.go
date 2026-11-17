package node

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// A pass acts on the shares when the share list or the settings moved, on
// apply when the settings or the principals moved, on both after a failed
// read or a refusal to retry, and on neither when nothing moved, so a
// refusal not to retry waits for its counter to move.
func TestDue(t *testing.T) {
	seen := kvstore.Counters{Settings: 5, ShareList: 5, Principals: 5}
	for _, tc := range []struct {
		name          string
		c             kvstore.Counters
		again         bool
		shares, apply bool
	}{
		{"nothing moved", seen, false, false, false},
		{"settings moved", kvstore.Counters{Settings: 6, ShareList: 5, Principals: 5}, false, true, true},
		{"share list moved", kvstore.Counters{Settings: 5, ShareList: 6, Principals: 5}, false, true, false},
		{"principals moved", kvstore.Counters{Settings: 5, ShareList: 5, Principals: 6}, false, false, true},
		{"nothing moved after a failed read or a refusal to retry", seen, true, true, true},
	} {
		if shares, apply := due(tc.c, seen, tc.again); shares != tc.shares || apply != tc.apply {
			t.Errorf("%s: got shares %v and apply %v, want %v and %v", tc.name, shares, apply, tc.shares, tc.apply)
		}
	}
}

// planShares binds a share not bound or bound in another state, in the
// order of the positions its changes raised, and refuses one with no remote
// store, a bound one included, which keeps its binding; run again over the
// running set its binds leave, it binds nothing.
func TestPlanShares(t *testing.T) {
	share := func(name string, state entity.ShareState, pos uint64) entity.Share {
		return entity.Share{ID: entity.NewShareID(), Name: name, State: state, Position: pos}
	}
	fresh := share("fresh", entity.ShareEnabled, 9)
	quiesced := share("quiesced", entity.ShareQuiesced, 4)
	same := share("same", entity.ShareEnabled, 2)
	lost := share("lost", entity.ShareEnabled, 3)
	nowhere := share("nowhere", entity.ShareEnabled, 7)
	bound := map[entity.ShareID]entity.Share{
		quiesced.ID: {ID: quiesced.ID, State: entity.ShareEnabled},
		same.ID:     same,
		lost.ID:     lost,
	}
	listed := []entity.Share{fresh, quiesced, same, lost, nowhere}
	remote := func(s entity.Share) bool { return s.ID != lost.ID && s.ID != nowhere.ID }

	bind, refusals := planShares(bound, listed, remote)
	if got, want := names(bind), []string{"quiesced", "fresh"}; !slices.Equal(got, want) {
		t.Errorf("binds: got %v, want %v, in position order", got, want)
	}
	var refusedIDs []entity.ShareID
	for _, f := range refusals {
		refusedIDs = append(refusedIDs, entity.ShareID(f.rec.Record.ID))
		if f.retry {
			t.Errorf("refusal of share %x for no remote store: retried, want it to wait for its record to change", f.rec.Record.ID)
		}
	}
	if !slices.Equal(refusedIDs, []entity.ShareID{lost.ID, nowhere.ID}) {
		t.Errorf("refused: got %v, want the bound share that lost its store and the one never bound", refusedIDs)
	}
	if _, ok := bound[lost.ID]; !ok {
		t.Errorf("running set: the share that lost its store was dropped, want it kept")
	}

	for _, s := range bind {
		bound[s.ID] = s
	}
	if again, _ := planShares(bound, listed, remote); len(again) != 0 {
		t.Errorf("binds over the same records again: got %v, want none", names(again))
	}
}

func names(shares []entity.Share) []string {
	var out []string
	for _, s := range shares {
		out = append(out, s.Name)
	}
	return out
}
