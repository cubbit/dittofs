package bootstrap

import "strings"

// The rules of the v1 table, spelled once.
const (
	// dropout is the rule of a v1 key that is not read any more and has no
	// replacement to point at.
	dropout = "no longer read; remove it"

	// handedOff is the rule of a v1 family that is now a record or a
	// setting, which only the management API writes.
	handedOff = "no longer read; a record or a setting, set it through the management API"

	// databaseRule is the rule of the v1 database block (RFC 13 §6).
	databaseRule = "no longer read; the metadata store is metadata.backend"

	// drainRule is the rule of the v1 shutdown timeout, which a setting
	// now bounds one adapter at a time.
	drainRule = "no longer read; set adapters.<protocol>.drain_timeout through the management API"
)

// moved is the rule of a v1 key that has a replacement key.
func moved(replacement string) string {
	return "no longer read; write " + replacement
}

// retiredKey is one row of RFC 13 Appendix C's "From the files of today"
// table: an exact key or a family prefix, with the rule its refusal reads.
type retiredKey struct {
	key string
	// family says key is the prefix of a family: the key itself and every
	// key under it match, and so does the variable of either.
	family bool
	rule   string
}

// covers reports whether key is the entry's: the key itself, or a key under
// it for a family. A name that only shares letters with a family is not.
func (e retiredKey) covers(key string) bool {
	if e.family {
		return key == e.key || strings.HasPrefix(key, e.key+".")
	}

	return key == e.key
}

// retiredKeys are the v1 keys a start refuses, kept in step with the RFC's
// table by TestRetiredKeysMatchAppendixC. The four keys the typed decode
// refuses (an upper-case logging.level, a bare logging.rotation.max_size)
// or keeps (logging.rotation.max_backups and max_age) have no row: their
// own checks say what to write.
//
// Today no entry contains another, so the longest-match rule of the lookups
// only matters once one is added.
var retiredKeys = []retiredKey{
	{"database", true, databaseRule},
	{"metadata.badger.block_cache_mb", false, dropout},
	{"metadata.badger.index_cache_mb", false, dropout},
	{"controlplane.tls.client_ca", false, dropout},
	{"controlplane.tls.min_version", false, dropout},
	{"controlplane.require_initial_password_change", false, dropout},
	{"controlplane.drain_stall_timeout", false, dropout},
	{"controlplane.read_timeout", false, dropout},
	{"controlplane.write_timeout", false, dropout},
	{"controlplane.idle_timeout", false, dropout},
	{"controlplane.host", false, moved("api.listen")},
	{"controlplane.port", false, moved("api.listen")},
	{"controlplane.tls.cert_file", false, moved("api.tls.cert_file")},
	{"controlplane.tls.key_file", false, moved("api.tls.key_file")},
	{"controlplane.jwt", true, dropout},
	{"controlplane.pprof", false, dropout},
	{"controlplane.pprof_mutex_rate", false, dropout},
	{"controlplane.pprof_block_rate_ns", false, dropout},
	{"metrics.enabled", false, moved("metrics.listen")},
	{"metrics.host", false, moved("metrics.listen")},
	{"metrics.port", false, moved("metrics.listen")},
	{"metrics.auth", false, dropout},
	{"metrics.token_file", false, dropout},
	{"metrics.path", false, dropout},
	{"metrics.tls", true, dropout},
	{"blockstore.journal.path", false, moved("journals")},
	{"blockstore.journal.chunk_size", false, dropout},
	{"blockstore.journal.chunk_max", false, dropout},
	{"blockstore.journal.dirty_expire", false, dropout},
	{"blockstore.journal.max_log_bytes", false, dropout},
	{"blockstore.journal.backpressure_max_wait", false, dropout},
	{"shutdown_timeout", false, drainRule},
	{"admin", true, handedOff},
	{"kerberos", true, handedOff},
	{"identity", true, handedOff},
	{"ldap", true, handedOff},
	{"gc", true, handedOff},
	{"snapshot", true, handedOff},
	{"integrity", true, handedOff},
}

// retiredAliases are the v1 families a key was read under two spellings:
// the spelling on the left is looked up as the one on the right.
//
// blockstore.local.* is the intermediate name the loader of the
// previous release still refused, so it takes the end of its
// blockstore.journal.* twin, and an operator still writing it is pointed at
// the same replacement as one writing the twin. Withdraw it when no release
// anyone upgrades across still refused the name.
var retiredAliases = map[string]string{
	"blockstore.local": "blockstore.journal",
}

// retiredRule is the rule of a key of the v1 table, matched exactly as
// written: a key in another case is an unknown key, never a retired one.
// The longest entry that covers the key wins.
func retiredRule(key string) (string, bool) {
	for old, replacement := range retiredAliases {
		if key == old || strings.HasPrefix(key, old+".") {
			key = replacement + strings.TrimPrefix(key, old)

			break
		}
	}

	rule, found, length := "", false, -1

	for _, e := range retiredKeys {
		if e.covers(key) && len(e.key) > length {
			rule, found, length = e.rule, true, len(e.key)
		}
	}

	return rule, found
}
