// Package syncer is the syncer's contract: the types its callers and its
// implementation share. It holds no state and does no I/O of its own.
//
// The syncer moves blocks between the node and a remote store, in two halves
// that share one component: the upload half puts blocks, the fetch half gets
// chunks. A registered store and an open flow are known by the identifiers
// declared here, and Stats is the snapshot of the counters the syncer keeps
// for each half and store.
package syncer
