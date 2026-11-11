package reclaim

import (
	"fmt"
	"testing"
)

// FuzzLiveness decodes arbitrary bytes into the same op sequences the
// oracle property test draws — writes, overwrites, removals, settles,
// re-tests and retirements — runs them against the real tracker over a
// planted index and real segments, and asserts the same two invariants
// after every step: SAFETY, a segment the exact oracle holds is held,
// and EQUALITY, the tracker holds nothing the exact rule does not, the
// no-merged-ranges shape the planted grid earns. Any input of any
// length must decode to bounded steps without panicking; the bytes the
// decoder turns into a retirement of a retired segment, or a write into
// one, are no-ops, the same shapes the seeded draws respawn off.
//
// The op encoding, consumed from the front until the bytes run out or
// 64 steps are read:
//
//	[0 file seg bucket v seq]  a write of bucket at (v, seq)
//	[1 file seg bucket v seq]  a removal record of bucket at (v, seq)
//	[2 file v]                 a settle of file at or below v
//	[3 file]                   a plain re-test of file
//	[4 seg]                    a retirement of a segment
//	[5…]                       a nop consuming its byte
//
// file masks to the two oracle files, seg to the five segments, and v
// sits in the 1..16 span the planted versions occupy; a byte naming a
// retired segment slot is a no-op for a write or a retirement draw, and
// the step runner never allocates a segment the scenario did not take.
func FuzzLiveness(f *testing.F) {
	// Every seed below retires the plant's other segments first and
	// cuts its own segment's content with the removal's Lose, so the
	// segment the checked registration sits in is byte-free and the
	// drop test alone decides it. The planted case of RFC 1 §7.2: a
	// truncate over bucket 1 of file 0 at (4, 60), its file's bucket
	// cut first, then a write over the same bucket at (3, 127) — a
	// strictly lower pair only by version, its sequence higher — and
	// a settle that covers the truncate's marker: the pair comparison
	// is the whole answer, and a sequence-only comparison would drop
	// the truncate while the write's bytes are still on disk.
	f.Add([]byte{4, 0, 4, 2, 4, 3, 1, 0, 1, 1, 3, 60, 0, 0, 4, 1, 2, 127, 2, 0, 15})
	// A truncate whose marker a settle covers with nothing below its
	// pair anywhere: the settle's own re-test is the one event that
	// can drop it, so a settle that runs no re-test keeps it live
	// against the oracle.
	f.Add([]byte{4, 0, 4, 2, 4, 3, 1, 0, 1, 1, 3, 70, 2, 0, 15})
	// The same truncate with no settle at all: its marker alone keeps
	// it, whatever a settle skipped would have done.
	f.Add([]byte{4, 0, 4, 2, 4, 3, 1, 0, 1, 1, 3, 70})
	// A retirement as the one re-test left: the settle cannot drop
	// the truncate — a write over its bucket at (3, 66) keeps it —
	// and retiring the write's segment is the only event that can.
	f.Add([]byte{4, 0, 4, 2, 4, 3, 1, 0, 1, 1, 3, 70, 0, 0, 4, 1, 2, 66, 2, 0, 15, 4, 4})
	// A duplicate op: the same truncate registered twice, its marker
	// settled with nothing below its pair: one re-test must lower
	// exactly the two registrations the duplicate made.
	f.Add([]byte{4, 0, 4, 2, 4, 3, 1, 0, 1, 1, 3, 60, 1, 0, 1, 1, 3, 60, 2, 0, 15})
	// A padding storm: nop bytes beside every retirement of a segment
	// slot in turn — the second retirement of the same slot a no-op —
	// then a settle, a re-test, a write into a retired slot (a no-op
	// too) and two trailing nops.
	f.Add([]byte{5, 5, 5, 4, 0, 4, 0, 4, 1, 4, 2, 4, 3, 4, 4, 2, 1, 15, 3, 1, 0, 0, 0, 0, 0, 64, 5, 5})
	f.Fuzz(func(t *testing.T, b []byte) {
		ops := decodeOps(b)
		s := drawScenario(t)
		for step, o := range ops {
			s.run(t, step, o)
			s.check(t, step, fmt.Sprintf("kind=%d file=%d bucket=%d seg=%d v=%d seq=%d",
				o.kind, o.file, o.bucket, o.seg, o.v, o.seq))
		}
	})
}

// decodeOps reads the op encoding above, bounding a body at 64 steps:
// an op whose bytes run out mid-record stops the decode there, and
// anything unrecognized is a nop consuming the one byte it sat on. The
// bytes never reach the runner unchecked: the kinds are the five the
// runner knows plus the nop, and every field is masked into its drawn
// domain before a call is made.
func decodeOps(b []byte) []op {
	ops := make([]op, 0, 64)
	for len(b) > 0 && len(ops) < 64 {
		kind := b[0] % 6
		if int(kind) == int(opNopKind) || kind > 4 {
			b = b[1:]
			ops = append(ops, op{kind: opNopKind})
			continue
		}
		n := 0
		switch kind {
		case 0, 1: // a write or a removal: file, seg, bucket, v, seq
			n = 6
		case 2: // a settle: file, v
			n = 3
		case 3: // a re-test: file
			n = 2
		case 4: // a retirement: seg
			n = 2
		}
		if len(b) < n {
			return ops
		}
		o := op{kind: opKind(kind)}
		switch kind {
		case 0, 1:
			o.file = b[1] & 1
			o.seg = b[2] % oracleSegs
			o.bucket = b[3] & 3
			o.v = 1 + uint64(b[4]%16)
			o.seq = uint64(b[5]) | 64 // a drawn byte, lifted past every planted number
			// A removal whose bytes name no sequence draws a fresh one
			// from the run; duplicates of the same bytes are the
			// equal-pair shapes the pair comparison must order.
		case 2:
			o.file = b[1] & 1
			o.v = 1 + uint64(b[2]%16)
		case 3:
			o.file = b[1] & 1
		case 4:
			o.seg = b[1] % oracleSegs
		}
		ops = append(ops, o)
		b = b[n:]
	}
	return ops
}
