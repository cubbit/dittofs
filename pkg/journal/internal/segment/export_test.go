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

// SetPinnedHook sets the instance's hook that runs once an append has left the
// drain with its record pinned, holding no lock: where a seal can first run.
func (s *Stream) SetPinnedHook(f func()) { s.pinnedHook = f }

// SetRefillClassHook sets the instance's hook that runs once a refill pass
// has chosen the class it fills and left the pool's mutex. It must run before
// Start.
func (p *Pool) SetRefillClassHook(f func()) { p.classHook = f }

// SetPassHook sets the instance's hook the retirement worker runs at the end
// of every pass, so a test knows a pass finished and what it left. It must run
// before Start.
func (r *Retirer) SetPassHook(f func()) { r.passHook = f }

// KickPendingForTest reports whether a kick sits unconsumed in the worker's
// one-slot channel, so a test knows no further pass is owed.
func (r *Retirer) KickPendingForTest() bool { return len(r.kick) > 0 }

// SeedRecordBufs hands n buffers to the pool class of a record of len(stale)
// bytes, each holding stale at its head as an earlier record's encode leaves
// it. It first draws one buffer out and drops it, so the class's next draw on
// this goroutine is one of these. It returns each buffer's whole backing
// array, so a test sees what an append that drew one encoded there.
func SeedRecordBufs(stale []byte, n int) [][]byte {
	_ = getRecordBuf(int64(len(stale)))
	var out [][]byte
	for range n {
		p := getRecordBuf(int64(len(stale)))
		b := append((*p)[:0], stale...)
		*p = b
		putRecordBuf(p)
		out = append(out, b[:cap(b)])
	}
	return out
}
