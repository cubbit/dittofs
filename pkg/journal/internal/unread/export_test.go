package unread

// entry returns the Kind the set holds for ref's (Segment, Off), and whether
// it holds one: Ranges answers bytes only, so this is the read a test takes
// of the Kind a mark kept. It lives with the tests because nothing outside
// them reads a held Kind yet.
func (s *Set) entry(ref Ref) (Kind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok := s.entries[ref.Segment][ref.Off]
	return held.Kind, ok
}
