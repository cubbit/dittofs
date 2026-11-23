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
	// seq is the job's place in the stream's queue, taken as it was queued.
	// A Sync records it per file to wait only the jobs the stream owed when
	// the call took its targets — those a read queued before the call — and
	// never one a later read queues once the call is waiting.
	seq uint64
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
// job owes its file the seq it takes here, and only a Sync that named the
// file before this moment — the job is inside the call's owed snapshot —
// waits that seq out.
func (st *stream) pushCorrupt(job corruptJob) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return false
	}
	st.corruptSeq++
	job.seq = st.corruptSeq
	st.corrupt = append(st.corrupt, job)
	st.corruptQueued[jobFile(job)] = job.seq
	return true
}

// jobFile is the file whose loss records job owes; all of one job's losses
// name one record, one file.
func jobFile(job corruptJob) core.FileID { return job.losses[0].File }

// drainCorrupt runs every corrupt loss queued on the stream, front to back.
// It runs on the worker, and once more in Stop after the worker has exited.
// Each job's finish records its seq as its file's done-through, which unblocks
// the waiters parked on it, and wakes them.
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

// owedCorrupt is a Sync call's snapshot of the corrupt debt its named files
// owed on one stream when the call took its targets: per file, through the
// seq of the newest job queued for it. nil names no owed job.
type owedCorrupt map[core.FileID]uint64

// owedCorruptLocked takes that snapshot. The caller holds mu — targetsOf
// folds it into the critical section that takes the call's targets, so the
// debt and the targets name one instant of the queue. The snapshot belongs
// to the call alone: jobs a later read queues after it were never owed to
// this call, and no later wait of the same files consults them.
func (st *stream) owedCorruptLocked(ids []core.FileID) owedCorrupt {
	var owed owedCorrupt
	for _, id := range ids {
		through := st.corruptQueued[id]
		if st.corruptDone[id] >= through {
			continue // nothing owed, or the file was never queued for
		}
		if owed == nil {
			owed = owedCorrupt{}
		}
		owed[id] = through
	}
	return owed
}

// finishCorrupt records one finished job's seq as its file's done-through and
// wakes every waiter: the job's Lose has appended and synced the loss records
// it owed — or recorded the failure that stands in for them — so a Sync whose
// snapshot reaches the job's seq may answer now. Jobs finish in queue order,
// so done keeps to the file's seqs and no waiter ends early while an earlier
// job of its snapshot is still owed. It runs on the worker and in Stop's
// drain; the closed-stream arm around it runs its job on the caller's
// goroutine, where it takes no seq and parks no waiter.
func (st *stream) finishCorrupt(job corruptJob) {
	f := jobFile(job)
	st.mu.Lock()
	st.corruptDone[f] = job.seq
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
