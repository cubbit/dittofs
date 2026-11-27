package identity_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The benchmarks below measure a directory principal's cold resolution, the
// latency of its directory queries and its mints together, against a
// directory whose every search the fault wrapper delays 5, 20 and 50
// milliseconds on the wall clock. The fault delays every search by the same
// amount whatever its filter, so the figures say what the queries' count and
// concurrency cost, not what a filter of many terms costs. Each
// iteration's entries are added, and its users minted, with the timer
// stopped and the directory answering at once; only the resolution runs
// timed, under the delay, after the clock is moved past every cached
// answer's TTL and stale grace, so the closure the mint of the user cached
// and his entry are asked again; the domain's SID is not, the client holds
// it for its life. A resolution that
// fails, past its 20-second bound among other refusals, fails the run.

// benchDelays are the delays every search is given.
var benchDelays = []time.Duration{5 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond}

// wallClock is the system clock moved on by skew, so the delays and the
// 20-second bound run in real time and the caches' answers can be aged at
// once.
type wallClock struct{ skew atomic.Int64 }

func (c *wallClock) Now() time.Time { return time.Now().Add(time.Duration(c.skew.Load())) }

func (*wallClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}

// dirBench is a server on the wall clock over the in-memory directory.
type dirBench struct {
	*dirPrincipals
	wall *wallClock // the promoted clock field is the test clock, nil here
	n    int        // numbers each iteration's entries apart
}

// delay delays every later search d; zero lets every search through at once.
func (e *dirBench) delay(b *testing.B, d time.Duration) {
	b.Helper()
	if d == 0 {
		e.fault.cur.Store(nil)
		return
	}
	s, err := sim.NewSchedule(1, e.wall, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Delay: d}})
	if err != nil {
		b.Fatal(err)
	}
	e.fault.cur.Store(s)
}

// users adds n directory users and mints each before any group of this
// iteration holds it, so the groups are left to its resolution to mint. It
// returns their DNs and principals.
func (e *dirBench) users(b *testing.B, n int) ([][]byte, []entity.Principal) {
	b.Helper()
	dns := make([][]byte, n)
	ps := make([]entity.Principal, n)
	for i := range n {
		name := fmt.Sprintf("u%d.%d", e.n, i)
		dns[i] = []byte(e.d.AddUser(b, name, nil))
		p, err := e.s.Find(context.Background(), pid.Spelling{Scheme: pid.SchemeQualified, Text: `KORP\` + name}, pid.FindOpts{Mint: true})
		if err != nil {
			b.Fatalf("Find(%s, mint) = %v", name, err)
		}
		ps[i] = p
	}
	return dns, ps
}

// resolve resolves ev's principal, proven by proof, on a share that
// squashes nobody.
func (e *dirBench) resolve(proof pid.ProofKind, ev pid.Evidence) error {
	_, err := e.s.Resolve(context.Background(), pid.Identity{Proof: proof, Evidence: ev}, fileShare(), pid.ReadOnlyNotNarrowed)
	return err
}

// benchDirectory runs, for each delay, one untimed warm-up and then b.N
// timed resolutions, each over the entries setup adds: setup returns the
// resolution, the DNs to delete once it has run and how many groups the
// resolution must mint. The directory is back to its size after each one,
// though the minted users and groups stay in the store, which grows with
// b.N. The warm-up reads the domain's SID and
// mints Domain Users, which every later resolution finds cached and minted.
func benchDirectory(b *testing.B, setup func(b *testing.B, e *dirBench) (op func() error, added []string, mints int)) {
	for _, d := range benchDelays {
		b.Run("delay="+d.String(), func(b *testing.B) {
			st, inst := dirStore(b)
			clock := &wallClock{}
			e := &dirBench{dirPrincipals: newDirPrincipalsOn(b, st, inst, directory.SchemaAD, clock), wall: clock}
			// Past the 5-minute TTL and the stale grace, every cached answer is asked again.
			age := 5*time.Minute + identity.DefaultSettings().DirectoryStaleGrace + time.Second
			iteration := func(timed bool) {
				e.n++
				op, added, mints := setup(b, e)
				before := e.counter(b, "identity_mints_total", "kind", "group")
				e.wall.skew.Add(int64(age))
				e.delay(b, d)
				if timed {
					b.StartTimer()
				}
				start := time.Now()
				err := op()
				took := time.Since(start)
				if timed {
					b.StopTimer()
				}
				e.delay(b, 0)
				if err != nil {
					b.Fatalf("resolution with every search delayed %v = %v after %v, want it to succeed within the 20-second bound", d, err, took)
				}
				// A resolution that asked the directory nothing, or whose searches the rule missed, runs undelayed.
				if took < d {
					b.Fatalf("resolution took %v with every search delayed %v: no search reached the delay", took, d)
				}
				// A closure served from the cache would mint nothing: the resolution was not cold.
				if got := e.counter(b, "identity_mints_total", "kind", "group") - before; got != float64(mints) {
					b.Fatalf("groups minted by the resolution %v, want %d", got, mints)
				}
				for _, dn := range added {
					e.d.Delete(dn)
				}
			}
			iteration(false)
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for range b.N {
				iteration(true)
			}
		})
	}
}

// groups adds n security groups whose members are dns and returns their DNs.
func (e *dirBench) groups(b *testing.B, n int, dns [][]byte) []string {
	b.Helper()
	out := make([]string, n)
	for i := range n {
		out[i] = e.d.AddGroup(b, fmt.Sprintf("g%d.%d", e.n, i), map[string][][]byte{"member": dns})
	}
	return out
}

// A cold resolution of one user in 300 groups none of which is minted: his
// entry, his closure and its SIDs asked of the directory, the groups minted.
func BenchmarkDirectoryColdResolution(b *testing.B) {
	benchDirectory(b, func(b *testing.B, e *dirBench) (func() error, []string, int) {
		dns, ps := e.users(b, 1)
		added := append(e.groups(b, 300, dns), string(dns[0]))
		return func() error { return e.resolve(pid.ProofSecret, pid.Evidence{Principal: ps[0]}) }, added, 300
	})
}

// 50 first sign-ins at once, each a cold resolution of a distinct user in
// the same 300 groups none of which is minted; an iteration is the whole
// burst, so its time is the slowest sign-in's latency.
func BenchmarkDirectoryFirstSignIns(b *testing.B) {
	benchDirectory(b, func(b *testing.B, e *dirBench) (func() error, []string, int) {
		dns, ps := e.users(b, 50)
		added := e.groups(b, 300, dns)
		for _, dn := range dns {
			added = append(added, string(dn))
		}
		return func() error {
			errs := make([]error, len(ps))
			var wg sync.WaitGroup
			for i, p := range ps {
				wg.Go(func() { errs[i] = e.resolve(pid.ProofSecret, pid.Evidence{Principal: p}) })
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					return fmt.Errorf("sign-in %d of %d: %w", i, len(errs), err)
				}
			}
			return nil
		}, added, 300
	})
}

// A ticket's cold resolution whose PAC carries 1 024 SIDs: Domain Users and
// 1 023 groups of the domain no principal holds, asked of the directory in
// batches and minted. It measures a ticket of that size at the current batch
// size: every search is delayed alike, so it does not price the filter.
func BenchmarkDirectoryPACSIDs(b *testing.B) {
	benchDirectory(b, func(b *testing.B, e *dirBench) (func() error, []string, int) {
		dns, ps := e.users(b, 1)
		domain := e.d.DomainSID()
		sids := make([]pid.SID, 0, 1024)
		added := []string{string(dns[0])}
		rids := []uint32{513} // Domain Users, which the directory holds already
		for i := range uint32(1023) {
			// RIDs well above the directory's own, which count up from 1 100.
			rid := uint32(10_000_000+e.n*1024) + i
			added = append(added, e.d.AddGroup(b, fmt.Sprintf("p%d.%d", e.n, rid), map[string][][]byte{"objectSid": {directory.WithRID(domain, rid)}}))
			rids = append(rids, rid)
		}
		for _, rid := range rids {
			v, _, err := sid.DecodeSID(directory.WithRID(domain, rid))
			if err != nil {
				b.Fatal(err)
			}
			sids = append(sids, *v)
		}
		return func() error { return e.resolve(pid.ProofTicket, pid.Evidence{Principal: ps[0], SIDs: sids}) }, added, 1023
	})
}
