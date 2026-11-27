package datapath

import (
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/cubbit/dittofs/pkg/journal/internal/lockorder"
)

// statsBackstop is the one generous bound a Stats call has to return in while
// a test holds a lock. No clock can be injected into a goroutine blocked on a
// mutex, so the bounded wait is the assertion: a Stats that does not return
// within it waits on the lock.
const statsBackstop = 5 * time.Second

// statsAllowed names the locks Stats may still take, each with the reason
// holding it cannot stall a write. A lock on Stats' path that is on neither
// this list nor removed from the path fails the test below by name.
var statsAllowed = map[string]string{
	"dur.lmu": "deviates from RFC 1 section 3.7's MUST NOT block a write (a loss-path write can wait on it while Stats copies the ring); held by a loss for its raise, counters and ring publish as one section, and by " +
		"Stats for the ring copy and the generation read; no I/O under it, and the one lock " +
		"taken inside it is dur.dir.mu",
	"dur.dir.mu": "part of the same deviation from RFC 1 section 3.7, a leaf taken inside dur.lmu that guards the directory's generation and failed " +
		"list, never held across I/O",
}

// minStatsLocks is the count of mutexes the walk finds on the components Stats
// reads today: the twelve locks Stats once took, the nested leaves, the
// index's 64 shard locks and the rest. A lower count means a root or a lock
// vanished, and the walk is no longer reaching what it did: the test needs a
// look, not the number. A higher count is fine.
const minStatsLocks = 95

var mutexTypes = map[reflect.Type]bool{
	reflect.TypeFor[sync.Mutex]():        true,
	reflect.TypeFor[sync.RWMutex]():      true,
	reflect.TypeFor[lockorder.Mutex]():   true,
	reflect.TypeFor[lockorder.RWMutex](): true,
}

type namedLock struct {
	name string
	l    sync.Locker
}

// field returns a pointer to the named field of the struct v points at, or the
// field itself when it is already a pointer.
func field(t *testing.T, v reflect.Value, name string) reflect.Value {
	t.Helper()
	f := v.Elem().FieldByName(name)
	if !f.IsValid() {
		t.Fatalf("%s has no field %q", v.Type(), name)
	}
	if f.Kind() == reflect.Pointer {
		return f
	}
	return f.Addr()
}

// locksOf returns a locker for every mutex of the struct v points at, named
// root.field, including mutexes nested in struct-valued fields (root.field.sub)
// and in array elements (root.field[i].sub). It reads the mutexes through their
// address, so a lock added to the struct later is found without a line here.
//
// What it does not follow: slices, maps, pointer fields that statsLocks does
// not name as a root, and *sync.Mutex fields. A component Stats begins to read
// must therefore be added to statsLocks as a root, or a lock it takes is not
// found by this test.
func locksOf(root string, v reflect.Value) []namedLock {
	return walkLocks(root, v.Elem())
}

// walkLocks returns the lockers inside the addressable value v: v itself when
// it is a mutex, and those inside it when it is a struct or an array.
func walkLocks(name string, v reflect.Value) []namedLock {
	switch {
	case mutexTypes[v.Type()]:
		p := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr()))
		return []namedLock{{name, p.Interface().(sync.Locker)}}
	case v.Kind() == reflect.Struct:
		var out []namedLock
		for i := range v.NumField() {
			out = append(out, walkLocks(name+"."+v.Type().Field(i).Name, v.Field(i))...)
		}
		return out
	case v.Kind() == reflect.Array:
		var out []namedLock
		for i := range v.Len() {
			out = append(out, walkLocks(fmt.Sprintf("%s[%d]", name, i), v.Index(i))...)
		}
		return out
	}
	return nil
}

// statsLocks returns every mutex of every component Stats reads.
func statsLocks(t *testing.T, c *Core) []namedLock {
	t.Helper()
	cv := reflect.ValueOf(c)
	roots := map[string]reflect.Value{
		"core":     cv,
		"dirty":    field(t, cv, "dirtyFiles"),
		"shares":   field(t, cv, "shares"),
		"cap":      field(t, cv, "cap"),
		"headroom": field(t, cv, "headroom"),
		"set":      field(t, cv, "set"),
		"pool":     field(t, cv, "pool"),
		"retirer":  field(t, cv, "retirer"),
		"cache":    field(t, cv, "cache"),
		"dur":      field(t, cv, "dur"),
		"counters": field(t, cv, "counters"),
		"stream0":  reflect.ValueOf(c.streams.Stream(0)),
	}
	roots["dur.dir"] = field(t, roots["dur"], "dir")
	roots["budget"] = field(t, roots["cache"], "budget")
	ix := field(t, cv, "index")
	roots["index"] = ix
	fs := field(t, ix, "fs")
	roots["files"] = fs
	roots["conflicts"] = field(t, fs, "conflicts")

	// A mutex reachable from two roots (a component's field and the same
	// component named as a root of its own) is one lock: it is kept once, under
	// the lexically first name, so a subtest and an allow-list entry name it
	// the same way every run.
	names := make([]string, 0, len(roots))
	for name := range roots {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []namedLock
	seen := map[sync.Locker]bool{}
	for _, name := range names {
		for _, nl := range locksOf(name, roots[name]) {
			if !seen[nl.l] {
				seen[nl.l] = true
				out = append(out, nl)
			}
		}
	}
	return out
}

// Stats returns while another goroutine holds any one lock of the components
// it reads, except the locks statsAllowed names.
func TestStatsReturnsWhileAnyLockIsHeld(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	// State in every figure Stats reads: records, a dirty file, a synced file.
	mustWrite(t, c, s, testFile(1), 0, make([]byte, 4096))
	mustWrite(t, c, s, testFile(2), 0, make([]byte, 4096))
	syncFiles(t, c, testFile(1))

	locks := statsLocks(t, c)
	if len(locks) < minStatsLocks {
		t.Fatalf("found %d mutexes on Stats' components, want at least %d", len(locks), minStatsLocks)
	}
	for _, nl := range locks {
		t.Run(nl.name, func(t *testing.T) {
			if why, ok := statsAllowed[nl.name]; ok {
				t.Skip("allow-listed: " + why)
			}
			nl.l.Lock()
			done := make(chan struct{})
			go func() {
				c.Stats()
				close(done)
			}()
			select {
			case <-done:
				nl.l.Unlock()
			case <-time.After(statsBackstop):
				nl.l.Unlock()
				<-done
				t.Errorf("Stats did not return within %s while %s was held: it takes that lock", statsBackstop, nl.name)
			}
		})
	}
}

// An allow-listed name must still name a mutex Stats' components own.
func TestStatsAllowListNamesRealLocks(t *testing.T) {
	c, _ := newTestCore(t)
	have := map[string]bool{}
	for _, nl := range statsLocks(t, c) {
		have[nl.name] = true
	}
	for name := range statsAllowed {
		if !have[name] {
			t.Errorf("allow-listed lock %q is no mutex of a component Stats reads", name)
		}
	}
}
