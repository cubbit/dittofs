package segment

// SetSyncAtGapHook sets the hook in SyncAt between its checks and its sync.
func SetSyncAtGapHook(f func()) { syncAtGapHook = f }

// SetRollTakeHook sets the hook between the roll's spare take and its read of
// the old segment.
func SetRollTakeHook(f func()) { rollTakeHook = f }
