package secrets

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

const rfc13 = "../../docs/internals/rfcs/rfc-13-configuration.md"

// sealsPhrase names, for each kind, the words of RFC 13 §7's "Seals" cell
// that name it. §7's table names what each role seals in prose, not by
// kind, so the phrase is matched as a substring of the whole cell, long
// enough to sit in one row only.
var sealsPhrase = map[entity.SecretKind]string{
	entity.SecretNTHash:          "NT hashes",
	entity.SecretKeytab:          "keytabs",
	entity.SecretMachinePassword: "the machine-account password NTLM pass-through signs in with",
	entity.SecretPassword: "password hashes, which only the management API's " +
		"sign-in verifies",
	entity.SecretRemoteCredential: "remote-tier credentials",
	entity.SecretKey:              "the management API's token-signing key",
}

// TestKindRolesMatchRFC checks every declared kind's role against RFC 13
// §7's table, so a kind sealed under the other role, which would let a
// protocol-only node open a storage secret, fails here.
func TestKindRolesMatchRFC(t *testing.T) {
	rows := wrappingKeyRows(t)
	if len(rows) == 0 {
		t.Fatalf("no row read from RFC 13 §7's wrapping-key table in %s", rfc13)
	}

	for _, r := range []Role{Protocol, Storage} {
		if _, ok := rows[r.String()]; !ok {
			t.Errorf("RFC 13 §7 has no row for role %q; rows %q", r, rows)
		}
	}

	if len(rows) != 2 {
		t.Errorf("RFC 13 §7 has %d roles, want 2: %q", len(rows), rows)
	}

	kinds := 0

	for k := entity.SecretKind(1); k.String() != ""; k++ {
		kinds++

		role, ok := roleOf(k)
		if !ok {
			t.Errorf("kind %q has no role", k)

			continue
		}

		phrase, ok := sealsPhrase[k]
		if !ok {
			t.Errorf("kind %q has no phrase of RFC 13 §7 to check its role against", k)

			continue
		}

		var in []string

		for name, seals := range rows {
			if strings.Contains(seals, phrase) {
				in = append(in, name)
			}
		}

		if len(in) != 1 {
			t.Errorf(
				"kind %q: phrase %q is in rows %q of RFC 13 §7, want exactly one",
				k,
				phrase,
				in,
			)

			continue
		}

		if role.String() != in[0] {
			t.Errorf("kind %q is sealed under %q, RFC 13 §7 gives %q", k, role, in[0])
		}
	}

	if kinds != len(sealsPhrase) {
		t.Errorf("%d kinds declared, %d phrases known", kinds, len(sealsPhrase))
	}
}

// wrappingKeyRows reads RFC 13 §7's table, role to its "Seals" cell.
func wrappingKeyRows(t *testing.T) map[string]string {
	t.Helper()

	f, err := os.Open(rfc13)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = f.Close()
	})

	rows := map[string]string{}
	in := false
	sc := bufio.NewScanner(f)

	for sc.Scan() {
		line := sc.Text()

		switch {
		case strings.HasPrefix(line, "| Wrapping key | Seals | Needed by |"):
			in = true
		case !in || strings.HasPrefix(line, "| ---"):
		case !strings.HasPrefix(line, "|"):
			return rows
		default:
			cells := strings.Split(line, "|")
			if len(cells) != 5 {
				t.Fatalf("RFC 13 §7 row %q has %d cells, want 3", line, len(cells)-2)
			}

			rows[strings.Trim(strings.TrimSpace(cells[1]), "`")] = strings.TrimSpace(cells[2])
		}
	}

	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	return rows
}
