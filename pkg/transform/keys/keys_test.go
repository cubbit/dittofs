package keys_test

import (
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
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}
