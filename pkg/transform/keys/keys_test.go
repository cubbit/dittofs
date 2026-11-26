package keys_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// The kinds are stored with every key record and named by other components,
// so a changed string strands the material recorded under the old one.
func TestKindStrings(t *testing.T) {
	for _, c := range []struct{ name, got, want string }{
		{"KindDataKey", keys.KindDataKey, "data-key"},
		{"KindChunkIDKey", keys.KindChunkIDKey, "chunk-id-key"},
		{"KindChunkingKey", keys.KindChunkingKey, "chunking-key"},
		{"KindExportKey", keys.KindExportKey, "export-key"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// The codec matches an unknown-material failure by the sentinel and reads the
// ID it names from the error itself, through any wrapping on the way up.
func TestUnknownMaterialErrorNamesTheID(t *testing.T) {
	id := keys.MaterialID{Kind: keys.KindDataKey, ID: "dk-7", Fingerprint: [32]byte{1}}
	err := fmt.Errorf("decode: %w", &keys.UnknownMaterialError{ID: id})

	if !errors.Is(err, keys.ErrUnknownMaterial) {
		t.Errorf("errors.Is(%v, ErrUnknownMaterial) = false, want true", err)
	}
	var unknown *keys.UnknownMaterialError
	if !errors.As(err, &unknown) {
		t.Fatalf("errors.As(%v, *UnknownMaterialError) = false, want true", err)
	}
	if unknown.ID != id {
		t.Errorf("UnknownMaterialError.ID = %+v, want %+v", unknown.ID, id)
	}
}
