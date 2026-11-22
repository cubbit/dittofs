package identity_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// sambaCapture is alice.json (testdata/pac/README.md): the time alice's
// captured AP-REQ was made, and her SID and her groups' by name, as
// samba-tool read them.
type sambaCapture struct {
	Time time.Time         `json:"time"`
	SIDs map[string]string `json:"sids"`
}

// alice's AP-REQ for cifs/, issued by the Samba fixture's domain controller,
// signs in under the captured service.keytab at the capture's time, through
// her krb row linked for her captured SID: her SID comes first, and her
// caller at Resolve holds Domain Users, devs and engineering, which devs is
// nested in, each seeded by its SID alone, so only her PAC can name it.
// Catches a verifier that accepts only what the test realm builds.
func TestKerberosSambaTicket(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", "pac", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var c sambaCapture
	if err := json.Unmarshal(read("alice.json"), &c); err != nil {
		t.Fatalf("alice.json: %v", err)
	}
	if c.Time.IsZero() || len(c.SIDs) != 4 {
		t.Fatalf("alice.json: time %v, sids %v; want a time and 4 SIDs", c.Time, c.SIDs)
	}
	k := kerberosServer(t)
	k.applyKeytab(t, read("service.keytab"))
	k.clock.Jump(c.Time.Sub(k.clock.Now()))
	alice := entity.User{Principal: entity.NewUser(), Name: "alice",
		IDs: append(fileIDs("1500"), linkedKrbID("alice@DITTOFS.AD", c.SIDs["alice"]))}
	k.user(t, alice, 0)
	groups := map[string]entity.Principal{}
	for _, g := range []string{"Domain Users", "devs", "engineering"} {
		groups[g] = k.group(t, g, c.SIDs[g])
	}

	res := k.signInAs(t, "alice's captured AP-REQ", pid.SignInStep{Kerberos: read("alice.apreq"),
		Service: pid.ServiceCIFS, Transport: pid.TransportSMB, Client: krbClient}, alice.Principal)
	if got := sidStrings(res.Identity.Evidence.SIDs); len(got) == 0 || got[0] != c.SIDs["alice"] {
		t.Errorf("SIDs = %v, want alice's %s first", got, c.SIDs["alice"])
	}
	caller, err := k.srv.Resolve(context.Background(), res.Identity, onShare(entity.SquashNone, entity.AnonymousPrincipal), pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for name, g := range groups {
		if !slices.Contains(caller.Groups, g) {
			t.Errorf("caller's groups %v lack %s (%s), which alice's PAC lists", caller.Groups, name, c.SIDs[name])
		}
	}
}
