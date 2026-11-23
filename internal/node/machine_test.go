package node_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/iana/etypeID"

	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
)

const (
	machineRealm = "CORP.EXAMPLE"
	machineDC    = "dc1.corp.example"
	machineSvc   = "cifs/nas.corp.example"
	// machineSalt is the computer account NAS$'s salt in CORP.EXAMPLE,
	// written out rather than built by the code under test.
	machineSalt = "CORP.EXAMPLEhostnas.corp.example"
	alicePW     = "Alice-Passw0rd"
	svcPW       = "Svc-Passw0rd"
)

// dcFaults fails each domain controller call whose operation it holds an
// error for, as identitytest's fault wrapper names them.
type dcFaults struct {
	mu sync.Mutex
	m  map[string]error
}

func (f *dcFaults) set(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[op] = err
}

func (f *dcFaults) At(op string, _ []byte) (time.Duration, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return 0, 0, f.m[op]
}

// machine is a node whose installation has a directory record for the
// domain CORP, realm CORP.EXAMPLE, whose one controller and directory are
// in memory and hold alice under one SID; the controller is reached through
// faults.
type machine struct {
	h      *nodetest.Harness
	p      *secrets.Provider
	dc     *identitytest.DomainController
	faults *dcFaults
	kv     *kvtest.FaultKV // the node's store, as its last start wrapped it
	keys   int             // the Idempotency-Keys sent
}

// newMachine builds the node over a FaultKV, in memory unless onDisk,
// writes its directory record, calls each of before on its node, and
// starts it.
func newMachine(t *testing.T, onDisk bool, before ...func(*node.Node)) *machine {
	t.Helper()
	return newMachineLogged(t, nil, onDisk, before...)
}

// newMachineLogged is newMachine whose node also logs every line, debug
// included, as text to log, when it is set.
func newMachineLogged(t *testing.T, log io.Writer, onDisk bool, before ...func(*node.Node)) *machine {
	t.Helper()
	dir := identitytest.NewDirectory("DC=corp,DC=example")
	domain, _, err := sid.DecodeSID(dir.DomainSID())
	if err != nil {
		t.Fatal(err)
	}
	dc := identitytest.NewDomainController("CORP", machineRealm, "DC=corp,DC=example", machineDC)
	dc.UseDomainSID(*domain)
	// Each user is added to both under the SID the controller gives it, so
	// no RID the directory hands out on its own collides with one of them.
	user := func(name, password string) string {
		s := dc.AddUser(t, name, []byte(password))
		var b bytes.Buffer
		sid.EncodeSID(&b, &s)
		return dir.AddUser(t, name, map[string][][]byte{"objectSid": {b.Bytes()}})
	}
	user("placeholder", "unused") // takes the RID the directory's own Administrator holds
	user("alice", alicePW)
	svc := user("svc", svcPW)
	dir.SetPassword(t, svc, []byte(svcPW))

	m := &machine{dc: dc, faults: &dcFaults{m: map[string]error{}}}
	signing := func(context.Context) ([]byte, error) { return []byte("installation"), nil }
	build := node.NewWithDialers(dir.Dial, identitytest.NewFaultyDomainController(dc, m.faults), admin, signing)
	opts := []nodetest.Option{nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		m.kv = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = m.kv
		if log != nil {
			cfg.Logger = slog.New(slog.NewMultiHandler(cfg.Logger.Handler(), slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})))
		}
		return build(cfg)
	})}
	if !onDisk {
		opts = append(opts, nodetest.InMemory())
	}
	m.h = nodetest.New(t, opts...)
	m.p = sealer(t, m.h)
	ref := entity.SecretRef(uuid.New())
	seal(t, m.h, m.p, secrets.Protocol, entity.SecretDirectoryCredential, entity.SecretOwner{Scope: installationScope, Key: "identity.directory"}, ref, []byte(svcPW))
	for k, v := range map[string]string{
		"address": strconv.Quote("ldaps://" + machineDC), "base": `"DC=corp,DC=example"`, "bind_name": strconv.Quote(svc),
		"realm": strconv.Quote(machineRealm), "netbios_domain": `"CORP"`, "dc_address": strconv.Quote(machineDC),
		"require_pac": "false", "bind_credential": refJSON(ref),
	} {
		putSetting(t, m.h, "identity.directory."+k, v)
	}
	for _, f := range before {
		f(m.h.Node())
	}
	started(t, m.h)
	return m
}

// post sends POST /api/v1/installation:verb with body and, but for a
// join-domain, which carries a credential, a fresh Idempotency-Key,
// wanting 202, and returns the operation's ID.
func (m *machine) post(t *testing.T, verb, body string) entity.OperationID {
	t.Helper()
	var hdr []string
	if verb != "join-domain" {
		m.keys++
		hdr = []string{"Idempotency-Key", "k" + strconv.Itoa(m.keys)}
	}
	addr, _ := m.h.Node().Addrs()
	r := answeredIn(t, verb, send(t.Context(), addr, http.MethodPost, "/api/v1/installation:"+verb, body, hdr...))
	if r.status != http.StatusAccepted {
		t.Fatalf("POST %s: answered %d %s, want 202", verb, r.status, r.body)
	}
	var op apiv1.Operation
	if err := json.Unmarshal(r.body, &op); err != nil {
		t.Fatalf("POST %s: decode %q: %v", verb, r.body, err)
	}
	id, err := uuid.Parse(op.ID)
	if err != nil {
		t.Fatalf("POST %s: operation ID %q: %v", verb, op.ID, err)
	}
	return entity.OperationID(id)
}

// ended turns the node until operation id has ended, and returns it.
func (m *machine) ended(t *testing.T, id entity.OperationID) entity.Operation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		op := operation(t, m.h, id)
		if op.State == entity.OpSucceeded || op.State == entity.OpFailed {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s within 30 s: %v at step %s, want it ended", op.Kind, op.State, op.Step)
		}
		turn(m.h)
	}
}

// succeeded wants operation id to end succeeded.
func (m *machine) succeeded(t *testing.T, what string, id entity.OperationID) {
	t.Helper()
	if op := m.ended(t, id); op.State != entity.OpSucceeded {
		t.Fatalf("%s: operation ended %v with %s, want succeeded", what, op.State, op.Error)
	}
}

// join joins the installation online as NAS$ through the API, waits for
// the join to succeed, and returns its operation's ID.
func (m *machine) join(t *testing.T) entity.OperationID {
	t.Helper()
	adm := m.dc.Admin()
	body, err := json.Marshal(apiv1.JoinDomain{
		Spec:       apiv1.JoinDomainSpec{Mode: apiv1.JoinOnline, Account: "NAS", DNSHostName: "nas.corp.example"},
		Credential: apiv1.JoinCredential{User: adm.User, Password: adm.Password},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := m.post(t, "join-domain", string(body))
	m.succeeded(t, "the online join", id)
	caughtUp(t, m.h)
	return id
}

// passThrough signs alice in as CORP\alice through the node's filesystem
// service, which passes it to the domain controller.
func (m *machine) passThrough(t *testing.T) (identity.SignInResult, error) {
	t.Helper()
	nt, _, _ := identitytest.NTLMv2Response([]byte(alicePW), "alice", "CORP", ntlmChallenge, [8]byte{0xaa})
	return m.h.Node().Service().SignIn(t.Context(), identity.SignInStep{
		NTLM:      &identity.NTLMAuthenticate{User: "alice", Domain: "CORP", NTResponse: nt},
		Challenge: ntlmChallenge,
		Client:    signInClient,
	})
}

// passedThrough wants alice's pass-through sign-in to bind a principal.
func (m *machine) passedThrough(t *testing.T, what string) {
	t.Helper()
	if res, err := m.passThrough(t); err != nil || res.Identity.Evidence.Principal.IsZero() {
		t.Fatalf("%s: CORP\\alice passed through: bound %v, %v; want a principal, nil", what, res.Identity.Evidence.Principal, err)
	}
}

// installation reads the installation record.
func (m *machine) installation(t *testing.T) entity.Installation {
	t.Helper()
	in, err := m.h.Store().Installation(t.Context())
	if err != nil {
		t.Fatalf("read the installation: %v", err)
	}
	return in
}

// password is the machine password's current version, read raw.
func (m *machine) password(t *testing.T) secrets.Version {
	t.Helper()
	in := m.installation(t)
	owner := entity.SecretOwner{Record: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: in.ID[:]}, Key: "machine_password"}
	vs, err := m.p.Versions(t.Context(), secrets.Protocol, entity.SecretMachinePassword, owner, in.MachinePassword)
	if err != nil || len(vs) == 0 {
		t.Fatalf("the machine password's versions: %d, %v; want one at least", len(vs), err)
	}
	return vs[0]
}

// ticket signs a machine client in with a ticket for machineSvc under the
// AES256 key derived from v, as the domain controller issues one to the
// computer account.
func (m *machine) ticket(t *testing.T, v secrets.Version) (identity.SignInResult, error) {
	t.Helper()
	et, err := crypto.GetEtype(etypeID.AES256_CTS_HMAC_SHA1_96)
	if err != nil {
		t.Fatal(err)
	}
	key, err := et.StringToKey(string(v.Value), machineSalt, et.GetDefaultStringToKeyParams())
	if err != nil {
		t.Fatal(err)
	}
	tk := identitytest.NewRealm(machineRealm).Ticket(identitytest.TicketOpts{Client: "host/client.corp.example", Service: machineSvc,
		Key: key, KVNO: int(v.Meta.KeyVersion), Now: m.h.Clock().Now()})
	return m.h.Node().Service().SignIn(t.Context(), identity.SignInStep{Kerberos: tk, Service: identity.ServiceCIFS,
		Transport: identity.TransportSMB, Client: signInClient})
}

// An online join through the API runs on the node's runner to joined, and
// alice's NTLM sign-in in the domain is then passed through to the domain
// controller. Catches the join-domain kind with no step, which leaves the
// join pending, and State.Machine or State.Join never filled, which leaves
// pass-through unavailable.
func TestAJoinThroughTheAPIRunsOnTheNode(t *testing.T) {
	m := newMachine(t, false)
	m.join(t)
	if j := m.installation(t).Join; !j.Joined || j.Account != "NAS$" {
		t.Fatalf("the installation's join after the join: %+v, want joined as NAS$", j)
	}
	m.passedThrough(t, "once joined")
}

// Once joined, a ticket under a key derived from the machine password signs
// in, and after a restart with no domain controller reachable it still
// does: the node hands identity the join and the password from the store.
// Catches State.Join left zero, which derives no key.
func TestAJoinedNodeSignsInUnderTheMachineKeyAcrossARestart(t *testing.T) {
	m := newMachine(t, true)
	m.join(t)
	v := m.password(t)
	if _, err := m.ticket(t, v); err != nil {
		t.Fatalf("a ticket under the machine password's key once joined: %v, want signed in", err)
	}
	m.faults.set("lookup", errors.New("no controller answers"))
	m.faults.set("dial", errors.New("no controller answers"))
	m.h.Restart(t)
	started(t, m.h)
	if _, err := m.ticket(t, v); err != nil {
		t.Fatalf("a ticket under the machine password's key after a restart with no controller: %v, want signed in", err)
	}
}

// secretReported reports whether s reports version v of ref.
func secretReported(s entity.NodeStatus, ref entity.SecretRef, v uint64) bool {
	return slices.Contains(s.Secrets, entity.SecretVersion{Secret: ref, Version: v})
}

// A rotation through the API runs on the node's runner: node status then
// reports the new version of the machine password, and alice still passes
// through. Catches the machine password's versions never reported, which
// keeps the old version from ever being deleted.
func TestARotationThroughTheAPIReportsTheNewVersion(t *testing.T) {
	m := newMachine(t, false)
	m.join(t)
	before := m.password(t).Number
	m.succeeded(t, "the rotation", m.post(t, "rotate-machine-password", ""))
	after := m.password(t).Number
	if after <= before {
		t.Fatalf("the machine password's version after the rotation: %d, want above %d", after, before)
	}
	ref := m.installation(t).MachinePassword
	statusUntil(t, m.h, fmt.Sprintf("the machine password's version %d reported", after),
		func(s entity.NodeStatus) bool { return secretReported(s, ref, after) })
	m.passedThrough(t, "after the rotation")
}

// A resolve-principal of a name the directory lacks fails its operation
// with the step's problem at the runner's first turns over it. Catches a
// problem the runner takes for a plain error, which it retries for ever.
func TestAResolveOfAnUnknownNameFailsAtTheFirstTurn(t *testing.T) {
	turned := make(chan struct{}, 1)
	m := newMachine(t, false, func(n *node.Node) {
		n.OnRunnerTurn(func() {
			select {
			case turned <- struct{}{}:
			default:
			}
		})
	})
	caughtUp(t, m.h)
	id := m.post(t, "resolve-principal", `{"name":"CORP\\nobody"}`)
	// A turn running while the operation was written may not have listed
	// it, so two turns are waited for: a retried error would leave the
	// operation running after both.
	for range 2 {
		select {
		case <-turned:
		default:
		}
		m.h.Clock().Advance(time.Second)
		select {
		case <-turned:
		case <-time.After(10 * time.Second):
			t.Fatal("no runner turn within 10 s of the clock passing the runner's interval")
		}
	}
	op := operation(t, m.h, id)
	var p apiv1.Problem
	if op.State != entity.OpFailed || json.Unmarshal(op.Error, &p) != nil || p.Status != http.StatusNotFound {
		t.Fatalf("resolve-principal of CORP\\nobody after a turn: %v with error %s, want failed with the step's 404 problem", op.State, op.Error)
	}
}

// hasCondition reports whether s holds condition name of scope, naming id
// unless it is empty.
func hasCondition(t *testing.T, s entity.NodeStatus, name, scope, id string) bool {
	t.Helper()
	return slices.ContainsFunc(conditionsOf(t, s), func(c storedCondition) bool {
		return c.Name == name && c.Scope == scope && (id == "" || c.ID == id)
	})
}

// The domain controller failing raises machine_account_unhealthy naming
// this node; a rotation the controller refuses raises
// machine_password_rotation_failed for the installation, kept across a
// restart and gone once a rotation completes. Catches a condition identity
// raises that the node never reads, and a join record the node never
// re-reads.
func TestTheMachineAccountConditionsReachNodeStatus(t *testing.T) {
	m := newMachine(t, true)
	m.join(t)
	node := fmt.Sprintf("%x", m.h.NodeID())
	m.faults.set("channel", errors.New("no controller answers"))
	if _, err := m.passThrough(t); err == nil {
		t.Fatal("CORP\\alice passed through with every channel failing: signed in, want an error")
	}
	statusUntil(t, m.h, "machine_account_unhealthy naming this node", func(s entity.NodeStatus) bool {
		return hasCondition(t, s, "machine_account_unhealthy", "node", node)
	})
	m.faults.set("channel", nil)
	m.passedThrough(t, "once the controller answers")

	m.faults.set("setpassword", netlogon.ErrRejected)
	if op := m.ended(t, m.post(t, "rotate-machine-password", "")); op.State != entity.OpFailed {
		t.Fatalf("a rotation the controller refuses: ended %v, want failed", op.State)
	}
	failed := func(s entity.NodeStatus) bool {
		return hasCondition(t, s, "machine_password_rotation_failed", "installation", "")
	}
	statusUntil(t, m.h, "machine_password_rotation_failed", failed)
	m.h.Restart(t)
	started(t, m.h)
	statusUntil(t, m.h, "machine_password_rotation_failed after the restart", failed)
	m.faults.set("setpassword", nil)
	m.succeeded(t, "a rotation the controller accepts", m.post(t, "rotate-machine-password", ""))
	statusUntil(t, m.h, "no machine_password_rotation_failed once a rotation completed", func(s entity.NodeStatus) bool { return !failed(s) })
}

// A rotation the runner fails outside the rotation's own failure path, its
// cursor not decoding, records the last rotation failed at the end of that
// turn, and the schedule starts the next an hour later rather than at its
// next minute. Catches a rotation ended without that record, which the
// schedule restarts every minute.
func TestARotationTheRunnerFailsBacksTheScheduleOff(t *testing.T) {
	m := newMachine(t, false)
	h := m.h
	in := m.installation(t)
	target := entity.ResourceRef{Kind: entity.ResourceInstallation, ID: in.ID[:]}
	err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		j := entity.MachineJoin{Joined: true, Account: "NAS$"}
		j.LastRotation.Confirmed = h.Clock().Now().Add(-8 * 24 * time.Hour) // a rotation is due
		return tx.SetMachineJoin(j)
	})
	if err != nil {
		t.Fatalf("record the installation joined: %v", err)
	}
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpRotateMachinePassword), Target: target,
		Account: entity.NewUser(), State: entity.OpRunning, Step: []byte("{")}
	if _, err := h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the rotation: %v", err)
	}
	from := h.Clock().Now()
	if got := m.ended(t, op.ID); got.State != entity.OpFailed {
		t.Fatalf("a rotation whose cursor does not decode: ended %v, want failed", got.State)
	}
	lr := m.installation(t).Join.LastRotation
	// Store time runs strictly increasing, so it may pass the clock by a
	// nanosecond a commit.
	if to := h.Clock().Now(); !lr.Failed || lr.At.Before(from) || lr.At.After(to.Add(time.Millisecond)) {
		t.Fatalf("the last rotation once the runner failed one: failed %v at %v, want failed between %v and %v, while the runner turned", lr.Failed, lr.At, from, to)
	}
	at := lr.At

	// The schedule checks once a minute on the node's clock, and a rotation
	// it starts fails at its first turn, as the installation names no
	// machine password, moving At. Store time follows a clock moved on only
	// once the store raises its ceiling, once a second of real time, so the
	// clock moves a minute at a time with real time between. No event marks
	// a check that starts nothing, so the wait at 59 minutes is a bounded
	// one, and it is the assertion.
	moved := func(within time.Duration) (time.Time, bool) {
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if got := m.installation(t).Join.LastRotation.At; !got.Equal(at) {
				return got, true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return time.Time{}, false
	}
	h.Clock().Advance(59 * time.Minute)
	if got, ok := moved(3 * time.Second); ok {
		t.Fatalf("the last rotation's time 59 min after a failure: %v, want %v: the schedule started a rotation within the hour", got, at)
	}
	for range 10 {
		h.Clock().Advance(time.Minute)
		if got, ok := moved(3 * time.Second); ok {
			if d := got.Sub(at); d < time.Hour || d > time.Hour+5*time.Minute {
				t.Fatalf("the next rotation's failure %v after the last, want an hour after it", d)
			}
			return
		}
	}
	t.Fatal("no rotation started within 10 min of the hour passing")
}

// installationWrite is the installation record's write, as a FaultKV rule
// names it.
var installationWrite = keys.Describe([]byte{keys.KindIN})

// errWriteFailed is an installation write the store refuses as a failure of
// the operation writing it, which the runner does not retry.
var errWriteFailed = fmt.Errorf("%w: the installation's write refused", step.ErrStepFailed)

// A rotation whose End is refused still records the last rotation failed:
// the runner's second commit writes the failure with the kind's failed
// write, so the schedule backs off from it. The rotation writes the
// installation twice, the sealed version's reference and then its End, so
// the second write is refused. Catches a fallback that records the failure
// bare after an End, which leaves the schedule restarting the rotation
// every minute.
func TestARotationWhoseEndIsRefusedIsRecordedFailed(t *testing.T) {
	m := newMachine(t, false)
	m.join(t)
	before := m.installation(t).Join.LastRotation
	setFault(t, m.kv, nil, sim.Rule{Op: kvtest.OpSet, Key: installationWrite, From: 2, Count: 1, Action: sim.FaultAction{Err: errWriteFailed}})
	op := m.ended(t, m.post(t, "rotate-machine-password", ""))
	if op.State != entity.OpFailed || !bytes.Contains(op.Error, []byte("the installation's write refused")) {
		t.Fatalf("a rotation whose End is refused: ended %v with %s, want failed naming the refused write", op.State, op.Error)
	}
	if lr := m.installation(t).Join.LastRotation; before.Failed || !lr.Failed || lr.Version != before.Version {
		t.Fatalf("the last rotation once its End was refused: %+v (before the rotation %+v), want failed at the version before", lr, before)
	}
}

// A rotation whose seal write is refused, and whose failed write then
// fails too, ends failed all the same: the runner's last commit records the
// failure bare, whether that write was refused or failed with a plain store
// error. The seal write is always refused, since a plain error there is
// retried and never reaches the failed write. Catches a commit that reruns
// the failed write or retries the turn, which leaves the rotation running,
// holding the running index against every join and rotation, and its step
// rerun at every turn.
func TestARotationWhoseFailedWriteIsRefusedEndsFailed(t *testing.T) {
	for name, fault := range map[string]error{
		"refused":       errWriteFailed,
		"a plain error": errors.New("the installation's write failed"),
	} {
		t.Run(name, func(t *testing.T) {
			m := newMachine(t, false)
			m.join(t)
			before := m.installation(t).Join.LastRotation
			setFault(t, m.kv, nil,
				sim.Rule{Op: kvtest.OpSet, Key: installationWrite, Count: 1, Action: sim.FaultAction{Err: errWriteFailed}},
				sim.Rule{Op: kvtest.OpSet, Key: installationWrite, From: 2, Action: sim.FaultAction{Err: fault}})
			op := m.ended(t, m.post(t, "rotate-machine-password", ""))
			if op.State != entity.OpFailed {
				t.Fatalf("a rotation whose failed write is %s: ended %v with %s, want failed", name, op.State, op.Error)
			}
			if lr := m.installation(t).Join.LastRotation; lr != before {
				t.Fatalf("the last rotation once its failed write was %s: %+v, want unchanged %+v", name, lr, before)
			}
		})
	}
}

// A machine password that does not read at a settings change keeps the
// versions and the join identity last applied. Catches a fallback that
// applies no machine account, which stops every pass-through until the
// record reads again.
func TestAMachinePasswordThatDoesNotReadKeepsTheLast(t *testing.T) {
	m := newMachine(t, false)
	m.join(t)
	setFault(t, m.kv, nil, sim.Rule{Op: kvtest.OpGet, Key: installationWrite, Action: sim.FaultAction{Err: errors.New("the installation does not read")}})
	gen := m.h.Node().Positions().Settings
	putSetting(t, m.h, "identity.ntlm", "true")
	for deadline := time.Now().Add(10 * time.Second); m.h.Node().Positions().Settings <= gen; turn(m.h) {
		if time.Now().After(deadline) {
			t.Fatalf("the settings position within 10 s: %d, want past %d", m.h.Node().Positions().Settings, gen)
		}
	}
	if st := m.h.Node().AppliedIdentity(); len(st.Machine) == 0 || !st.Join.Joined {
		t.Fatalf("identity once the machine password did not read: %d versions, joined %v; want the last kept", len(st.Machine), st.Join.Joined)
	}
}
