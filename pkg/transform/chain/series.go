package chain

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// series is one chain's handles on RFC 5 §6's series, every one resolved at
// build, so a call records through a handle it already holds, with no label
// lookup (RFC 25 §2.9).
type series struct {
	// steps is each configured step's recorder, in the chain's stage order.
	steps []TransformRecorder
	// byID is every registered transform's recorder, by ID, for a decode,
	// which may undo a transform the chain no longer configures.
	byID map[transform.ID]TransformRecorder
	// envelope records a refusal that belongs to no transform: an envelope
	// that does not parse, a body without what the chain requires, and a
	// body of no transform past the limit.
	envelope TransformRecorder
}

// resolveSeries calls rec.For once for each transform of the decode set, in
// ID order, then once for envelope, and picks each configured step's handle
// from them, so a series exists for every transform a body can name. It
// returns, with the name, the first For that gives no recorder. The registry
// only grows, so every ID in set has a name.
func (r *registry) resolveSeries(rec Recorder, set *decodeSet, steps []transform.Transform) (series, string, error) {
	r.mu.Lock()
	names := make(map[transform.ID]string, len(set.byID))
	for id := range set.byID {
		names[id] = r.byID[id].name
	}
	r.mu.Unlock()
	s := series{steps: make([]TransformRecorder, len(steps)), byID: make(map[transform.ID]TransformRecorder, len(names))}
	for _, id := range slices.Sorted(maps.Keys(names)) {
		t := rec.For(names[id])
		if t == nil {
			return series{}, names[id], fmt.Errorf("chain: Deps.Recorder gives no recorder for transform %q", names[id])
		}
		s.byID[id] = t
	}
	if s.envelope = rec.For(nameEnvelope); s.envelope == nil {
		return series{}, nameEnvelope, fmt.Errorf("chain: Deps.Recorder gives no recorder for %q", nameEnvelope)
	}
	for i, t := range steps {
		s.steps[i] = s.byID[t.ID()]
	}
	return s, "", nil
}

// reasonOf is the error label of err, one of the chain's own decode errors.
// Unknown material is matched first, since it is also unavailable material,
// and the label tells it apart (RFC 5 §6).
func reasonOf(err error) FailureReason {
	switch {
	case errors.Is(err, keys.ErrUnknownMaterial):
		return ReasonMaterialUnknown
	case errors.Is(err, keys.ErrMaterialDestroyed):
		return ReasonMaterialDestroyed
	case errors.Is(err, keys.ErrMaterialUnavailable):
		return ReasonMaterialUnavailable
	case errors.Is(err, transform.ErrTooLarge):
		return ReasonTooLarge
	default:
		return ReasonMalformed
	}
}
