// Package lockorder records the order in which the journal's ranked locks
// are acquired, and every rule that order forbids.
//
// The journal's concurrency rules are one total order over six lock
// domains — file append → capacity → file index → file map → segment append
// → storage guard — with absolute prohibitions beside it: no two locks of
// one domain at once, nothing acquired while the file map is held or after
// the storage guard, no I/O under the file index, and no lock held across
// the offload callback. Mutex and RWMutex carry a Rank and
// report each acquisition to a Recorder; a recorder with no mutexes
// pointing at it records nothing, and a mutex with no recorder records
// nothing.
//
// The recorder is test machinery only. A nil recorder leaves the mutexes
// as plain locks costing one atomic load per acquisition, so the checks
// run in tests and never in production; the recorder's own mutex is
// unranked because it is not part of the order it polices. When a rule is
// violated the recorder does not panic or block: it appends a Violation
// with the stack of the offending acquisition, and the test that drove
// the acquisition asserts on it. A deadlock order is reported before the
// deadlock itself, because the mutex reports its attempt before it waits.
package lockorder
