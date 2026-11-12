package datapath

import "context"

// beginCall is the one helper every data-path call starts with (RFC 1 §3):
// it checks the context before any refusal check and before any state
// change, and returns the context's error having changed nothing and taken
// no reservation. It admits the call to its share next: a handle whose close
// began is refused with core.ErrClosed, still changing nothing, and a call
// that runs holds its handle's Close back until it returns.
//
// decision: the rules that follow the check — a call that has appended its
// record completes and returns its result whatever the context says, a wait
// for the file append domain is never abandoned, and a refusal past this
// check returns at once as itself — live at the sites that hold them, not
// here: this helper is the shared top of every call, and everything below
// it is each call's own (RFC 1 §3, §8.2).
func beginCall(ctx context.Context, s *Share) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	exit, err := s.enter()
	if err != nil {
		return nil, err
	}
	return exit, nil
}
