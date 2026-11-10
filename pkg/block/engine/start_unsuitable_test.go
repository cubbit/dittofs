package engine_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// unsuitable is the refusal a journal answers for a directory the platform
// profile refuses: the sentinel, wrapped with the rule and the directory.
func unsuitable() error {
	return fmt.Errorf(
		"journal: directory %s refused: mount has no barriers: %w",
		startDevice,
		journal.ErrUnsuitableStorage,
	)
}

// conditionOf is the condition named name that st raises for the start
// device, and whether there is one.
func conditionOf(st engine.Stats, name string) (engine.Condition, bool) {
	for _, c := range st.Journals[startDevice].Conditions {
		if c.Name == name {
			return c, true
		}
	}

	return engine.Condition{}, false
}

// A journal whose Open refuses the directory as unsuitable does not fail
// Start: its shares are not attached, Stats names each with the device and
// the rule, and journal_unsuitable is raised (RFC 8 §2.5).
func TestStartLeavesAnUnsuitableJournalsSharesUnattached(t *testing.T) {
	s := newStarter(t)
	s.openErr = unsuitable()

	if err := s.start(t); err != nil {
		t.Fatalf("Start = %v, want nil: a refused journal does not stop the node", err)
	}

	st := s.e.Stats()

	for _, id := range []engine.Share{s.one, s.two} {
		got := st.Shares[id.ID]
		if got.Device != startDevice || !errors.Is(got.Refused, journal.ErrUnsuitableStorage) {
			t.Errorf(
				"share %v: device %q, refused %v; want %q and the rule",
				id.ID, got.Device, got.Refused, startDevice,
			)
		}

		if err := s.write(t, id.ID); !isDelay(err, vfserr.DelayFrozen) {
			t.Errorf("a write on share %v = %v, want ErrDelay(DelayFrozen)", id.ID, err)
		}
	}

	c, ok := conditionOf(st, "journal_unsuitable")
	if !ok || !errors.Is(c.Cause, journal.ErrUnsuitableStorage) {
		t.Errorf("journal_unsuitable = %+v (raised %t), want one naming the rule", c, ok)
	}

	if got := len(st.Journals[startDevice].Conditions); got != 1 {
		t.Errorf("%d conditions on the device, want exactly journal_unsuitable", got)
	}
}

// A removed share's tag is still in the journal waiting for its retried
// open, so its Drop, which forgets the tag there, answers retry-later too:
// ErrUnavailable would not be retried, and the tag would stay in the journal.
func TestADropOfARemovedSharesTagWaitsForTheRetriedOpen(t *testing.T) {
	s := newStarter(t)
	removed := entity.NewShareID()
	s.lc.removed = map[entity.ShareID]entity.TagRecord{
		removed: {
			Tag:   91,
			State: entity.TagOffloaded,
		},
	}
	s.openErr = unsuitable()

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	if err := s.e.Drop(t.Context(), removed); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Drop of a removed share while unattached = %v, want ErrDelay(DelayFrozen)", err)
	}

	if err := s.e.Unbind(t.Context(), removed, false); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Unbind of a removed share while unattached = %v, want ErrDelay(DelayFrozen)", err)
	}
}

// Until the journal's shares attach, a Bind, Unbind or Drop naming a share
// the journal would serve answers retry-later: an Unbind or Drop that
// answered nil would report a share ended while its tag is still in the
// journal (RFC 8 §2.5).
func TestAnUnsuitableJournalsSharesAnswerRetryLaterToLifecycleCalls(t *testing.T) {
	s := newStarter(t, withSpare())
	s.openErr = unsuitable()

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	if err := s.e.Unbind(t.Context(), s.one.ID, false); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Unbind of a refused share = %v, want ErrDelay(DelayFrozen)", err)
	}

	if err := s.e.Drop(t.Context(), s.one.ID); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Drop of a refused share = %v, want ErrDelay(DelayFrozen)", err)
	}

	if err := s.e.Bind(t.Context(), s.spare); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Bind of a share the journal would serve = %v, want ErrDelay(DelayFrozen)", err)
	}
}
