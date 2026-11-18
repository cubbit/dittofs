package config

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/cli/output"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/google/uuid"
)

// nodeUUID is the node identity these tests show, in RFC 13 Appendix C's form.
const nodeUUID = "6f1c2d3e-4b5a-4978-8a6b-1c2d3e4f5a6b"

// show prints node.id as the UUID RFC 13 Appendix C writes, not as the 16
// numbers of its array, in YAML and in JSON.
func TestConfigShowPrintsNodeIDAsUUID(t *testing.T) {
	cfg := bootstrap.GetDefaultConfig()
	cfg.Node.ID = entity.NodeID(uuid.MustParse(nodeUUID))

	for _, tc := range []struct {
		format output.Format
		want   string
	}{
		{output.FormatYAML, "\nnode:\n  id: " + nodeUUID + "\n"},
		{output.FormatJSON, "\"id\": \"" + nodeUUID + "\""},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			var out bytes.Buffer

			if err := printConfig(&out, cfg, tc.format); err != nil {
				t.Fatalf("printConfig: %v", err)
			}

			if !strings.Contains(out.String(), tc.want) {
				t.Errorf(
					"show in %s: node.id not printed as %q; output:\n%s",
					tc.format,
					tc.want,
					out.String(),
				)
			}
		})
	}
}
