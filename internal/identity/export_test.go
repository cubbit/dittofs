package identity

// Applied is the State calls now read; nil before the first Apply and after
// Stop.
func (s *Server) Applied() *State { return s.state.Load() }

// ErrNotApplied is Start's refusal before the first Apply.
var ErrNotApplied = errNotApplied

// OnStateLoaded has every Resolve pass call f just after it loads the
// State, before its first store read, so a test can Apply in between.
func (s *Server) OnStateLoaded(f func()) { s.loaded = f }
