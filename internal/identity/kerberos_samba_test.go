package identity_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// sambaCapture is alice.json (testdata/pac/README.md): the time alice's
// captured AP-REQ was made, and her SID and her groups' by name, as
// samba-tool read them.
type sambaCapture struct {
	Time time.Time         `json:"time"`
	SIDs map[string]string `json:"sids"`
}

func readCapture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pac", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// alice's AP-REQ for cifs/, issued by the Samba fixture's domain controller,
// signs in under the captured service.keytab at the capture's time, through
// her krb row linked for her captured SID: her SID comes first, and her
// caller at Resolve holds Domain Users, devs and engineering, which devs is
// nested in, each seeded by its SID alone, so only her PAC can name it.
// Catches a verifier that accepts only what the test realm builds.
func TestKerberosSambaTicket(t *testing.T) {
	read := func(name string) []byte { return readCapture(t, name) }
	var c sambaCapture
	if err := json.Unmarshal(read("alice.json"), &c); err != nil {
		t.Fatalf("alice.json: %v", err)
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

// alice's AP-REQ for cifs/nas.dittofs.test, issued by an MIT KDC (krb5 1.20),
// whose PAC holds no logon information, signs in under the captured
// mit-service.keytab at the capture's time, in DITTOFS.TEST, a keytab realm
// other than the directory's: it binds the local alice, and her caller at
// Resolve holds staff, the store group she is in. Catches an MIT PAC refused,
// or its missing logon information read as an empty group list.
func TestKerberosMITTicket(t *testing.T) {
	var c sambaCapture
	if err := json.Unmarshal(readCapture(t, "mit-alice.json"), &c); err != nil {
		t.Fatalf("mit-alice.json: %v", err)
	}
	k := kerberosServer(t)
	st := state(1)
	st.Settings.NetBIOSName = "NAS01"
	st.Keytab = []secrets.Version{{Number: 1, Value: readCapture(t, "mit-service.keytab")}}
	st.Directory = &directory.Record{Schema: directory.SchemaAD, Address: "ldaps://dc.corp.example", Realm: "CORP.EXAMPLE"}
	if err := k.srv.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	k.clock.Jump(c.Time.Sub(k.clock.Now()))
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	k.user(t, alice, 0)
	staff := k.group(t, "staff", "")
	k.member(t, staff, alice.Principal)

	res := k.signInAs(t, "alice's MIT AP-REQ", pid.SignInStep{Kerberos: readCapture(t, "mit-alice.apreq"),
		Service: pid.ServiceCIFS, Transport: pid.TransportSMB, Client: krbClient}, alice.Principal)
	caller, err := k.srv.Resolve(context.Background(), res.Identity, onShare(entity.SquashNone, entity.AnonymousPrincipal), pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !slices.Contains(caller.Groups, staff) {
		t.Errorf("caller's groups %v lack staff %v, which alice is a member of in the store", caller.Groups, staff)
	}
}
