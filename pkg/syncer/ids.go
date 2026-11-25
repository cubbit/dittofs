package syncer

// StoreID names a store registered with the syncer. The syncer mints it at
// registration and never mints zero, so a zero StoreID names no store.
type StoreID uint64

// FlowID names a flow open on a registered store. The syncer mints it when
// the flow opens and never mints zero. Stats reports a flow's queue gauges
// under it, and the node maps it to the share the flow serves.
type FlowID uint64

// ProbeKey says which stores share health probes: every store registered under
// the same key shares them. The composer passes a digest of the store's
// endpoint, credential and bucket, so no credential text is held in the key.
// The zero key shares nothing: each store registered under it is probed alone.
type ProbeKey [32]byte

// Probes says which directions the syncer probes for a store. A store whose
// put probe would write where it must not, such as an immutable backup
// location, is registered with PutProbe false.
type Probes struct{ PutProbe, GetProbe bool }
