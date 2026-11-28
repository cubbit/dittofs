// Package provider is the material provider of one store configuration (RFC 5
// §3.1, Appendix B.1 and B.2): its custody holds the configuration's master
// key, either read from its key file or kept by a KMIP key service that wraps
// and unwraps for it, and it keeps every namespace key as a key record in the
// metadata store, wrapped under that master key. It reaches the metadata store
// only through KeyRecords, the view it declares, which the store implements
// without importing this package.
package provider
