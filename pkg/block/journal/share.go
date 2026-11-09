package journal

import "context"

// Share attaches the share whose tag is tag and returns a handle through which
// its engine works. Every record the handle appends carries tag, and a file
// belongs, for as long as any segment holds one of its records, to the tag of
// its records.
//
// floor is the larger of the share's version floor, the highest content
// version recorded for any file of the share, and the share's durable version
// limit. Before it returns, Share raises the journal's version counter above
// floor, so no write through the handle is given a version below one the
// share's files already hold, nor one issued before a crash whose record was
// lost.
//
// limit bounds the record bytes of the share's records on disk; zero means no
// limit below the journal's maximum, and a negative limit is refused with
// ErrInvalidSetting. A floor with a non-zero epoch is refused with
// ErrUnknownFormat, and tag 0 with ErrInvalidSetting; a refusal attaches
// nothing.
//
// A tag has one handle open at a time. Share of a tag whose handle is open
// returns that same handle, with its limit replaced by limit and the floors
// raised to floor; after the handle's Close, a later Share makes a new handle.
func (j *Journal) Share(tag ShareTag, limit int64, floor Version) (*Handle, error) {
	// decision: a Journal with no core, the zero value, refuses Share with
	// ErrNotImplemented instead of panicking, and the other methods are not
	// guarded. Only the engine's tests build one, to see a refused Share answer
	// nil, because no call yet returns a Journal. Withdraw this when Open does,
	// and give those tests a journal that refuses for a real reason.
	if j.core == nil {
		return nil, ErrNotImplemented
	}
	// The map lock is held across the core's attach so that a share the core
	// returns is never one whose Close already removed its handle: Close
	// removes the entry only after the core dropped the share, so an attach
	// that sees the share also sees its entry.
	j.handlesMu.Lock()
	defer j.handlesMu.Unlock()
	s, err := j.core.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}
	if h, ok := j.handles[s]; ok {
		return h, nil
	}
	h := &Handle{j: j, s: s}
	j.handles[s] = h
	return h, nil
}

// Forget drops tag whole: every held and retained extent, removal marker and
// hold mark of the tag's files. It appends one header-only forget record,
// drawn on the reserved headroom and durable before Forget returns, so
// recovery never rebuilds any of them. Nothing is left to settle, and Since
// never reports the drop.
//
// Forget is refused with ErrHandleOpen while a handle of tag is open, and with
// a NoSpaceError naming LimitHeadroom when the headroom is exhausted. It is
// idempotent: forgetting a tag that holds nothing succeeds.
//
// The journal never decides on its own which tags to forget: a tag the
// caller still records may hold the only copy of dirty content.
func (j *Journal) Forget(ctx context.Context, tag ShareTag) error {
	return ErrNotImplemented
}

// Strand marks tag as claimed by no share. From the call on, no read through
// another tag's handle is served from the tag's extents, and none of them is
// evicted, offered or released. A stranded tag stays readable through a
// handle of its own, so its extents can be exported before it is forgotten.
//
// Strand is kept in memory only: the caller names every unclaimed tag again at
// every start, before serving. It appends nothing.
func (j *Journal) Strand(tag ShareTag) error {
	return ErrNotImplemented
}

// Draw issues the next content version and appends nothing, for a change the
// caller stores elsewhere; the caller records it, with the file's floor, in
// the change's own transaction. It takes no file append lock.
//
// Draw is refused with a NoSpaceError naming LimitVersion when the next
// version would reach the version limit, and before the first
// SetVersionLimit, since the journal issues no version without a limit.
func (j *Journal) Draw() (Version, error) {
	return j.core.Draw()
}

// SetVersionLimit sets the version limit: no version is issued at or above
// mark. mark is the smallest of the durable per-share limits the caller has
// recorded for every share the journal serves, raised by the caller ahead of
// use so a write is not refused for want of versions. A mark with a non-zero
// epoch is refused with ErrUnknownFormat and the limit stays as it was.
func (j *Journal) SetVersionLimit(mark Version) error {
	return j.core.SetVersionLimit(mark)
}
