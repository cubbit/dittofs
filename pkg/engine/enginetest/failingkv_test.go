package enginetest

import (
	"sync"
	"testing"
)

// A restore to no failures stays a restore while Updates race it: an Update
// that read the old count must not write a count back over the new one.
// The race is between two goroutines' atomics, so each round gives it one
// chance and the rounds make missing it unlikely; a pass proves nothing
// about a single round.
func TestFailingKVRestoreIsNotUndoneByARacingUpdate(t *testing.T) {
	for round := range 2000 {
		var k failingKV
		k.left.Store(0)
		started := make(chan struct{}, 4)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				k.fails()
				started <- struct{}{}
				for {
					select {
					case <-stop:
						return
					default:
						k.fails()
					}
				}
			})
		}
		for range 4 {
			<-started
		}
		k.left.Store(-1)
		close(stop)
		wg.Wait()
		if got := k.left.Load(); got != -1 {
			t.Fatalf("round %d: left after the restore and the racing Updates = %d, want -1", round, got)
		}
	}
}
