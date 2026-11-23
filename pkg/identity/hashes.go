package identity

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"slices"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/md4" //nolint:staticcheck // SA1019: NTLM's NT hash is MD4 by definition

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// NewHashes derives what a new password is stored as: the password hash and,
// only while ntlm is on and u holds file spellings and is not a directory
// principal, the NT hash. It is pure: it reads and writes nothing. The API
// writes the result with the record, conditional on ntlm.Generation still being
// current; for a directory principal it fails vfserr.ErrInvalid.
//
// The password hash is argon2id in the PHC string form, its parameters in the
// string, so a later change of them re-hashes at the next password set.
//
// decision: the NT hash is MD4 of the password, unsalted, and is as good as
// the password for NTLM, so while NTLM is on it undoes argon2id for its user:
// a leaked store lets an attacker sign in over NTLM without cracking anything.
// It is the price of NTLM sign-in; turning NTLM off deletes every NT hash and
// derives no new one.
func NewHashes(u entity.User, password []byte, ntlm NTLMSetting) (PasswordHashes, error) {
	// The NT hash reads the password as UTF-16 and argon2id as bytes, so a
	// password that is not UTF-8 would be two different passwords.
	if !utf8.Valid(password) {
		return PasswordHashes{}, fmt.Errorf("%w: a password is UTF-8 text", vfserr.ErrInvalid)
	}
	if holds(u, entity.SchemeDir) {
		return PasswordHashes{}, fmt.Errorf("%w: a directory principal has no local password", vfserr.ErrInvalid)
	}
	salt := make([]byte, argonSaltLen)
	_, _ = rand.Read(salt) // crypto/rand.Read never fails
	key := argon2.IDKey(password, salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	h := PasswordHashes{Password: fmt.Appendf(nil, "$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))}
	if ntlm.On && holds(u, entity.SchemeUID) {
		h.NTHash = ntHash(password)
	}
	return h, nil
}

// The argon2id parameters: 19 MiB, two passes, one lane, a 16-byte salt and
// a 32-byte key.
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonSaltLen = 16
	argonKeyLen  = 32
)

// ntHash is MD4 over the password's UTF-16LE form, NTLM's NTOWFv1. It
// decodes the bytes in place rather than through a string, and clears the
// UTF-16 copy and md4's buffer once hashed. One copy is beyond its reach:
// Sum pads a private copy of md4's state, last partial block included, and
// leaves it to the garbage collector.
func ntHash(password []byte) []byte {
	b := make([]byte, 0, 2*len(password)) // a UTF-8 byte makes at most one UTF-16 unit
	defer clear(b[:cap(b)])
	for p := password; len(p) > 0; {
		r, n := utf8.DecodeRune(p)
		p = p[n:]
		if r1, r2 := utf16.EncodeRune(r); r1 != utf8.RuneError {
			b = binary.LittleEndian.AppendUint16(b, uint16(r1))
			b = binary.LittleEndian.AppendUint16(b, uint16(r2))
		} else {
			b = binary.LittleEndian.AppendUint16(b, uint16(r))
		}
	}
	m := md4.New()
	_, _ = m.Write(b) // a hash never fails a write
	sum := m.Sum(nil)
	_, _ = m.Write(make([]byte, md4.BlockSize)) // one block overwrites the buffered tail
	return sum
}

func holds(u entity.User, scheme entity.ProtocolScheme) bool {
	return slices.ContainsFunc(u.IDs, func(id entity.ProtocolID) bool { return id.Scheme == scheme })
}
