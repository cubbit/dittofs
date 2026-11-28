package s3

import "time"

// Recorder is what the store records through (RFC 4 §4.12); the node's content
// wiring implements it over the root's registry, one per store configuration,
// which supplies the store label. op is a call's op label, result is ok, a
// sentinel's name or local, and cause is a denial's cause, empty otherwise.
type Recorder interface {
	// Call records one call: its outcome, how long it took and the body bytes
	// it moved.
	Call(op, result, cause string, took time.Duration, bytes int64)
	// ConnOpened records one connection the client opened.
	ConnOpened()
	// ForeignKey records one key under the block namespace a listing skipped
	// because it is not a block name.
	ForeignKey()
	// Capability records one feature's verdict at the last capability check
	// of the namespace under prefix: present, or missing or unverified.
	Capability(prefix, feature string, present bool)
}
