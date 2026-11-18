package settings

import (
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// The sections of an identity component and its OpenID Connect child.
type (
	identityMode struct {
		valid
		Mode string `json:"mode" class:"live"`
	}
	oidcIssuer struct {
		valid
		Issuer string `json:"issuer" class:"live"`
	}
	identityWithOIDC struct {
		valid
		OIDC string `json:"oidc" class:"live"`
	}
	identityDotted struct {
		valid
		Issuer string `json:"oidc.issuer" class:"live"`
	}
	identityOIDCMode struct {
		valid
		OIDCMode string `json:"oidc_mode" class:"live"`
	}
	smbA struct {
		valid
		Port uint16 `json:"port" class:"restart"`
	}
	smbB struct {
		valid
		Port uint16 `json:"port" class:"restart"`
	}
)

func TestLookupLongestPrefix(t *testing.T) {
	prof.Watch(t, prof.Options{})

	table := Table{
		Of("identity", installation, identityMode{}),
		Of("identity.oidc", installation, oidcIssuer{}),
	}

	var idx index

	if text := panicText(t, func() {
		idx = table.index()
	}); text != "" {
		t.Fatalf("index of a valid table panicked: %s", text)
	}

	cases := []struct {
		key  string
		want string // the section's prefix, "" for none
	}{
		{"identity.oidc.issuer", "identity.oidc"},
		{"identity.oidc", "identity.oidc"},
		{"identity.oidcx", "identity"},
		{"identity.mode", "identity"},
		{"identityx.mode", ""},
		{"other.key", ""},
	}

	for _, c := range cases {
		got := ""
		if sec, ok := idx.lookup(c.key); ok {
			got = sec.prefix
		}

		if got != c.want {
			t.Errorf("lookup(%q) = section %q, want %q", c.key, got, c.want)
		}
	}
}

func TestTablePanicsSamePrefix(t *testing.T) {
	prof.Watch(t, prof.Options{})

	table := Table{
		Of("adapters.smb", installation, smbA{}),
		Of("adapters.smb", installation, smbB{}),
	}

	got := panicText(t, func() {
		table.index()
	})

	const want = "settings: sections adapters.smb (settings.smbA) and " +
		"adapters.smb (settings.smbB) declare one prefix"
	if got != want {
		t.Errorf("panic = %q, want %q", got, want)
	}
}

func TestTablePanicsConsumedKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	child := Of("identity.oidc", installation, oidcIssuer{})

	cases := []struct {
		name   string
		parent Section
		want   string
	}{
		{
			"parent field named as the child's prefix",
			Of("identity", installation, identityWithOIDC{}),
			"settings: section identity (settings.identityWithOIDC) declares key " +
				"identity.oidc, which section identity.oidc (settings.oidcIssuer) takes",
		},
		{
			"parent json name with a dot under the child's prefix",
			Of("identity", installation, identityDotted{}),
			"settings: section identity (settings.identityDotted) declares key " +
				"identity.oidc.issuer, which section identity.oidc (settings.oidcIssuer) takes",
		},
		{
			"parent field sharing only the child's first letters",
			Of("identity", installation, identityOIDCMode{}),
			"",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			table := Table{c.parent, child}
			if got := panicText(t, func() {
				table.index()
			}); got != c.want {
				t.Errorf("panic = %q, want %q", got, c.want)
			}
		})
	}
}
