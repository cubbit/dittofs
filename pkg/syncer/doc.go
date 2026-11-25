// Package syncer is the syncer's contract: the types its callers and its
// implementation share, and nothing that holds state.
//
// The syncer moves blocks between the node and a remote store, in two halves
// that share one component: the upload half puts blocks, the fetch half gets
// chunks. This package declares the identifiers a registration and a flow are
// known by, and Stats, the snapshot of the counters the syncer keeps for each
// half and store. It imports the standard library only.
package syncer
