package identity

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/crypto/common"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/cubbit/dittofs/pkg/secrets"
)

// s2kIterations is RFC 3962's default PBKDF2 iteration count, the one Active
// Directory derives a computer account's AES keys with.
const s2kIterations = 4096

// machineEtypes are the encryption types derived from the machine password.
var machineEtypes = []int32{etypeID.AES256_CTS_HMAC_SHA1_96, etypeID.AES128_CTS_HMAC_SHA1_96}

// machineKey is one key derived from one version of the machine password.
type machineKey struct {
	key      types.EncryptionKey
	kvno     uint32
	notAfter time.Time // zero: none
}

// machineRing is the keys derived from the machine password for the service
// names the computer account holds. Every name shares one salt, so one key
// per version and encryption type serves them all.
type machineRing struct {
	realm string
	names map[string]bool        // ASCII letters folded to lower case
	keys  map[int32][]machineKey // by encryption type, current first
}

// machineSalt is the computer account's salt: the realm in upper case, "host",
// the account's name without its $ in lower case, "." and the realm in lower
// case.
func machineSalt(realm, account string) string {
	return strings.ToUpper(realm) + "host" + strings.ToLower(strings.TrimSuffix(account, "$")) + "." + strings.ToLower(realm)
}

// stringToKey is RFC 3962's string-to-key of password under salt with
// iterations, for etype.
func stringToKey(etype int32, password []byte, salt string, iterations uint32) (types.EncryptionKey, error) {
	et, err := crypto.GetEtype(etype)
	if err != nil {
		return types.EncryptionKey{}, err
	}
	k, err := et.StringToKey(string(password), salt, common.IterationsToS2Kparams(iterations))
	if err != nil {
		return types.EncryptionKey{}, err
	}
	return types.EncryptionKey{KeyType: etype, KeyValue: k}, nil
}

// buildMachineRing derives the machine password's keys from st, from each of
// machineVersions: none while the installation is not joined or has no
// directory, whose realm the keys are in.
//
// decision: the computer account's names are derived rather than imported,
// because their key and the machine password are one key; rotating them apart
// breaks every Kerberos sign-in for them at the first rotation. Samba keeps
// its keytab in step with the machine password the same way. Names on another
// account keep an imported keytab, since the machine password says nothing of
// their key. Overturned if no deployment registers a service name on the
// computer account, which leaves derivation without a user.
//
// ponytail: every Apply derives each version's keys again, two PBKDF2 runs of
// 4096 iterations per version, even when Machine and Join are unchanged.
// Reuse the current State's ring when they are equal once Apply's rate makes
// the derivation show up in a profile.
func buildMachineRing(st *State, log *slog.Logger) machineRing {
	j := &st.Join
	if !j.Joined || st.Directory == nil || len(j.Names) == 0 {
		return machineRing{}
	}
	m := machineRing{realm: st.Directory.Realm, names: map[string]bool{}, keys: map[int32][]machineKey{}}
	for _, n := range j.Names {
		m.names[foldASCII(n)] = true
	}
	salt := machineSalt(m.realm, j.Account)
	for _, v := range machineVersions(st) {
		for _, et := range machineEtypes {
			k, err := stringToKey(et, v.Value, salt, s2kIterations)
			if err != nil { // only an unknown etype fails, and machineEtypes are known
				log.Error("identity: machine password version derives no key", "version", v.Number, "err", err)
				continue
			}
			m.keys[et] = append(m.keys[et], machineKey{k, v.Meta.KeyVersion, v.notAfter})
		}
	}
	return m
}

// liveVersion is a machine password version that may sign in, with the time
// past which it no longer may; zero for none.
type liveVersion struct {
	secrets.Version
	notAfter time.Time
}

// machineVersions are the versions of st.Machine that derive keys and sign in
// the secure channel, current first: none marked HandedOver, none below the
// newest one under the one the last join or rotation confirmed, and that
// newest one below expiring the retention plus the clock skew after that
// confirm.
//
// decision: during the retention only the version just below the confirmed
// one stays usable, and older ones derive no key and sign in no channel, so a
// run of rotations within the retention never keeps an older password usable
// past the one rotation the retention covers. Overturned by a deployment
// whose controllers lag more than one rotation behind within the retention.
func machineVersions(st *State) []liveVersion {
	j := &st.Join
	var prev uint64 // the newest version below the confirmed one
	for _, v := range st.Machine {
		if v.Number < j.LastRotation.Version && v.Number > prev {
			prev = v.Number
		}
	}
	var out []liveVersion
	for _, v := range st.Machine {
		if v.Meta.HandedOver {
			continue
		}
		var notAfter time.Time
		if v.Number < j.LastRotation.Version {
			if v.Number != prev {
				continue
			}
			notAfter = j.LastRotation.Confirmed.Add(retention + clockSkew)
		}
		out = append(out, liveVersion{v, notAfter})
	}
	return out
}

// holds reports whether name in realm is one of the computer account's.
func (m *machineRing) holds(name, realm string) bool {
	return m.names[foldASCII(name)] && equalFoldASCII(realm, m.realm)
}

// candidates are the derived keys to try, in order, for a ticket for one of
// the account's names under kvno and etype at now, current first: the
// unexpired keys tagged kvno, two when a lagging controller gave two versions
// one number, or, when none is, every unexpired key of etype, so a ticket
// under a number not yet confirmed, or from a controller whose number lags,
// still verifies.
func (m *machineRing) candidates(kvno uint32, etype int32, now time.Time) []types.EncryptionKey {
	var tagged, live []types.EncryptionKey
	for _, k := range m.keys[etype] {
		if !k.notAfter.IsZero() && now.After(k.notAfter) {
			continue
		}
		if k.kvno == kvno {
			tagged = append(tagged, k.key)
		}
		live = append(live, k.key)
	}
	if len(tagged) > 0 {
		return tagged
	}
	return live
}

// dropMachineNames removes from r every key for a name the computer account
// holds and returns what it removed, nil when nothing; Apply logs each one
// the State it replaces did not already drop.
//
// decision: an imported key for a name the computer account holds contributes
// nothing to the ring and is logged, while Apply still succeeds: refusing the
// State would hold back every other change it carries, and the secret route
// refuses such an import itself once the account's names are stored.
// Overturned if an imported key can no longer reach the store for one of
// those names.
func (r keyRing) dropMachineNames(m *machineRing) map[ringKey]bool {
	var dropped map[ringKey]bool
	for k := range r {
		if m.holds(k.name, k.realm) {
			if dropped == nil {
				dropped = map[ringKey]bool{}
			}
			dropped[k] = true
			delete(r, k)
		}
	}
	return dropped
}

// serviceKeys are the keys to try, in order, for a ticket for name in realm
// under kvno and etype at now: the derived ones for a name the computer
// account holds, then the imported one, which Apply has dropped for such a
// name.
func (st *State) serviceKeys(name, realm string, kvno uint32, etype int32, now time.Time) ([]types.EncryptionKey, error) {
	var keys []types.EncryptionKey
	if st.machine.holds(name, realm) {
		keys = st.machine.candidates(kvno, etype, now)
	}
	if k, ok := st.keys.key(name, realm, kvno, etype); ok {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no key for %q in %q, kvno %d, etype %d", name, realm, kvno, etype)
	}
	return keys, nil
}
