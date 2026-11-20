package identity

import (
	"log/slog"
	"strings"

	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
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

// buildRing parses each keytab version into one ring, and returns the name
// of each key the versions it keeps hold.
//
// decision: a version holding a key that is not AES, or that does not parse,
// contributes no key and no name, whole, and is logged, while Apply still
// succeeds: refusing the whole State would also hold back every other change
// it carries, a disable among them, and the import's own check refuses such a
// keytab before it is ever stored. Overturned if a keytab can reach the store
// without that check.
func buildRing(versions []secrets.Version, log *slog.Logger) (keyRing, []identity.KeytabName) {
	ring := keyRing{}
	var names []identity.KeytabName
	for _, v := range versions {
		n, err := identity.KeytabNames(v.Value)
		if err != nil {
			log.Warn("identity: keytab version dropped", "version", v.Number, "err", err)
			continue
		}
		kt := keytab.New()
		if err := kt.Unmarshal(v.Value); err != nil {
			log.Warn("identity: keytab version dropped", "version", v.Number, "err", err)
			continue
		}
		names = append(names, n...)
		for _, e := range kt.Entries {
			k := ringKey{foldASCII(strings.Join(e.Principal.Components, "/")), e.Principal.Realm, e.KVNO, e.Key.KeyType}
			if _, held := ring[k]; !held { // the current version, first, wins
				ring[k] = e.Key
			}
		}
	}
	return ring, names
}

// AppliedKeytab is the reference of the keytab in force, the number of its
// current version and the name of each key its versions in force hold, as
// Apply read them, a version buildRing dropped holding none; zero and none
// while no keytab is in force.
func (s *Server) AppliedKeytab() (entity.SecretRef, uint64, []identity.KeytabName) {
	st := s.state.Load()
	if st == nil || len(st.Keytab) == 0 {
		return entity.SecretRef{}, 0, nil
	}
	return st.Settings.Keytab, st.Keytab[0].Number, st.keytabNames
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

// holdsKeys reports whether st holds any service key, imported or derived.
func (st *State) holdsKeys() bool { return len(st.keys) > 0 || len(st.machine.keys) > 0 }

// keytabRealm reports whether realm, compared as the KDC spells it, is the
// realm of a service key st holds.
func (st *State) keytabRealm(realm string) bool {
	for k := range st.keys {
		if k.realm == realm {
			return true
		}
	}
	return len(st.machine.keys) > 0 && st.machine.realm == realm
}
