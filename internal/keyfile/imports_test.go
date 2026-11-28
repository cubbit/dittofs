package keyfile

import (
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
)

// The key file's format reaches nothing of the module and, outside the
// standard library, only Argon2 (with the BLAKE2b it brings) and the UUID
// package, so the provider and pkg/keyprovider can both import it.
func TestImportWalk(t *testing.T) {
	graphs, err := archtest.Walk(".", false)
	if err != nil {
		t.Fatal(err)
	}
	table := []archtest.Row{{
		Pkg:  "internal/keyfile",
		Libs: []string{"golang.org/x/crypto/argon2", "golang.org/x/crypto/blake2b", "golang.org/x/sys/cpu", "github.com/google/uuid"},
	}}
	for _, v := range archtest.Check(table, graphs...) {
		t.Error(v)
	}
}
