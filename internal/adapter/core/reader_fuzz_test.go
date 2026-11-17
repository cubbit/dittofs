package core_test

import (
	"context"
	"encoding/binary"
	"net"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
)

// seedFragments hands the fuzzer a stream of n fragments: a mark before each
// body, the first saying more follow, the last ending the record.
func seedFragments(bodies ...[]byte) []byte {
	var b []byte
	for i, body := range bodies {
		b = binary.BigEndian.AppendUint32(b, uint32(len(body)))
		if i < len(bodies)-1 {
			b[len(b)-4] |= 0x80
		}
		b = append(b, body...)
	}
	return b
}

// FuzzReader feeds arbitrary streams through the record cut: no panic, the
// record returned is the stream's own framing, its body byte for byte and
// nothing past the framing's reach, and the receive budget ends at zero
// whatever the stream held. A mark or body the framing refuses ends the read
// with nothing held.
func FuzzReader(f *testing.F) {
	f.Add(seedFragments(payload(20)))
	f.Add(seedFragments(payload(64), payload(11), payload(32)))
	f.Add(seedFragments(payload(3), payload(40)))
	f.Add(binary.BigEndian.AppendUint32(nil, 1<<20+4<<10+1)) // a body past the cap
	f.Add([]byte{0, 0, 0})                                   // a cut mark
	f.Fuzz(func(t *testing.T, stream []byte) {
		budgets := &resources.Budgets{Receive: resources.NewWaitBudget("receive", 1<<30)}
		adaptertest.CheckLeaks(t, budgets)
		pool := resources.NewBufPool(budgets.Receive)
		srv, cli := net.Pipe()
		go func() {
			_, _ = cli.Write(stream)
			_ = cli.Close()
		}()
		r := core.NewTestReader(srv, framer(), pool, readerKey, 0)

		// paced walks the stream by its own counters, the way the cut does,
		// and reports the body bytes the stream's whole records hold, and
		// whether the stream is exactly records to its end: a fragment without
		// its last, or a mark or body cut by the end, is not one.
		capBytes, capFrags := framer().MaxRecord()
		// paced walks the stream by its own counters, the way the cut does,
		// refusals included: a mark past the cap, a fragment count past the
		// framer's, and fragments whose total passes the cap end the record
		// there, as the reader refuses them.
		paced := func() (bodies []byte, whole bool) {
			at := 0
			for at != len(stream) {
				var rec []byte
				frags, total := 0, 0
				last := false
				for !last {
					if at+4 > len(stream) {
						return bodies, false // a mark the stream cut, or trailing bytes
					}
					m := binary.BigEndian.Uint32(stream[at:])
					n, more := int(m&^0x80000000), m&0x80000000 != 0
					if n > capBytes || frags+1 > capFrags || total+n > capBytes {
						return bodies, false // a refusal: past the caps the framer names
					}
					if at+4+n > len(stream) {
						return bodies, false // a body the stream cut
					}
					rec = append(rec, stream[at+4:at+4+n]...)
					at += 4 + n
					total += n
					frags++
					last = !more
				}
				bodies = append(bodies, rec...)
			}
			return bodies, true
		}
		want, whole := paced()

		var read []byte
		for {
			rec, err := r.Read(context.Background())
			if err != nil {
				break // the stream's end, a truncated record or a refused mark
			}
			read = append(read, rec.B...)
			rec.Release()
		}
		_ = srv.Close()
		if whole {
			if string(read) != string(want) {
				t.Fatalf("the cut reads %d bytes of whole records, want the %d the framing holds", len(read), len(want))
			}
		} else if len(read) > len(want) {
			t.Fatalf("the cut reads %d bytes where a framing reaches %d", len(read), len(want))
		}
		if held := budgets.Receive.Reserved(); held != 0 {
			t.Fatalf("the cut leaves %d bytes held of a stream of %d bytes", held, len(stream))
		}
	})
}
