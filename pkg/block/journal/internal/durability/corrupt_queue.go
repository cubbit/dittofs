package durability

import "github.com/cubbit/dittofs/pkg/block/journal/internal/index"

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
// which a running layer never makes it do, but there is no worker left to hand it to and the loss must not be
// forgotten; a closed stream refuses the append at once, and the drop stands
// with the seq listed as failed. Overturned if a Corrupt after Stop must
// return without I/O, which would then list the seq as failed unappended.
func (d *Durability) queueCorrupt(st *stream, job corruptJob) {
	st.mu.Lock()
	closed := st.closed
	if !closed {
		st.corrupt = append(st.corrupt, job)
	}
	st.mu.Unlock()
	if closed {
		d.runCorrupt(job)
		return
	}
	st.wake()
}

// drainCorrupt runs every corrupt loss queued on the stream. It runs on the
// worker, and once more in Stop after the worker has exited.
func (d *Durability) drainCorrupt(st *stream) {
	st.mu.Lock()
	jobs := st.corrupt
	st.corrupt = nil
	st.mu.Unlock()
	for _, job := range jobs {
		d.runCorrupt(job)
	}
}

// runCorrupt hands each piece of job to Lose, which appends a loss record of
// its own over the piece's extent. A Lose error is logged by Lose and the drop
// stands, so it is dropped here.
func (d *Durability) runCorrupt(job corruptJob) {
	for _, l := range job.losses {
		d.lose(l)
	}
}
