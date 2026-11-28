package chain_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// BenchmarkChainOverhead measures the chain around two identity transforms, a
// compressor and a redundancy step, against the same two transforms called
// directly into buffers made once, for each direction and chunk size: compare
// the two with benchstat -col /via over -count=10 runs on one machine. It
// gates nothing. The chain case includes the step buffer it makes per call and,
// on encode, the copy behind the envelope, which its ponytail: marker
// accepts, so its gap is RFC 5 §8.3's overhead, to be held within 2%, only
// once those are paid off.
func BenchmarkChainOverhead(b *testing.B) {
	ctx := context.Background()
	steps := []transform.Transform{
		pad{id: idA, stage: transform.StageCompress},
		pad{id: idC, stage: transform.StageRedundancy},
	}
	for _, size := range []int{4 << 10, 1 << 20} {
		plain := bytes.Repeat([]byte{0x5a}, size)
		d := depsFor()
		d.Recorder = &tally{}
		c, err := allocsRegistry().NewChain(ctx, names("c", "r"), keystest.NewMemory(), d)
		if err != nil {
			b.Fatalf("NewChain: %v", err)
		}
		a, err := c.Pin(ctx, ns)
		if err != nil {
			b.Fatalf("Pin: %v", err)
		}
		dst := make([]byte, 0, a.MaxEncodedLen(size))
		body, encs, err := a.Encode(ctx, nil, hash, plain)
		if err != nil {
			b.Fatalf("Encode: %v", err)
		}
		mid, out := make([]byte, 0, size), make([]byte, 0, size)
		inner := body[len(body)-size:] // behind the envelope

		run := func(name string, f func() error) {
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					if err := f(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
		kib := fmt.Sprintf("size=%dKiB", size>>10)
		run("dir=encode/"+kib+"/via=chain", func() error {
			_, _, err := a.Encode(ctx, dst, hash, plain)
			return err
		})
		run("dir=encode/"+kib+"/via=transforms", func() error {
			e, err := steps[0].Encode(ctx, ns, nil, mid, hash, plain)
			if err != nil {
				return err
			}
			_, err = steps[1].Encode(ctx, ns, nil, out, hash, e.Body)
			return err
		})
		run("dir=decode/"+kib+"/via=chain", func() error {
			_, err := c.Decode(ctx, ns, out, hash, body, size, encs)
			return err
		})
		run("dir=decode/"+kib+"/via=transforms", func() error {
			m, err := steps[1].Decode(ctx, ns, mid, hash, inner, size, nil)
			if err != nil {
				return err
			}
			_, err = steps[0].Decode(ctx, ns, out, hash, m, size, nil)
			return err
		})
	}
}
