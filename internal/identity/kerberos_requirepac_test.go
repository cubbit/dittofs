package identity_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/identity/directory"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// applyDirectory applies the fixture's keytab with a directory record of
// schema serving realm, requiring a PAC as require says, nil leaving
// require_pac unset. No directory is dialed: a sign-in reaching step 3 finds
// it unavailable.
func (k *kerberos) applyDirectory(t *testing.T, schema, realm string, require *bool) {
	t.Helper()
	st := state(1)
	st.Settings.NetBIOSName = "NAS01"
	st.Keytab = []secrets.Version{{Number: 1, Value: k.realm.Keytab()}}
	st.Directory = &directory.Record{Schema: schema, Address: "ldaps://dc.test.realm", Realm: realm, RequirePAC: require}
	if err := k.srv.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// pacAs is an SMB step presenting a cifs/nas.test ticket for client, issued
// now, whose PAC carries logon, or no logon information for nil.
func (k *kerberos) pacAs(client string, logon *identitytest.LogonInfo) pid.SignInStep {
	o := identitytest.PACOpts{Service: "cifs/nas.test", Client: client, AuthTime: k.clock.Now()}
	if logon != nil {
		o.Logon = []identitytest.LogonInfo{*logon}
	}
	return k.step(identitytest.TicketOpts{Client: client, PAC: k.realm.PAC(o)}, krbClient)
}

// rid is a logon information of client in the PAC domain with the RID rid.
func rid(client string, rid uint32) *identitytest.LogonInfo {
	return &identitytest.LogonInfo{Name: client, Domain: "TEST", DomainSID: pacDomain, UserRID: rid}
}

// RFC 18 §10's "PAC" clauses on the realm: in the directory's realm, a
// ticket with no PAC and one whose PAC holds no logon information, for a
// directory user and for backup, a name only a local user holds, are each
// refused ErrLogonFailure and counted no_pac, with the schema ad and again
// rfc2307 and with require_pac unset; with it false, backup's are accepted,
// bound by step 2. Catches a PAC-less ticket landing on a same-named local
// user, a schema read as exempting rfc2307, and require_pac never read.
func TestKerberosRequirePACInTheDirectoryRealm(t *testing.T) {
	no := false
	tickets := map[string]func(k *kerberos, client string) pid.SignInStep{
		"no PAC":                     func(k *kerberos, client string) pid.SignInStep { return k.as(client) },
		"a PAC with no logon buffer": func(k *kerberos, client string) pid.SignInStep { return k.pacAs(client, nil) },
	}
	for _, schema := range []string{directory.SchemaAD, directory.SchemaRFC2307} {
		for ticket, step := range tickets {
			for _, client := range []string{"alice", "backup"} {
				t.Run(schema+", "+ticket+", "+client, func(t *testing.T) {
					k := kerberosServer(t)
					k.user(t, entity.User{Principal: entity.NewUser(), Name: "backup", IDs: fileIDs("1510")}, 0)
					k.applyDirectory(t, schema, krbRealm, nil)
					before := k.kerberosSignIns(t, "smb", "no_pac")
					res, oc, err := k.srv.SignInOutcome(context.Background(), step(k, client))
					refused(t, client, res, oc, err, vfserr.ErrLogonFailure, "no_pac")
					if d := k.kerberosSignIns(t, "smb", "no_pac") - before; d != 1 {
						t.Errorf("identity_authentications_total{outcome=no_pac} rose by %v, want 1", d)
					}
				})
			}
		}
	}
	for ticket, step := range tickets {
		t.Run("require_pac false, "+ticket+", backup", func(t *testing.T) {
			k := kerberosServer(t)
			backup := entity.User{Principal: entity.NewUser(), Name: "backup", IDs: fileIDs("1510")}
			k.user(t, backup, 0)
			k.applyDirectory(t, directory.SchemaAD, krbRealm, &no)
			k.signInAs(t, "backup", step(k, "backup"), backup.Principal)
		})
	}
}

// With require_pac false, a PAC-less ticket of a directory user who has no
// local namesake is accepted and binds the principal step 3 mints; with it
// unset, the same ticket is refused no_pac. Catches require_pac read as
// refusing every PAC-less ticket in the directory's realm.
func TestKerberosRequirePACFalseAdmitsADirectoryUser(t *testing.T) {
	e := newAD(t)
	ctx := context.Background()
	e.d.AddUser(t, "dave", nil)
	realm := e.kerberos(t)
	if _, err := e.s.SignIn(ctx, e.ticket(realm, "dave", nil)); err == nil {
		t.Fatal("dave's PAC-less ticket with require_pac unset: signed in, want refused")
	}
	st := *e.s.Applied()
	rec := *st.Directory
	no := false
	rec.RequirePAC = &no
	st.Directory = &rec
	if err := e.s.Apply(st); err != nil {
		t.Fatal(err)
	}
	res, err := e.s.SignIn(ctx, e.ticket(realm, "dave", nil))
	if err != nil {
		t.Fatalf("dave's PAC-less ticket with require_pac false: %v", err)
	}
	a := e.entry(t, directory.ByUserName, "dave")
	if sp, err := e.spell(t, res.Identity.Evidence.Principal, pid.SchemeDir); err != nil || sp.Text != string(a.EntryID) {
		t.Errorf("the bound principal's dir spelling %q, %v; want dave's entry ID", sp.Text, err)
	}
}

// In a keytab realm other than the directory's, a ticket with no PAC and one
// whose PAC holds no logon information, for alice in a store group, are
// accepted, and the group's grant applies at Resolve. Catches the PAC
// required outside the directory's realm.
func TestKerberosNoPACOutsideTheDirectoryRealm(t *testing.T) {
	for name, step := range map[string]func(k *kerberos) pid.SignInStep{
		"no PAC":                     func(k *kerberos) pid.SignInStep { return k.as("alice") },
		"a PAC with no logon buffer": func(k *kerberos) pid.SignInStep { return k.pacAs("alice", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			k := kerberosServer(t)
			alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
			k.user(t, alice, 0)
			staff := k.group(t, "staff", "")
			k.member(t, staff, alice.Principal)
			k.applyDirectory(t, directory.SchemaAD, "CORP.EXAMPLE", nil)
			res := k.signInAs(t, name, step(k), alice.Principal)
			p := &pacSIDs{kerberos: k}
			if a := p.access(t, res.Identity, grantTo(staff, entity.AccessReadWrite)); a != entity.AccessReadWrite {
				t.Errorf("access through staff's grant %v, want %v", a, entity.AccessReadWrite)
			}
		})
	}
}

// RFC 18 §10's "PAC bound to the record": in the directory's realm, backup's
// krb row linked for directory backup's SID binds the local backup from a
// ticket whose PAC carries that SID; once the name is reused, a PAC carrying
// the new holder's SID is refused, logged naming backup's principal and the
// one holding the PAC's SID and no name or SID; a row linked for no SID
// refuses a PAC-bearing ticket; and outside the directory's realm a local
// alice whose record's SID is not her PAC's is refused. Catches a link a
// reused name inherits, an unlinked row binding whoever presents the name,
// and a binding checked on krb rows only, leaving step 2's match unbound.
func TestKerberosPACBoundToTheRecord(t *testing.T) {
	ctx := context.Background()
	t.Run("a linked row", func(t *testing.T) {
		k := kerberosServer(t)
		backup := entity.User{Principal: entity.NewUser(), Name: "backup", IDs: append(fileIDs("1510"), linkedKrbID("backup@TEST.REALM", pacDomain+"-1300"))}
		k.user(t, backup, 0)
		eve := entity.User{Principal: entity.NewUser(), Name: "eve", IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "1520"}, sidID(pacDomain + "-1301")}}
		k.user(t, eve, 0)
		k.applyDirectory(t, directory.SchemaAD, krbRealm, nil)
		k.signInAs(t, "backup's linked SID", k.pacAs("backup", rid("backup", 1300)), backup.Principal)
		k.logs.Reset()
		res, oc, err := k.srv.SignInOutcome(ctx, k.pacAs("backup", rid("backup", 1301)))
		refused(t, "the reused name's SID", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
		logs := k.logs.String()
		for _, want := range []string{"a PAC's user SID is not the one its record is bound to", backup.Principal.ID.String(), eve.Principal.ID.String()} {
			if !strings.Contains(logs, want) {
				t.Errorf("logs %s, want %q", logs, want)
			}
		}
		for _, never := range []string{"backup@", "S-1-5-21-7-7-7"} {
			if strings.Contains(logs, never) {
				t.Errorf("logs %s carry %q", logs, never)
			}
		}
	})
	t.Run("an unlinked row", func(t *testing.T) {
		k := kerberosServer(t)
		backup := entity.User{Principal: entity.NewUser(), Name: "backup", IDs: append(fileIDs("1510"), krbID("backup@TEST.REALM"))}
		k.user(t, backup, 0)
		k.applyDirectory(t, directory.SchemaAD, krbRealm, nil)
		res, oc, err := k.srv.SignInOutcome(ctx, k.pacAs("backup", rid("backup", 1300)))
		refused(t, "a PAC on an unlinked row", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
	})
	t.Run("step 2's match", func(t *testing.T) {
		k := kerberosServer(t)
		k.user(t, entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}, 0)
		res, oc, err := k.srv.SignInOutcome(ctx, k.pacAs("alice", rid("alice", 1105)))
		refused(t, "a PAC whose SID is not alice's record's", res, oc, err, vfserr.ErrLogonFailure, "logon_failure")
	})
}
