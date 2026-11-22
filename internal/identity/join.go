package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// holds is the administrator credentials held for running joins, by the
// operation each was taken for. The credential lives here alone: no method
// writes it to a record, a log or another table.
type holds struct {
	mu sync.Mutex
	m  map[entity.OperationID]*identity.JoinCredential
}

// HoldJoinCredential holds cred in this node's memory under the operation id
// until release is called or the server stops. release drops the hold and
// zeroes cred.Password, the caller's slice, which the hold keeps rather than
// copies; it is safe to call more than once. A second hold under the same id
// replaces the first and zeroes its password, unless both share one slice, so
// a forgotten release cannot leave it in memory; the first's release then
// leaves the second alone.
func (s *Server) HoldJoinCredential(id entity.OperationID, cred identity.JoinCredential) (release func()) {
	c := &cred
	s.holds.mu.Lock()
	if s.holds.m == nil {
		s.holds.m = make(map[entity.OperationID]*identity.JoinCredential)
	}
	if old := s.holds.m[id]; old != nil && !samePassword(old.Password, c.Password) {
		clear(old.Password)
	}
	s.holds.m[id] = c
	s.holds.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.holds.mu.Lock()
			cur := s.holds.m[id]
			if cur == c {
				delete(s.holds.m, id)
			}
			// A live hold over the same buffer keeps it: zeroing it here
			// would clear the password that hold still answers.
			shared := cur != nil && cur != c && samePassword(cur.Password, c.Password)
			s.holds.mu.Unlock()
			if !shared {
				clear(c.Password)
			}
		})
	}
}

// samePassword reports whether a and b are the same slice, so replacing a
// hold with one over the caller's same buffer does not zero the new hold.
func samePassword(a, b []byte) bool {
	return len(a) > 0 && len(b) > 0 && &a[0] == &b[0]
}

// heldCredential is the credential held under id. The caller uses it for one
// call and keeps no copy: its Password is zeroed in place at release.
func (s *Server) heldCredential(id entity.OperationID) (identity.JoinCredential, bool) {
	s.holds.mu.Lock()
	defer s.holds.mu.Unlock()
	c, ok := s.holds.m[id]
	if !ok {
		return identity.JoinCredential{}, false
	}
	return *c, true
}

// dropHolds zeroes and drops every hold, so none outlives the server.
func (s *Server) dropHolds() {
	s.holds.mu.Lock()
	defer s.holds.mu.Unlock()
	for id, c := range s.holds.m {
		clear(c.Password)
		delete(s.holds.m, id)
	}
}

// releaseHold drops and zeroes the credential held under id, as the join
// that took it ends.
func (s *Server) releaseHold(id entity.OperationID) {
	s.holds.mu.Lock()
	defer s.holds.mu.Unlock()
	if c, ok := s.holds.m[id]; ok {
		clear(c.Password)
		delete(s.holds.m, id)
	}
}

// The join's phases (RFC 18 §3.5). An online join runs joinPhases; an
// offline one runs offlinePhases, its two closing rotations each a seal, a
// set and a confirm.
const (
	joinStart   = ""        // check the request, refuse a running rotation
	joinPin     = "pin"     // pin a controller
	joinSeal    = "seal"    // read the account, seal the new version
	joinCreate  = "create"  // probe, record the attempt, create or reset
	joinConfirm = "confirm" // probe, read the account, record the join

	joinVerify   = "verify"   // read the account, verify the handed-over password
	joinSeal1    = "seal1"    // the first closing rotation's seal
	joinSet1     = "set1"     // probe, record the attempt, set
	joinConfirm1 = "confirm1" // probe, read the account
	joinSeal2    = "seal2"    // the second closing rotation's seal
	joinSet2     = "set2"     // probe, record the attempt, set
	joinConfirm2 = "confirm2" // probe, read the account, record the join
)

var (
	joinPhases    = []string{joinStart, joinPin, joinSeal, joinCreate, joinConfirm}
	offlinePhases = []string{joinStart, joinPin, joinVerify, joinSeal1, joinSet1, joinConfirm1, joinSeal2, joinSet2, joinConfirm2}
)

// phasesOf is the phases a join of mode runs; nil for a mode not built.
func phasesOf(mode apiv1.JoinMode) []string {
	switch mode {
	case apiv1.JoinOnline:
		return joinPhases
	case apiv1.JoinOffline:
		return offlinePhases
	}
	return nil
}

// joinCursor is the join's durable cursor, recorded as the operation's Step
// after each turn.
type joinCursor struct {
	Phase       string    `json:"phase"`
	Account     string    `json:"account,omitempty"` // without its $, upper case
	OU          string    `json:"ou,omitempty"`      // empty for the domain's Computers container
	DNSHostName string    `json:"dns_host_name,omitempty"`
	DC          string    `json:"dc,omitempty"` // the pinned controller
	Exists      bool      `json:"exists,omitempty"`
	Tag         uint32    `json:"tag,omitempty"`
	Version     uint64    `json:"version,omitempty"`
	Attempted   bool      `json:"attempted,omitempty"`
	Unanswered  time.Time `json:"unanswered,omitzero"`
	Number      uint32    `json:"number,omitempty"` // offline: the key version number read at the verify
	First       uint64    `json:"first,omitempty"`  // offline: the first closing rotation's version
}

// joinResult is a completed join's Result.
type joinResult struct {
	Account    string   `json:"account"`
	DC         string   `json:"dc"`
	Version    uint64   `json:"version"`
	Tag        uint32   `json:"tag"`
	Read       uint32   `json:"read"`
	Names      []string `json:"names,omitempty"`
	Owner      string   `json:"owner,omitempty"`
	Delegation []byte   `json:"delegation,omitempty"`
}

// JoinProgress is the join's progress, read from its cursor.
func (s *Server) JoinProgress(_ context.Context, op entity.Operation) apiv1.Progress {
	var c joinCursor
	var p apiv1.JoinDomainParams
	_ = json.Unmarshal(op.Step, &c) // no cursor yet reads as the first phase
	_ = json.Unmarshal(op.Params, &p)
	phases := phasesOf(p.Mode)
	return apiv1.Progress{Phase: c.Phase, Done: int64(slices.Index(phases, c.Phase)), Total: int64(len(phases)), Unit: "steps"}
}

// JoinStep runs one turn of a join-domain operation on the installation op
// targets, from the cursor op.Step holds. A failure that ends the join is a
// *apiv1.Problem naming the controller, the account, the version, the number
// read or the missing credential; every other error is retried with the same
// cursor. The administrator credential held under op.ID is released when the
// join ends.
//
// An offline join takes the version op.Params names, which the API stored
// marked HandedOver, verifies it at the pinned controller and rotates the
// password twice as its own last steps, joined only at the second confirm.
func (s *Server) JoinStep(ctx context.Context, op entity.Operation) (res step.StepResult, err error) {
	if op.Target.Kind != entity.ResourceInstallation {
		return step.StepResult{}, fmt.Errorf("%w: %s's target %s names no installation", step.ErrStepFailed, op.Kind, op.Target.Kind)
	}
	defer func() {
		if _, ended := step.Failure(err); ended || res.Done {
			s.releaseHold(op.ID)
		}
	}()
	var p apiv1.JoinDomainParams
	if err := json.Unmarshal(op.Params, &p); err != nil {
		return step.StepResult{}, fmt.Errorf("%w: the join's params: %w", step.ErrStepFailed, err)
	}
	phases := phasesOf(p.Mode)
	if phases == nil {
		return step.StepResult{}, fmt.Errorf("%w: the %q join is not built", step.ErrStepFailed, p.Mode)
	}
	var c joinCursor
	if len(op.Step) > 0 {
		if err := json.Unmarshal(op.Step, &c); err != nil {
			return step.StepResult{}, fmt.Errorf("%w: the join's cursor: %w", step.ErrStepFailed, err)
		}
	}
	if !slices.Contains(phases, c.Phase) {
		return step.StepResult{}, fmt.Errorf("%w: the %s join's cursor names no phase %q", step.ErrStepFailed, p.Mode, c.Phase)
	}
	st := s.state.Load()
	if st == nil {
		return step.StepResult{}, errNotApplied
	}
	if s.deps.Secrets == nil || s.deps.DomainController == nil || s.deps.ControlPlane == nil {
		return step.StepResult{}, fmt.Errorf("%w: no secret provider, domain controller or control plane", step.ErrStepFailed)
	}
	in, err := s.deps.Store.Installation(ctx)
	if err != nil {
		return step.StepResult{}, err
	}
	j := &joining{s: s, st: st, in: in, c: c, op: op.ID}
	if p.Mode == apiv1.JoinOffline {
		j.offline, j.handed = true, p.Version
	}
	if c.Phase != joinStart && st.Directory == nil {
		return step.StepResult{}, j.fail(ctx, http.StatusConflict, "the installation has no directory record")
	}
	switch c.Phase {
	case joinStart:
		err = j.start(ctx, op.Target, p.JoinDomainSpec)
	case joinPin:
		err = j.pin(ctx)
	case joinSeal:
		err = j.seal(ctx, p.Takeover)
	case joinCreate:
		err = j.create(ctx)
	case joinConfirm:
		res, err = j.confirm(ctx, 0)
	case joinVerify:
		err = j.verify(ctx)
	case joinSeal1:
		err = j.closingSeal(ctx, 1)
	case joinSeal2:
		err = j.closingSeal(ctx, 2)
	case joinSet1:
		err = j.closingSet(ctx, 1)
	case joinSet2:
		err = j.closingSet(ctx, 2)
	case joinConfirm1:
		res, err = j.confirm(ctx, 1)
	case joinConfirm2:
		res, err = j.confirm(ctx, 2)
	}
	if errors.Is(err, errUnanswered) {
		err = nil
	} else if j.c.Phase != c.Phase {
		j.c.Unanswered = time.Time{} // a phase moved on was answered
	}
	if err != nil || res.Done {
		return res, err
	}
	res.Next, err = json.Marshal(j.c)
	return res, err
}

// joining is one turn of a join: the State and Installation it reads and the
// cursor it moves.
type joining struct {
	s  *Server
	st *State
	in entity.Installation
	c  joinCursor
	op entity.OperationID
	// offline is an offline join, handed the version its params name.
	offline bool
	handed  uint64
}

func joinProblem(status int, code apiv1.Code, detail string) *apiv1.Problem {
	return &apiv1.Problem{Status: status, Code: code, Title: "the domain join failed", Detail: detail}
}

// start refuses a join of a joined installation that is not the takeover, a
// request the directory record or the names make wrong, and a rotation
// running on the installation, read tracked in one write that
// writes nothing. It fixes the account's name and the DNS host name, each
// defaulted, in the cursor.
func (j *joining) start(ctx context.Context, target entity.ResourceRef, spec apiv1.JoinDomainSpec) error {
	rec := j.st.Directory
	switch {
	case j.in.Join.Joined && !spec.Takeover:
		return joinProblem(http.StatusConflict, apiv1.CodeJoined, fmt.Sprintf("the installation is joined as %s; a new join needs the takeover", j.in.Join.Account))
	case rec == nil:
		return joinProblem(http.StatusConflict, "", "the installation has no directory record")
	case !rec.IsAD():
		return joinProblem(http.StatusConflict, "", fmt.Sprintf("the directory record's schema %q has no domain controller to join", rec.Schema))
	case spec.OU != "" && !directory.StrictlyUnder(spec.OU, rec.Base):
		return joinProblem(http.StatusBadRequest, "", fmt.Sprintf("the organisational unit %q is not under the directory's base %q", spec.OU, rec.Base))
	}
	account, err := joinAccountName(spec.Account, j.st.Settings.NetBIOSName)
	if err != nil {
		return joinProblem(http.StatusBadRequest, "", err.Error())
	}
	if equalFoldASCII(account, rec.NetBIOSDomain) {
		return joinProblem(http.StatusBadRequest, "", fmt.Sprintf("the account %s$ is named as the domain %s", account, rec.NetBIOSDomain))
	}
	host := spec.DNSHostName
	if host == "" {
		host = strings.ToLower(account) + "." + strings.ToLower(rec.Realm)
	}
	if !dnsHostName(host) {
		return joinProblem(http.StatusBadRequest, "", fmt.Sprintf("the host name %q is not a DNS name of two labels or more", host))
	}
	// decision: this read is a guard behind the writes that start a join or
	// a rotation, not what keeps the two apart. The API's start of either
	// kind and the rotation schedule each read the other kind's running
	// index tracked in the transaction that puts their own operation's row,
	// so a join and a rotation are never both running, and a join's first
	// turn finds a rotation started before it. This write writes nothing,
	// and the rotation's first turn writes nothing the join reads, so two
	// first turns that overlapped would not conflict: none is tested to.
	// Overturned if an operation of either kind can start without such a
	// creating write; each first turn must then write a key the other reads
	// tracked, so the second to commit retries and refuses naming the first.
	err = j.s.deps.ControlPlane.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		op, ok, err := tx.RunningOperation(string(apiv1.OpRotateMachinePassword), target)
		if err != nil {
			return err
		}
		if ok {
			return joinProblem(http.StatusConflict, apiv1.CodeOperationRunning, fmt.Sprintf("the rotation %x is running on the installation", op.ID))
		}
		return j.handedOver(tx)
	})
	if err != nil {
		return err
	}
	j.c.Phase, j.c.Account, j.c.OU, j.c.DNSHostName = joinPin, account, spec.OU, host
	return nil
}

// joinAccountName is the computer account's name a join takes: name, else
// the installation's NetBIOS name, without its $ and in upper case, refused
// unless it is a valid NetBIOS name.
func joinAccountName(name, netbios string) (string, error) {
	n := strings.TrimSuffix(name, "$")
	if n == "" {
		n = netbios
	}
	b := []byte(n)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	n = string(b)
	if err := validNetBIOSName(n); err != nil {
		return "", fmt.Errorf("the account name: %w", err)
	}
	return n, nil
}

// dnsHostName reports whether s is a DNS host name of two labels or more,
// its last label not all digits.
func dnsHostName(s string) bool {
	labels := strings.Split(s, ".")
	if len(s) > 253 || len(labels) < 2 || strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' ||
			strings.Trim(l, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
			return false
		}
	}
	return true
}

// pin picks the controller every later turn uses: the record's address, or
// the first of the domain's that answers a dial.
func (j *joining) pin(ctx context.Context) error {
	ctx, done := j.s.bounded(ctx)
	defer done()
	rec := j.st.Directory
	addrs, err := j.s.controllers(ctx, rec)
	if err != nil {
		return j.noAnswer(ctx, err)
	}
	var errs []error
	for _, a := range addrs {
		dc, err := j.s.deps.DomainController.Dial(ctx, *rec, a)
		if err == nil {
			_ = dc.Close()
			j.c.DC, j.c.Phase = a, joinSeal
			if j.offline {
				j.c.Phase = joinVerify
			}
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", a, err))
	}
	return j.noAnswer(ctx, fmt.Errorf("no domain controller of %s answers: %w", rec.Realm, errors.Join(errs...)))
}

// seal reads the account at the pinned controller, refusing one that exists
// unless the join takes it over, and seals a new version of the machine
// password beside Installation.MachinePassword in one write, before any
// controller sees it.
//
// decision: a created account's version is tagged 1, the number a
// controller gives an account added with its password, which the create
// does; a taken-over account's is the pinned controller's number plus one,
// as a rotation's. A tag is never changed, so a controller that numbers the
// create otherwise is a mismatch the confirm reports and counts. Overturned
// if the create stops carrying the password in the add.
func (j *joining) seal(ctx context.Context, takeover bool) error {
	acct, err := j.account(ctx)
	switch {
	case errors.Is(err, netlogon.ErrNoAccount):
		j.c.Exists, j.c.Tag = false, 1
	case err != nil:
		return err
	case !takeover:
		return j.fail(ctx, http.StatusConflict, fmt.Sprintf("the computer account %s$ already exists at %s; join with a takeover to reset it", j.c.Account, j.c.DC))
	default:
		j.c.Exists, j.c.Tag = true, acct.KeyVersion+1
	}
	v, err := j.sealNew(ctx, j.c.Tag)
	if err != nil {
		return err
	}
	j.c.Version, j.c.Phase = v, joinCreate
	return nil
}

// sealNew seals a new version of the machine password tagged tag beside
// Installation.MachinePassword in one write, before any controller sees it,
// and answers its number.
func (j *joining) sealNew(ctx context.Context, tag uint32) (uint64, error) {
	ref := j.in.MachinePassword
	if ref == (entity.SecretRef{}) {
		ref = entity.SecretRef(entity.NewShareID())
	}
	pw := newMachinePassword()
	defer clear(pw)
	sec, err := j.s.deps.Secrets.Seal(ctx, secrets.Protocol, entity.SecretMachinePassword, machineOwner(j.in), ref, pw,
		secrets.Meta{KeyVersion: tag})
	if err != nil {
		return 0, err
	}
	// decision: identity writes the machine account's records itself, the
	// sealed version beside Installation.MachinePassword in one write, since
	// the version must be durable before any controller sees its password.
	// Overturned if another component takes the machine account's writes.
	err = j.s.deps.ControlPlane.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		if err := tx.PutSecret(sec, sec.Version-1); err != nil {
			return err
		}
		if err := tx.SetMachinePassword(ref); err != nil {
			return err
		}
		return j.unjoin(tx)
	})
	if err != nil {
		return 0, err
	}
	return sec.Version, nil
}

// unjoin records an offline join's installation not joined in its closing
// seals' write, so a takeover's old join no longer holds once the first
// rotation's version exists: pass-through, the secure channel and the
// machine keys each read Join.Joined alone, and would otherwise sign in with
// a closing rotation's version before the second confirm, which RFC 18 §3.5
// forbids. As fail's rule has it, from the seal on a join recorded before no
// longer holds. An online join's seal leaves Join as it was.
func (j *joining) unjoin(tx entity.ControlPlaneTx) error {
	if !j.offline {
		return nil
	}
	in, err := tx.Installation()
	if err != nil || !in.Join.Joined {
		return err
	}
	jn := in.Join
	jn.Joined = false
	return tx.SetMachineJoin(jn)
}

// account reads the computer account at the pinned controller, bound with
// the directory record's bind credential: netlogon.ErrNoAccount when absent.
// A refused bind fails the join; any other error is noAnswer's.
func (j *joining) account(ctx context.Context) (netlogon.Account, error) {
	ctx, done := j.s.bounded(ctx)
	defer done()
	dc, err := j.s.deps.DomainController.Dial(ctx, *j.st.Directory, j.c.DC)
	if err != nil {
		return netlogon.Account{}, j.noAnswer(ctx, err)
	}
	defer func() { _ = dc.Close() }()
	return j.accountAt(ctx, dc)
}

func (j *joining) accountAt(ctx context.Context, dc netlogon.DC) (netlogon.Account, error) {
	rec := j.st.Directory
	pw, err := j.s.bindPassword(ctx, rec.BindCredential)
	if err != nil {
		return netlogon.Account{}, err
	}
	defer clear(pw)
	a, err := dc.Account(ctx, identity.JoinCredential{User: rec.BindName, Password: pw}, j.c.Account)
	switch {
	case err == nil, errors.Is(err, netlogon.ErrNoAccount):
		return a, err
	case errors.Is(err, netlogon.ErrRejected):
		return a, j.fail(ctx, http.StatusServiceUnavailable, fmt.Sprintf("domain controller %s refused the directory's bind to read %s$: %v", j.c.DC, j.c.Account, err))
	}
	return a, j.noAnswer(ctx, err)
}

// probe opens the pinned controller and asks whether it accepts the sealed
// version; the caller closes the controller.
func (j *joining) probe(ctx context.Context) (netlogon.DC, bool, error) {
	pw, err := j.sealed(ctx, j.c.Version)
	if err != nil {
		return nil, false, err
	}
	defer clear(pw)
	dc, err := j.s.deps.DomainController.Dial(ctx, *j.st.Directory, j.c.DC)
	if err != nil {
		return nil, false, j.noAnswer(ctx, err)
	}
	ok, err := probe(ctx, dc, j.c.Account, pw)
	if err != nil {
		_ = dc.Close()
		return nil, false, j.noAnswer(ctx, err)
	}
	return dc, ok, nil
}

// sealed is version n's password, a copy the caller clears.
func (j *joining) sealed(ctx context.Context, n uint64) ([]byte, error) {
	vs, err := j.s.deps.Secrets.Versions(ctx, secrets.Protocol, entity.SecretMachinePassword, machineOwner(j.in), j.in.MachinePassword)
	if err != nil {
		return nil, err
	}
	var pw []byte
	for _, v := range vs {
		if v.Number == n {
			pw = slices.Clone(v.Value)
		}
		clear(v.Value)
	}
	if pw == nil {
		return nil, j.fail(ctx, http.StatusServiceUnavailable, fmt.Sprintf("the machine password's version %d is no longer held", n))
	}
	return pw, nil
}

// create probes whether the pinned controller accepts the sealed version
// and, only if not, creates the account with it, or resets it on a
// takeover, as the held administrator. The first turn that finds it not
// accepted only records the attempt, so the attempt is durable before the
// controller sees the password. A join holding no credential fails, naming
// it.
func (j *joining) create(ctx context.Context) error {
	ctx, done := j.s.bounded(ctx)
	defer done()
	dc, accepted, err := j.probe(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()
	switch {
	case accepted:
		j.c.Phase = joinConfirm
		return nil
	case !j.c.Attempted:
		// The attempt is recorded in the cursor, as rotation.set's decision:
		// says.
		j.c.Attempted = true
		return nil
	}
	// The held administrator writes the password, so unlike a rotation's set
	// and the offline join's closingSet no machine password signs in here.
	admin, ok := j.s.heldCredential(j.op)
	if !ok {
		return j.fail(ctx, http.StatusConflict, fmt.Sprintf("no administrator credential is held for the join to create %s$; run the join again with a takeover", j.c.Account))
	}
	pw, err := j.sealed(ctx, j.c.Version)
	if err != nil {
		return err
	}
	defer clear(pw)
	if j.c.Exists {
		err = dc.Reset(ctx, admin, j.c.Account, pw)
	} else {
		err = dc.Create(ctx, admin, netlogon.Computer{Name: j.c.Account, OU: j.c.OU, DNSHostName: j.c.DNSHostName}, pw)
	}
	switch {
	case errors.Is(err, fs.ErrExist):
		return j.fail(ctx, http.StatusConflict, fmt.Sprintf("the computer account %s$ already exists at %s; join with a takeover to reset it", j.c.Account, j.c.DC))
	case errors.Is(err, netlogon.ErrRejected), errors.Is(err, netlogon.ErrNoAccount):
		return j.fail(ctx, http.StatusServiceUnavailable, fmt.Sprintf("domain controller %s refused to write %s$: %v", j.c.DC, j.c.Account, err))
	case err != nil:
		// The write may have reached the controller; the next turn's probe
		// settles it.
		return j.noAnswer(ctx, err)
	}
	j.c.Phase = joinConfirm
	return nil
}

// confirm fails the join if the account's name breaks identity.netbios_name's
// rule, which a settings write validates, then asks the pinned controller
// whether it accepts the sealed version, failing the join if not, and reads
// the account: a key version number other than the tag is logged and counted.
// round is the offline join's closing rotation, 0 for the online join's
// confirm. The first closing rotation's confirm moves on to the second's seal;
// the second's fails the join unless the number read is the one the verify
// read plus two. The join's last confirm ends it writing Join, joined, and
// the account's name as identity.netbios_name in one transaction.
func (j *joining) confirm(ctx context.Context, round int) (step.StepResult, error) {
	ctx, done := j.s.bounded(ctx)
	defer done()
	if err := validNetBIOSName(j.c.Account); err != nil {
		return step.StepResult{}, j.fail(ctx, http.StatusConflict, fmt.Sprintf("identity.netbios_name: %v", err))
	}
	dc, accepted, err := j.probe(ctx)
	if err != nil {
		return step.StepResult{}, err
	}
	defer func() { _ = dc.Close() }()
	if !accepted {
		return step.StepResult{}, j.fail(ctx, http.StatusServiceUnavailable, fmt.Sprintf("domain controller %s does not accept the machine password's version %d", j.c.DC, j.c.Version))
	}
	acct, err := j.accountAt(ctx, dc)
	if errors.Is(err, netlogon.ErrNoAccount) {
		err = j.noAnswer(ctx, err)
	}
	if err != nil {
		return step.StepResult{}, err
	}
	switch {
	case round == 2 && acct.KeyVersion != j.c.Number+2:
		// decision: the proof of no third set is the pinned controller's
		// number alone, since msDS-KeyVersionNumber is each controller's own:
		// a controller the two sets have not reached still accepts the
		// handed-over password until they replicate. Overturned if a
		// deployment needs the guarantee at every controller before the join
		// ends; the confirm then waits for each one's number.
		return step.StepResult{}, j.fail(ctx, http.StatusConflict, fmt.Sprintf("domain controller %s reads %s$'s key version number %d, not %d, the %d the join verified plus two",
			j.c.DC, j.c.Account, acct.KeyVersion, j.c.Number+2, j.c.Number))
	case acct.KeyVersion != j.c.Tag:
		j.s.logger.Warn("identity: the joined machine password's key version number differs from its tag",
			"dc", j.c.DC, "version", j.c.Version, "tag", j.c.Tag, "read", acct.KeyVersion)
		j.s.mismatches.WithLabelValues().Inc()
	}
	if round == 1 {
		j.c.First, j.c.Phase = j.c.Version, joinSeal2
		return step.StepResult{}, nil
	}
	c := j.c
	jr := joinResult{Account: c.Account + "$", DC: c.DC, Version: c.Version, Tag: c.Tag, Read: acct.KeyVersion,
		Names: acct.Names, Delegation: acct.Delegation}
	if acct.Owner.Revision != 0 {
		jr.Owner = sid.FormatSID(&acct.Owner)
	}
	result, err := json.Marshal(jr)
	if err != nil {
		return step.StepResult{}, err
	}
	next, err := json.Marshal(c)
	if err != nil {
		return step.StepResult{}, err
	}
	name, err := json.Marshal(c.Account)
	if err != nil {
		return step.StepResult{}, err
	}
	var owner bytes.Buffer
	sid.EncodeSID(&owner, &acct.Owner)
	return step.StepResult{Next: next, Done: true, Result: result, End: func(tx entity.ControlPlaneTx) error {
		now := tx.Now()
		jn := entity.MachineJoin{Joined: true, Account: c.Account + "$", Owner: owner.Bytes(),
			Delegation: acct.Delegation, Names: acct.Names}
		jn.LastRotation.At = now
		jn.LastRotation.Tag, jn.LastRotation.Read = c.Tag, acct.KeyVersion
		jn.LastRotation.Version, jn.LastRotation.Confirmed = c.Version, now
		if err := tx.SetMachineJoin(jn); err != nil {
			return err
		}
		scope := entity.SettingScope{Kind: entity.ScopeInstallation}
		old, _, err := tx.Setting(scope, KeyNetBIOSName)
		if err != nil {
			return err
		}
		_, err = tx.PutSetting(entity.Setting{Scope: scope, Key: KeyNetBIOSName, Value: name}, old.Generation)
		return err
	}}, nil
}

// noAnswer records when the pinned controller first went unanswered and
// fails the join, naming it, once it has gone unanswered for an hour; below
// the hour it answers errUnanswered, and the next turn asks again.
func (j *joining) noAnswer(ctx context.Context, err error) error {
	now := j.s.deps.Clock.Now()
	if j.c.Unanswered.IsZero() {
		j.c.Unanswered = now
		j.s.logger.Warn("identity: the join's domain controller did not answer", "dc", j.c.DC, "err", err)
	}
	if now.Sub(j.c.Unanswered) < unanswered {
		return errUnanswered
	}
	name := j.c.DC
	if name == "" {
		name = j.st.Directory.Realm
	}
	return j.fail(ctx, http.StatusServiceUnavailable, fmt.Sprintf("domain controller %s unanswered for %v: %v", name, unanswered, err))
}

// fail records the installation not joined and the join failed, leaving
// identity.netbios_name, the machine password's versions and the last
// confirm as they were, then answers the problem that fails the operation.
//
// decision: a failed join leaves the installation as it was; only a failure
// after the seal records it not joined. From the seal on, the machine
// password in force is one no controller may hold, so a join recorded before
// no longer holds. A failure before it (a refused request, an existing
// account, no controller answering) has changed nothing, so it writes
// nothing. A joined installation reaches a join only as its takeover, since
// the API and the join's first turn refuse any other. Every failure of the
// join's phases, the confirm's refusal of the account's name among them, ends
// here, so none rolls back without the record. The exception is corrupted
// state: a cursor that does not decode, or names no phase, fails the step
// without fail, and may do so after the seal, leaving the installation
// recorded as it was. Overturned if a failure before the seal can change
// what the installation holds.
func (j *joining) fail(ctx context.Context, status int, detail string) error {
	if j.c.Version == 0 {
		return joinProblem(status, "", detail)
	}
	// The turn's bound may have ended ctx; the record is still written.
	err := j.s.deps.ControlPlane.Write(context.WithoutCancel(ctx), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		in, err := tx.Installation()
		if err != nil {
			return err
		}
		jn := in.Join
		jn.Joined = false
		jn.LastRotation.At, jn.LastRotation.Failed = tx.Now(), true
		return tx.SetMachineJoin(jn)
	})
	if err != nil {
		return err
	}
	return joinProblem(status, "", detail)
}
