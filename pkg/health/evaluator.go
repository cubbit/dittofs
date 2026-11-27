package health

import (
	"fmt"
	"slices"
	"sync"
)

// Scope is the kind of thing a condition is about.
type Scope uint8

// The scopes, in RFC 25's order.
const (
	ScopeStore        Scope = iota + 1 // a remote store
	ScopeNamespace                     // a namespace
	ScopeJournal                       // a journal device: ID its identity, Name its directory
	ScopeShare                         // a share: ID its identity
	ScopeNode                          // this node: ID its identity
	ScopeInstallation                  // the installation
)

// scopeNames is each scope's name, as RFC 25 spells it.
var scopeNames = [...]string{ScopeStore: "store", ScopeNamespace: "namespace", ScopeJournal: "journal",
	ScopeShare: "share", ScopeNode: "node", ScopeInstallation: "installation"}

// String is the scope's name, as RFC 25 spells it; a scope it does not
// define reads as its number.
func (s Scope) String() string {
	if int(s) < len(scopeNames) && scopeNames[s] != "" {
		return scopeNames[s]
	}
	return fmt.Sprintf("scope(%d)", s)
}

// Subject is what a condition is about.
type Subject struct {
	Scope    Scope
	ID, Name string
}

// Condition is one health condition a component raises. It is derived, never
// latched: a condition its source no longer returns is cleared.
type Condition struct {
	Name    string // as RFC 25's condition table spells it
	Subject Subject
	Cause   error  // the error or reason that raised it
	Detail  string // a constant message and the figures that matter
}

// Source is what a component implements to publish its conditions: it
// appends the conditions it holds now to dst and returns the result. Remove
// finds a source by identity, so an implementation is a pointer: a value of
// an uncomparable type, such as a struct holding a func, panics there.
type Source interface {
	Conditions(dst []Condition) []Condition
}

// Evaluator holds the node's sources and reads each one when asked, so a
// component publishes its conditions without a goroutine pushing them.
//
// ponytail: the evaluator holds only the sources the node adds, and the
// watchdog covers only the loops the node starts; the engine's work scheduler
// and an evaluator loop of its own are not watched, and conditions carry no
// severity or since. Ceiling: RFC 25's full registry. Upgrade path: RFC 25's
// own plan, which extends this package without changing what is built here.
type Evaluator struct {
	mu      sync.Mutex
	sources []Source
}

// Add adds s, whose conditions every later Conditions reads.
func (e *Evaluator) Add(s Source) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sources = append(e.sources, s)
}

// Remove removes s, whose conditions are then cleared. Removing a source not
// added does nothing.
func (e *Evaluator) Remove(s Source) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sources = slices.DeleteFunc(e.sources, func(x Source) bool { return x == s })
}

// Conditions appends every source's conditions to dst, in the order the
// sources were added.
func (e *Evaluator) Conditions(dst []Condition) []Condition {
	e.mu.Lock()
	sources := slices.Clone(e.sources)
	e.mu.Unlock()
	for _, s := range sources {
		dst = s.Conditions(dst)
	}
	return dst
}
