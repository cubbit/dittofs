package identity

import (
	"log/slog"
	"strings"

	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// ringKey names one key of the ring: the service name, its components joined
// by "/" with ASCII letters folded to lower case, its realm as written, the
// key version number and the encryption type.
type ringKey struct {
	name  string
	realm string
	kvno  uint32
	etype int32
}

// keyRing is every key of the keytab versions in force, built at every
// Apply, never per sign-in.
type keyRing map[ringKey]types.EncryptionKey

// buildRing parses each keytab version into one ring.
//
// decision: a version holding a key that is not AES, or that does not parse,
// contributes no key, whole, and is logged, while Apply still succeeds:
// refusing the whole State would also hold back every other change it
// carries, a disable among them, and the import's own check refuses such a
// keytab before it is ever stored. Overturned if a keytab can reach the store
// without that check.
func buildRing(versions []secrets.Version, log *slog.Logger) keyRing {
	ring := keyRing{}
	for _, v := range versions {
		if err := identity.CheckKeytab(v.Value); err != nil {
			log.Warn("identity: keytab version dropped", "version", v.Number, "err", err)
			continue
		}
		kt := keytab.New()
		if err := kt.Unmarshal(v.Value); err != nil {
			log.Warn("identity: keytab version dropped", "version", v.Number, "err", err)
			continue
		}
		for _, e := range kt.Entries {
			k := ringKey{foldASCII(strings.Join(e.Principal.Components, "/")), e.Principal.Realm, e.KVNO, e.Key.KeyType}
			if _, held := ring[k]; !held { // the current version, first, wins
				ring[k] = e.Key
			}
		}
	}
	return ring
}

// AppliedKeytab is the number of the keytab's current version in force and
// the name of each key its versions in force hold, zero and none while no
// keytab is in force. A version that does not parse fails the call, since
// the API's check of a join against it cannot then see its names.
func (s *Server) AppliedKeytab() (uint64, []identity.KeytabName, error) {
	st := s.state.Load()
	if st == nil || len(st.Keytab) == 0 {
		return 0, nil, nil
	}
	var names []identity.KeytabName
	for _, v := range st.Keytab {
		n, err := identity.KeytabNames(v.Value)
		if err != nil {
			return 0, nil, err
		}
		names = append(names, n...)
	}
	return st.Keytab[0].Number, names, nil
}

// key is the ring's key for a service name, compared with ASCII letters
// folded, in realm, under kvno and etype.
func (r keyRing) key(name, realm string, kvno uint32, etype int32) (types.EncryptionKey, bool) {
	k, ok := r[ringKey{foldASCII(name), realm, kvno, etype}]
	return k, ok
}

// foldASCII lower-cases s's ASCII letters and leaves every other byte.
func foldASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
