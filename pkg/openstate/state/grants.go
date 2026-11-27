package state

import (
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate/grantfsm"
)

// GrantKind is what a caching grant lets its holder cache: read, write and
// handle, in any combination holding read, or none.
type GrantKind = grantfsm.Kind

const (
	GrantNone   = grantfsm.None
	GrantRead   = grantfsm.Read
	GrantWrite  = grantfsm.Write
	GrantHandle = grantfsm.Handle
)

// CachingGrant is a promise that no other holder is using the file in a way
// that conflicts with its Kind, so its holder may cache. It covers every open
// of its client under its Key, an oplock's one open, or, a delegation, none.
//
// decision: what the adapter keeps with a lease is its parent key, typed as
// Parent, as the stored lease's ParentKey is, rather than opaque Adapter
// bytes. Add Adapter if an adapter needs other bytes kept with a lease.
type CachingGrant struct {
	ID     GrantID
	Client entity.ClientID
	File   entity.FileID
	Kind   GrantKind
	Key    LeaseKey // zero: it covers one open, or none
	Epoch  uint16   // a v2 lease's: starts at its request's, raised by every change of Kind sent to the holder
	V2     bool     // a v2 lease, whose Epoch is counted and sent; fixed when the lease is made
	Parent LeaseKey // a v2 lease's parent key, as its first request named it; never read here
	Recall *Recall  // a recall in flight; nil: none
}

// Recall is a recall in flight.
type Recall struct {
	To GrantKind // the kind it goes to; none: a return
}
