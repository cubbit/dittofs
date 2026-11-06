//go:build linux

package fixture

import (
	"strings"
	"testing"
)

// Both mounts get the options RFC 31 §3.4 requires; the profile and the
// authentication change only their own options.
func TestMountOptions(t *testing.T) {
	c := Config{Auth: "ntlm", Profile: "sd"}
	o := c.mountOpts(445)
	for _, want := range []string{"vers=3.1.1", "multiuser", "sec=ntlmssp", "noperm", "cache=none", "actimeo=0",
		"nolease", "nohandlecache", "rsize=1048576", "wsize=1048576", "serverino", "soft"} {
		if !strings.Contains(","+o+",", ","+want+",") {
			t.Errorf("missing %s in %s", want, o)
		}
	}
	if strings.Contains(o, "cifsacl") || strings.Contains(o, "nobrl") || strings.Contains(o, "port=") {
		t.Errorf("unexpected option in %s", o)
	}
	c = Config{Auth: "krb5", Profile: "posix"}
	o = c.mountOpts(smbPort)
	for _, want := range []string{"sec=krb5", "cifsacl", "port=12445"} {
		if !strings.Contains(o, want) {
			t.Errorf("missing %s in %s", want, o)
		}
	}
}

func TestCleanRefRefusesEscapes(t *testing.T) {
	h := Hooks{}
	for _, d := range []string{"", "../x", "a/b", ".", ".."} {
		if err := h.CleanRef(d); err == nil {
			t.Errorf("CleanRef(%q) accepted", d)
		}
	}
}
