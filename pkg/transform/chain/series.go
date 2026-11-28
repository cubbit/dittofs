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
// build, so a call finds its handle by transform ID, with no label lookup
// (RFC 25 §2.9).
type series struct {
	// byID is every registered transform's recorder, configured or not,
	// since a body may name any of them.
	byID map[transform.ID]TransformRecorder
	// envelope records a decode refusal that belongs to no transform: a body
	// whose envelope does not parse, that lacks a required transform, or that
	// holds no transform and runs past the limit.
	envelope TransformRecorder
}

// resolveSeries calls rec.For once for each transform of the decode set, in
// ID order, then once for envelope. It returns, with the name, the first For
// that gives no recorder. Every ID in set is in r, which only grows.
func (r *registry) resolveSeries(rec Recorder, set *decodeSet) (series, string, error) {
	r.mu.Lock()
	names := make(map[transform.ID]string, len(set.byID))
	for id := range set.byID {
		names[id] = r.byID[id].name
	}
	r.mu.Unlock()
	s := series{byID: make(map[transform.ID]TransformRecorder, len(names))}
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
	return s, "", nil
}

// reasonOf is the error label of err, one of the chain's own decode errors.
// Unknown material is matched first, as decodeErr wraps it, since it is also
// unavailable material and the label tells it apart (RFC 5 §6).
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
