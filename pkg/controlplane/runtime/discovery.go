package runtime

// DiscoveryNameKey is the system-settings key holding the instance-wide name
// advertised on the LAN by the service-discovery advertisers (mDNS /
// WS-Discovery). Set it with `dfsctl settings set discovery.name "<name>"`.
//
// decision: nothing reads this key on this branch until the cutover's
// discovery sidecars (RFC 20 §3.5) return; the advertisers that read it went
// with the v1 adapters. It stays because an operator's stored value is
// persisted surface.
const DiscoveryNameKey = "discovery.name"
