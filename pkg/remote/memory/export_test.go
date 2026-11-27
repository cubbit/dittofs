package memory

// PutRaw stores body under key beneath s, for the opener's Subject.PutRaw.
func PutRaw(s *Store, key string, body []byte) { s.putRaw(key, body) }

// Reopen returns a second store over s's objects, for the opener's
// Subject.Reopen.
func Reopen(s *Store) *Store { return s.reopen() }

// RawKeys returns every key beneath s, for the opener's Subject.Raw.
func RawKeys(s *Store) []string { return s.rawKeys() }
