package identity_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/md4" //nolint:staticcheck // SA1019: NTLM's NT hash is MD4 by definition

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// phcPrefix is the argon2id PHC string's fixed head: m = 19 MiB, t = 2, p = 1.
const phcPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"

// verifies decodes a PHC string independently of NewHashes and reports
// whether it is the hash of password, and its salt.
func verifies(t *testing.T, phc, password []byte) (bool, []byte) {
	t.Helper()
	rest, ok := strings.CutPrefix(string(phc), phcPrefix)
	if !ok {
		t.Fatalf("password hash %q, want the prefix %q", phc, phcPrefix)
	}
	salt64, key64, ok := strings.Cut(rest, "$")
	if !ok {
		t.Fatalf("password hash %q has no salt$key", phc)
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(salt64)
	key, err2 := base64.RawStdEncoding.DecodeString(key64)
	if err := errors.Join(err1, err2); err != nil || len(salt) != 16 || len(key) != 32 {
		t.Fatalf("password hash %q: salt %d bytes, key %d bytes, %v; want 16 and 32", phc, len(salt), len(key), err)
	}
	return bytes.Equal(argon2.IDKey(password, salt, 2, 19456, 1, 32), key), salt
}

func TestNewHashes(t *testing.T) {
	// MS-NLMP §4.2.2.1.2: NTOWFv1("Password").
	published, _ := hex.DecodeString("a4f49c406510bdcab6824ee7c30fd852")
	uid := entity.ProtocolID{Scheme: entity.SchemeUID, Value: "1500"}
	dir := entity.ProtocolID{Scheme: entity.SchemeDir, Value: "0d9a2c1e-6f11-4c55-9d1d-6a7b1f3c2e10"}
	file := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: entity.ProtocolIDs{uid}}
	apiOnly := entity.User{Principal: entity.NewUser(), Name: "admin"}
	directory := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: entity.ProtocolIDs{uid, dir}}
	on, off := identity.NTLMSetting{On: true, Generation: 7}, identity.NTLMSetting{Generation: 7}

	for _, c := range []struct {
		name   string
		pw     string
		u      entity.User
		ntlm   identity.NTLMSetting
		ntHash []byte
		err    error
	}{
		{"NTLM on, file user", "Password", file, on, published, nil},
		{"NTLM off, file user", "Password", file, off, nil, nil},
		{"NTLM on, API-only account", "Password", apiOnly, on, nil, nil},
		{"NTLM on, directory principal", "Password", directory, on, nil, vfserr.ErrInvalid},
		{"NTLM off, directory principal", "Password", directory, off, nil, vfserr.ErrInvalid},
		// Read as UTF-16 it would be U+FFFD U+FFFD, the NT hash of other passwords too.
		{"NTLM on, not UTF-8", "\xff\xff", file, on, nil, vfserr.ErrInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, err := identity.NewHashes(c.u, []byte(c.pw), c.ntlm)
			if !errors.Is(err, c.err) {
				t.Fatalf("NewHashes error = %v, want %v", err, c.err)
			}
			if err != nil {
				if h.Password != nil || h.NTHash != nil {
					t.Errorf("NewHashes refused with hashes %+v, want none", h)
				}
				return
			}
			if !bytes.Equal(h.NTHash, c.ntHash) {
				t.Errorf("NT hash = %x, want %x", h.NTHash, c.ntHash)
			}
			if ok, _ := verifies(t, h.Password, []byte("Password")); !ok {
				t.Errorf("password hash %q does not verify Password", h.Password)
			}
			if ok, _ := verifies(t, h.Password, []byte("password")); ok {
				t.Errorf("password hash %q verifies a wrong password", h.Password)
			}
		})
	}
}

// The NT hash of a password past ASCII, one rune outside the BMP among them,
// matches MD4 over the standard library's UTF-16 encoding of it.
func TestNewHashesNTHashEncodesUTF16(t *testing.T) {
	u := entity.User{Principal: entity.NewUser(), Name: "alice",
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "1500"}}}
	for _, pw := range []string{"Pässwörd", "pw\U0001F600!", "\u00e9\U00010348\u4e2d"} {
		var le []byte
		for _, u := range utf16.Encode([]rune(pw)) {
			le = binary.LittleEndian.AppendUint16(le, u)
		}
		m := md4.New()
		_, _ = m.Write(le)
		want := m.Sum(nil)
		h, err := identity.NewHashes(u, []byte(pw), identity.NTLMSetting{On: true})
		if err != nil {
			t.Fatalf("NewHashes(%q): %v", pw, err)
		}
		if !bytes.Equal(h.NTHash, want) {
			t.Errorf("NT hash of %q = %x, want %x", pw, h.NTHash, want)
		}
	}
}

// Two calls with one password draw two salts, and each hash verifies.
func TestNewHashesSaltsEachCall(t *testing.T) {
	u := entity.User{Principal: entity.NewUser(), Name: "alice"}
	a, err1 := identity.NewHashes(u, []byte("Password"), identity.NTLMSetting{})
	b, err2 := identity.NewHashes(u, []byte("Password"), identity.NTLMSetting{})
	if err := errors.Join(err1, err2); err != nil {
		t.Fatalf("NewHashes: %v", err)
	}
	okA, saltA := verifies(t, a.Password, []byte("Password"))
	okB, saltB := verifies(t, b.Password, []byte("Password"))
	if !okA || !okB {
		t.Errorf("hashes %q, %q verify Password: %t, %t; want both", a.Password, b.Password, okA, okB)
	}
	if bytes.Equal(saltA, saltB) {
		t.Errorf("two calls drew one salt %x, want two", saltA)
	}
}
