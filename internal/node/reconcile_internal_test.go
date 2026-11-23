package node

import (
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// A pass acts on the shares when the share list or the settings moved, on
// apply when the settings or the principals moved, on both after a failed
// read or a refusal to retry, and on neither when nothing moved, so a
// refusal not to retry waits for its counter to move.
func TestDue(t *testing.T) {
	t.Parallel()
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
// store, a bound one included, which keeps its binding and still has its
// state change bound; run again over the running set its binds leave, it
// binds nothing.
func TestPlanShares(t *testing.T) {
	t.Parallel()
	share := func(name string, state entity.ShareState, pos uint64) entity.Share {
		return entity.Share{ID: entity.NewShareID(), Name: name, State: state, Position: pos}
	}
	fresh := share("fresh", entity.ShareEnabled, 9)
	quiesced := share("quiesced", entity.ShareQuiesced, 4)
	same := share("same", entity.ShareEnabled, 2)
	lost := share("lost", entity.ShareEnabled, 3)
	lostQuiesced := share("lost quiesced", entity.ShareQuiesced, 5)
	nowhere := share("nowhere", entity.ShareEnabled, 7)
	bound := map[entity.ShareID]entity.Share{
		quiesced.ID:     {ID: quiesced.ID, State: entity.ShareEnabled},
		same.ID:         same,
		lost.ID:         lost,
		lostQuiesced.ID: {ID: lostQuiesced.ID, State: entity.ShareEnabled},
	}
	listed := []entity.Share{fresh, quiesced, same, lost, lostQuiesced, nowhere}
	remote := func(s entity.Share) bool { return s.ID != lost.ID && s.ID != lostQuiesced.ID && s.ID != nowhere.ID }

	bind, refusals := planShares(bound, listed, remote)
	if got, want := names(bind), []string{"quiesced", "lost quiesced", "fresh"}; !slices.Equal(got, want) {
		t.Errorf("binds: got %v, want %v, in position order", got, want)
	}
	var refusedIDs []entity.ShareID
	for _, f := range refusals {
		refusedIDs = append(refusedIDs, entity.ShareID(f.rec.Record.ID))
		if f.retry {
			t.Errorf("refusal of share %x for no remote store: retried, want it to wait for its record to change", f.rec.Record.ID)
		}
	}
	if !slices.Equal(refusedIDs, []entity.ShareID{lost.ID, lostQuiesced.ID, nowhere.ID}) {
		t.Errorf("refused: got %v, want the two bound shares that lost their store and the one never bound", refusedIDs)
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

// The backoff starts at firstRetry, doubles to lastRetry and stays there,
// and ends once a pass needs no retry.
func TestBackoff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		last  time.Duration
		again bool
		want  time.Duration
	}{
		{0, true, time.Second},
		{time.Second, true, 2 * time.Second},
		{2 * time.Second, true, 4 * time.Second},
		{16 * time.Second, true, 30 * time.Second},
		{30 * time.Second, true, 30 * time.Second},
		{8 * time.Second, false, 0},
	} {
		if got := backoff(tc.last, tc.again); got != tc.want {
			t.Errorf("backoff after %v, again %v: got %v, want %v", tc.last, tc.again, got, tc.want)
		}
	}
}

func names(shares []entity.Share) []string {
	var out []string
	for _, s := range shares {
		out = append(out, s.Name)
	}
	return out
}
