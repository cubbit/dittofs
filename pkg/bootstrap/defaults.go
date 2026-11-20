package bootstrap

// GetDefaultConfig returns the bootstrap a load with no file and no variable
// gives: every key at its RFC 13 Appendix C default.
//
// It panics when even that load refuses, which a relative XDG_STATE_HOME or
// HOME does, since the state directory makes blockstore.journal.path's
// default: it has no error to return, and a zero bootstrap would run with no
// log level and no listener. No command calls it; a caller that can meet a
// bad environment calls Load.
func GetDefaultConfig() *Bootstrap {
	b, err := Load("", nil)
	if err != nil {
		panic("bootstrap: the defaults do not load: " + err.Error())
	}

	return b
}
