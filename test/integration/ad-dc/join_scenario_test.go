//go:build ad_dc

package ad_dc_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/internal/node/step"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// joinNode is an installation driving identity's join and rotation steps
// against the fixture's controller, as the operation runner would: a Badger
// store in memory, the real secret provider, and the record naming the
// controller's address explicitly.
type joinNode struct {
	st     *kvstore.Store
	p      *secrets.Provider
	rec    *directory.Record
	in     entity.Installation
	s      *identity.Server
	logons *logonSink
}

func newJoinNode(t *testing.T, dcIP string, ca []byte) *joinNode {
	t.Helper()
	st, p, ref := sealedStore(t)
	n := &joinNode{st: st, p: p, rec: adRecord(dcIP, ca, ref)}
	var err error
	if n.in, err = st.Installation(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.restart(t)
	return n
}

// restart builds a fresh server over the same store, as a node restarts,
// and applies the store's State.
func (n *joinNode) restart(t *testing.T) {
	t.Helper()
	if n.s != nil {
		_ = n.s.Stop(context.Background())
	}
	n.logons = &logonSink{}
	s, err := identity.New(identity.Deps{Store: n.st, Clock: wallClock{}, Secrets: n.p, Logons: n.logons,
		Directory: directory.DialTLS, DomainController: netlogon.NewDialer((&net.Dialer{}).DialContext),
		ControlPlane: n.st.ControlPlane()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	n.s = s
	n.reconcile(t)
}

func (n *joinNode) owner() entity.SecretOwner {
	return entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: n.in.ID[:]}, Key: "machine_password"}
}

// versions are the machine password's versions in the store, current first.
func (n *joinNode) versions(t *testing.T) []secrets.Version {
	t.Helper()
	in := n.installation(t)
	if in.MachinePassword == (entity.SecretRef{}) {
		return nil
	}
	vs, err := n.p.Versions(context.Background(), secrets.Protocol, entity.SecretMachinePassword, n.owner(), in.MachinePassword)
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

func (n *joinNode) installation(t *testing.T) entity.Installation {
	t.Helper()
	in, err := n.st.Installation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// reconcile applies the store's State, as the node's reconcile does,
// identity.netbios_name NAS until a join writes it: the default, DITTOFS,
// is the fixture's domain.
func (n *joinNode) reconcile(t *testing.T) {
	t.Helper()
	st := identity.State{Settings: identity.Settings{IDRange: entity.DefaultIDRange, NTLM: true, NetBIOSName: "NAS"}, Directory: n.rec, Machine: n.versions(t), Join: n.installation(t).Join}
	for s, err := range n.st.ControlPlane().Settings(context.Background(), entity.SettingScope{Kind: entity.ScopeInstallation}) {
		if err != nil {
			t.Fatal(err)
		}
		if s.Key == "identity.netbios_name" {
			if err := json.Unmarshal(s.Value, &st.Settings.NetBIOSName); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := n.s.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// handOver seals password as the machine password's current version marked
// HandedOver, as the API's hand-over does, and answers its number.
func (n *joinNode) handOver(t *testing.T, password string) uint64 {
	t.Helper()
	ctx := context.Background()
	ref := entity.SecretRef(uuid.New())
	sec, err := n.p.Seal(ctx, secrets.Protocol, entity.SecretMachinePassword, n.owner(), ref, []byte(password), secrets.Meta{HandedOver: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.st.ControlPlane().Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PutSecret(sec, sec.Version-1); err != nil {
			return err
		}
		return tx.SetMachinePassword(ref)
	}); err != nil {
		t.Fatal(err)
	}
	return sec.Version
}

// op is a new operation of kind on the installation with params.
func (n *joinNode) op(t *testing.T, kind apiv1.OpKind, params any) *entity.Operation {
	t.Helper()
	op := &entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(kind),
		Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: n.in.ID[:]}, State: entity.OpRunning}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		op.Params = b
	}
	return op
}

// run runs op's turns to its end as the runner does, the store's State
// applied before each, a done turn's End written, and answers the failure
// that ended it, if one did. A turn left at its phase, its controller not
// answering, is run again after a short pause; a retried error fails the
// test.
func (n *joinNode) run(t *testing.T, op *entity.Operation) error {
	t.Helper()
	ctx := context.Background()
	turn := n.s.RotateStep
	if op.Kind == string(apiv1.OpJoinDomain) {
		turn = n.s.JoinStep
	}
	for range 60 {
		n.reconcile(t)
		res, err := step.Run(ctx, turn, *op)
		if _, fails := step.Failure(err); fails {
			op.State = entity.OpFailed
			n.reconcile(t)
			return err
		}
		if err != nil {
			t.Fatalf("%s turn at %s: retried error %v", op.Kind, op.Step, err)
		}
		if res.Done {
			if res.End != nil {
				if err := n.st.ControlPlane().Write(ctx, entity.WriteMeta{}, res.End); err != nil {
					t.Fatalf("the %s's End: %v", op.Kind, err)
				}
			}
			op.State, op.Result, op.Step = entity.OpSucceeded, res.Result, res.Next
			n.reconcile(t)
			return nil
		}
		if string(res.Next) == string(op.Step) {
			// Polls the controller's own answer; the loop bounds the wait.
			time.Sleep(250 * time.Millisecond)
		}
		op.Step = res.Next
	}
	t.Fatalf("%s not done after 60 turns; at %s", op.Kind, op.Step)
	return nil
}

// complete runs op to its end and wants it done.
func (n *joinNode) complete(t *testing.T, op *entity.Operation) {
	t.Helper()
	if err := n.run(t, op); err != nil {
		t.Fatalf("%s failed: %v", op.Kind, err)
	}
}

// ticket signs alice's ticket for spn, fetched now by a fresh client, in
// through SignIn, and wants it verified.
func (n *joinNode) ticket(t *testing.T, krb5Conf, spn string) {
	t.Helper()
	cl := adLogin(t, krb5Conf, adUserAlice, adUserPass)
	tkt, key := adTicket(t, cl, spn)
	b, _ := adAPReq(t, cl, tkt, key)
	_, err := n.s.SignIn(context.Background(), pid.SignInStep{Kerberos: b, Service: pid.ServiceCIFS, Transport: pid.TransportSMB,
		Client: netip.MustParseAddr("192.0.2.51")})
	if err != nil || n.logons.last() != "ok" {
		t.Errorf("alice's ticket for %s under kvno %d: %v, outcome %q; want ok", spn, tkt.EncPart.KVNO, err, n.logons.last())
	}
}

// passThrough signs alice in by pass-through, with an AUTHENTICATE computed
// from her password against the computer name the node advertises, and
// wants it answered.
func (n *joinNode) passThrough(t *testing.T) {
	t.Helper()
	computer, err := n.s.ComputerName()
	if err != nil {
		t.Fatalf("ComputerName: %v", err)
	}
	var challenge [8]byte
	_, _ = rand.Read(challenge[:])
	nt, _ := ntlmResponseFor(context.Background(), t, computer, adUserAlice, adUserPass, challenge)
	_, err = n.s.SignIn(context.Background(), pid.SignInStep{Challenge: challenge, Client: netip.MustParseAddr("192.0.2.52"),
		NTLM: &pid.NTLMAuthenticate{User: adUserAlice, Domain: adDomain, Workstation: "DCTWS", NTResponse: nt}})
	if err != nil || n.logons.last() != "ok" {
		t.Errorf("alice's pass-through naming %s: %v, outcome %q; want ok", computer, err, n.logons.last())
	}
}

// adKeyVersion is the computer account name$'s msDS-KeyVersionNumber, read
// with samba-tool.
func adKeyVersion(t *testing.T, name string) uint32 {
	t.Helper()
	v := adAttrs(t, "computer", name, "msDS-KeyVersionNumber")["msDS-KeyVersionNumber"]
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		t.Fatalf("%s$'s msDS-KeyVersionNumber %q: %v", name, v, err)
	}
	return uint32(n)
}

// adValues is every value of the computer name$'s attribute, read with
// samba-tool.
func adValues(t *testing.T, name, attr string) []string {
	t.Helper()
	out, err := adDockerExec("samba-tool", "computer", "show", name, "--attributes="+attr)
	if err != nil {
		t.Fatalf("samba-tool computer show %s: %v\n%s", name, err, out)
	}
	var vs []string
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, attr+": "); ok {
			vs = append(vs, strings.TrimSpace(v))
		}
	}
	return vs
}

// Both joins and a rotation against Samba, driving identity's steps as the
// runner would (RFC 18 §3.3, §3.5). An offline join of offjoinaes$, its
// password handed over, rotates twice and reads the verified number plus two;
// alice then signs in with a Samba ticket under the derived key and by
// pass-through. A rotation records Samba's number, and a fresh server
// verifies a ticket issued after it. On another installation, online joins
// of DITTOFS and BUILD01 are refused with their passwords untouched, and one
// of ONJOIN creates the account Samba reads back, tagged with Samba's number,
// its ticket for the DNS host name verified under the key salted by the
// account's name.
func TestJoinScenario(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AD-DC integration test in short mode")
	}
	dcIP := setupADDC(t)
	_, ca := dcRecord(t)
	seed := &sambaDCSeeder{conn: adminLDAP(t, dcIP, ca), ip: dcIP}
	seed.Reset(t)
	krb5Conf := writeADKrb5Conf(t, dcIP)
	const (
		offAccount  = "offjoinaes"
		offPassword = "OffJoinAES01!"
	)
	waitForMachinePassword(t, dcIP, offAccount, offPassword)
	// The fixture stages offjoinaes$ with no service name, and identity
	// derives keys for the names the account holds alone: give it the host/
	// name a member server carries, which sPNMappings extends to cifs/.
	if out, err := adDockerExec("samba-tool", "spn", "add", "HOST/"+offAccount+".dittofs.ad", offAccount+"$"); err != nil {
		t.Fatalf("samba-tool spn add for %s$: %v\n%s", offAccount, err, out)
	}

	off := newJoinNode(t, dcIP, ca)
	t.Run("offline join", func(t *testing.T) {
		verified := adKeyVersion(t, offAccount)
		v := off.handOver(t, offPassword)
		off.complete(t, off.op(t, apiv1.OpJoinDomain, apiv1.JoinDomainParams{
			JoinDomainSpec: apiv1.JoinDomainSpec{Mode: apiv1.JoinOffline, Account: offAccount + "$"}, Version: v}))
		j := off.installation(t).Join
		if !j.Joined || j.Account != "OFFJOINAES$" {
			t.Fatalf("join %+v, want joined as OFFJOINAES$", j)
		}
		if got := adKeyVersion(t, offAccount); j.LastRotation.Read != verified+2 || got != verified+2 {
			t.Errorf("the second confirm read %d, Samba holds %d; want %d, the verified %d plus two",
				j.LastRotation.Read, got, verified+2, verified)
		}
		off.ticket(t, krb5Conf, "cifs/"+offAccount+".dittofs.ad")
		off.passThrough(t)
	})

	t.Run("rotation", func(t *testing.T) {
		before := off.installation(t).Join.LastRotation
		off.complete(t, off.op(t, apiv1.OpRotateMachinePassword, nil))
		lr := off.installation(t).Join.LastRotation
		got := adKeyVersion(t, offAccount)
		if lr.Failed || lr.Version <= before.Version || lr.Read != got || lr.Tag != got {
			t.Errorf("last rotation %+v, Samba's number %d; want a later version, tagged and read as Samba's number", lr, got)
		}
		off.restart(t)
		off.ticket(t, krb5Conf, "cifs/"+offAccount+".dittofs.ad")
	})

	t.Run("online join", func(t *testing.T) {
		on := newJoinNode(t, dcIP, ca)
		online := func(account string) *entity.Operation {
			op := on.op(t, apiv1.OpJoinDomain, apiv1.JoinDomainParams{JoinDomainSpec: apiv1.JoinDomainSpec{
				Mode: apiv1.JoinOnline, Account: account, DNSHostName: "fileserver.dittofs.ad"}})
			on.s.HoldJoinCredential(op.ID, seed.Admin())
			return op
		}

		// DITTOFS is the domain's own name, and BUILD01$ exists: each is
		// refused, naming it, and its password still sets up a channel.
		for _, tc := range []struct{ account, password, refusal string }{
			{"DITTOFS", machineAccountPass, "is named as the domain"},
			{nodeAccount, nodePassword, "BUILD01$ already exists"},
		} {
			err := on.run(t, online(tc.account))
			p, ok := errors.AsType[*apiv1.Problem](err)
			if !ok || !strings.Contains(p.Detail, tc.refusal) {
				t.Errorf("online join of %s: %v; want refused, %q", tc.account, err, tc.refusal)
			}
			if err := channelOnce(t.Context(), netlogon.NewDialer((&net.Dialer{}).DialContext), dcIP, tc.account, tc.password); err != nil {
				t.Errorf("%s$'s password after the refused join: %v; want it unchanged", tc.account, err)
			}
		}
		if j := on.installation(t).Join; j.Joined || len(on.versions(t)) != 0 {
			t.Fatalf("join %+v with %d versions after the refusals, want nothing written", j, len(on.versions(t)))
		}

		on.complete(t, online("onjoin"))
		j := on.installation(t).Join
		if !j.Joined || j.Account != "ONJOIN$" {
			t.Fatalf("join %+v, want joined as ONJOIN$", j)
		}
		if got := adKeyVersion(t, "onjoin"); j.LastRotation.Tag != got {
			t.Errorf("the join's tag %d, Samba's msDS-KeyVersionNumber %d; want them equal", j.LastRotation.Tag, got)
		}
		spns := adValues(t, "onjoin", "servicePrincipalName")
		for _, want := range []string{"cifs/ONJOIN", "cifs/fileserver.dittofs.ad", "host/ONJOIN", "host/fileserver.dittofs.ad"} {
			if !slices.ContainsFunc(spns, func(s string) bool { return strings.EqualFold(s, want) }) {
				t.Errorf("onjoin$'s servicePrincipalName %v, want %s among them", spns, want)
			}
		}
		attrs := adAttrs(t, "computer", "onjoin", "dNSHostName", "msDS-SupportedEncryptionTypes")
		if attrs["dNSHostName"] != "fileserver.dittofs.ad" || attrs["msDS-SupportedEncryptionTypes"] != "24" {
			t.Errorf("onjoin$'s dNSHostName %q, msDS-SupportedEncryptionTypes %q; want fileserver.dittofs.ad, 24",
				attrs["dNSHostName"], attrs["msDS-SupportedEncryptionTypes"])
		}
		// The derived key's salt is DITTOFS.ADhostonjoin.dittofs.ad, from the
		// account's name, not its DNS host name, as Samba's is.
		on.ticket(t, krb5Conf, "cifs/fileserver.dittofs.ad")
	})
}
