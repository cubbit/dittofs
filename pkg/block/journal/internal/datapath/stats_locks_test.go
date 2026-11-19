package datapath

import (
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
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
	"dur.lmu": "a leaf: it holds a loss's raise, counters and ring publish as one section, " +
		"never across an append, a sync or a marker write",
}

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

// locksOf returns a locker for every mutex field of the struct v points at,
// named root.field. It reads the mutexes through their address, so a lock
// added to the struct later is found without a line here.
func locksOf(root string, v reflect.Value) []namedLock {
	var out []namedLock
	st := v.Elem()
	for i := range st.NumField() {
		ft := st.Type().Field(i)
		if !mutexTypes[ft.Type] {
			continue
		}
		p := reflect.NewAt(ft.Type, unsafe.Pointer(st.Field(i).UnsafeAddr()))
		out = append(out, namedLock{root + "." + ft.Name, p.Interface().(sync.Locker)})
	}
	return out
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
	roots["budget"] = field(t, roots["cache"], "budget")
	ix := field(t, cv, "index")
	roots["index"] = ix
	fs := field(t, ix, "fs")
	roots["files"] = fs
	roots["conflicts"] = field(t, fs, "conflicts")

	var out []namedLock
	for name, v := range roots {
		out = append(out, locksOf(name, v)...)
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
	if len(locks) < 12 {
		t.Fatalf("found %d mutexes on Stats' components, want at least the twelve it once took", len(locks))
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
