package durability

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// corruptJob is one corrupt record's loss, handed by Corrupt to the stream's
// worker: one Loss per piece the drop took, all of one record.
type corruptJob struct {
	record index.RecordAt
	synced bool
	losses []Loss
}

// queueCorrupt hands job to the stream's worker and wakes it. Once the layer
// has stopped, or the stream closed under the worker, no worker drains the
// queue any more, and the loss is recorded on the caller's goroutine instead.
//
// decision: a Corrupt after Stop appends the loss record itself. The reader
// then waits on an append and its sync, and for a synced loss on MarkLoss,
// which a running layer never makes it do, but there is no worker left to
// hand it to and the loss must not be forgotten; a closed stream refuses the
// append at once, and the drop stands with the seq listed as failed.
// Overturned if a Corrupt after Stop must return without I/O, which would
// then list the seq as failed unappended.
func (d *Durability) queueCorrupt(st *stream, job corruptJob) {
	if !st.pushCorrupt(job) {
		d.runCorrupt(job)
		return
	}
	st.wake()
}

// pushCorrupt queues job for the worker without waking it, and reports
// whether it did: a closed stream has no worker left to drain it. A queued
// job counts as owed on its file, the count a Sync naming the file waits out.
func (st *stream) pushCorrupt(job corruptJob) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return false
	}
	st.corrupt = append(st.corrupt, job)
	st.corruptFiles[jobFile(job)]++
	return true
}

// jobFile is the file whose loss records job owes; all of one job's losses
// name one record, one file.
func jobFile(job corruptJob) core.FileID { return job.losses[0].File }

// drainCorrupt runs every corrupt loss queued on the stream. It runs on the
// worker, and once more in Stop after the worker has exited. Each job's
// finish retires its hold on its file and wakes the waiters parked on it.
func (d *Durability) drainCorrupt(st *stream) {
	st.mu.Lock()
	jobs := st.corrupt
	st.corrupt = nil
	st.mu.Unlock()
	for _, job := range jobs {
		d.runCorrupt(job)
		st.finishCorrupt(job)
	}
}

// owesCorruptLocked reports whether a corrupt job still queued or running on
// this stream owes the loss record of a named file. The caller holds mu.
func (st *stream) owesCorruptLocked(ids []core.FileID) bool {
	for _, id := range ids {
		if st.corruptFiles[id] > 0 {
			return true
		}
	}
	return false
}

// finishCorrupt retires one finished corrupt job's hold on its file and
// wakes every waiter: the job's Lose has appended and synced the loss
// records it owed — or recorded the failure that stands in for them — so a
// Sync that named the file may answer now. It runs on the worker and in
// Stop's drain; the closed-stream arm around it runs its job on the
// caller's goroutine and parks no waiter on the count.
func (st *stream) finishCorrupt(job corruptJob) {
	f := jobFile(job)
	st.mu.Lock()
	if n := st.corruptFiles[f]; n <= 1 {
		delete(st.corruptFiles, f)
	} else {
		st.corruptFiles[f] = n - 1
	}
	st.mu.Unlock()
	st.d.notifyAll()
}

// runCorrupt hands each piece of job to Lose, which appends a loss record of
// its own over the piece's extent. A Lose error is logged by Lose and the drop
// stands, so it is dropped here.
func (d *Durability) runCorrupt(job corruptJob) {
	for _, l := range job.losses {
		d.lose(l)
	}
}
