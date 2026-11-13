package segment

// SetSyncAtGapHook sets the hook in SyncAt between its checks and its sync.
func SetSyncAtGapHook(f func()) { syncAtGapHook = f }

// SetRollTakeHook sets the instance's hook between the roll's spare take and
// its read of the old segment: the staging a racing seal needs.
func (s *Stream) SetRollTakeHook(f func()) { s.rollTakeHook = f }

// SetRollWaitHook sets the instance's hook where the roll has just read a
// sealing state and is about to park on the drain: the confirmation a row
// holds a finishing seal on.
func (s *Stream) SetRollWaitHook(f func()) { s.rollWaitHook = f }
