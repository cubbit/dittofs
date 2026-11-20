package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// directory is identity's directory as the API asks it before a write. A
// Kerberos name's link is the directory user SID it holds under the name's
// user part, from a fresh answer only: vfserr.ErrUnavailable when it cannot
// answer or could serve only a stale one, vfserr.ErrUnknownPrincipal when it
// holds no such entry, and the zero SID for a name in another realm, which
// links for none.
type directory interface {
	KerberosLinkSID(ctx context.Context, name string) (sid.SID, error)
}

// joins holds an online join's administrator credential in memory under the
// operation it is handed to, until the join's step takes it or release is
// called; release zeroes the password. AppliedKeytab is the number of the
// keytab's current version identity has applied and the name of each key
// its versions in force hold, zero and none while it holds no keytab.
type joins interface {
	HoldJoinCredential(id entity.OperationID, cred identity.JoinCredential) (release func())
	AppliedKeytab() (uint64, []identity.KeytabName, error)
}

// opStart is an installation action's pre-write step's result: the
// operation's ID, picked before the write so the step can hand it to
// identity, zero for one the write picks; its params, as its kind decodes
// them; the release of what the step holds under the ID, which the write
// adapter calls unless the write commits, nil for none; and check, run in
// the operation's own transaction on the installation read there, which
// refuses the start, nil for none.
type opStart struct {
	id      entity.OperationID
	params  []byte
	release func()
	check   func(tx entity.ControlPlaneTx, in entity.Installation) error
}

// The settings and secret owner the domain's routes read and write.
const (
	directoryBase   = "identity.directory.base"
	directoryRealm  = "identity.directory.realm"
	machinePassword = "machine_password"
)

// maxDirectoryName bounds a principal's name, a Kerberos name, a computer
// account's name and an organisational unit, in bytes, as the directory's own
// names are bounded.
const maxDirectoryName = 1024

// tooLong refuses field, longer than maxDirectoryName.
func tooLong(field string) error {
	return &fieldError{field: field, detail: "at most " + strconv.Itoa(maxDirectoryName) + " bytes"}
}

var (
	errNoDirectory = &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no directory to ask"}
	errNoIdentity  = &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no identity service to drive the operation"}
	errNoSealer    = &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no secret provider to seal the value with"}
)

// installationRef is the installation as an operation's target, an audit
// record's and a secret's owner name it.
func installationRef(in entity.Installation) entity.ResourceRef {
	return entity.ResourceRef{Kind: entity.ResourceInstallation, ID: in.ID[:]}
}

// installationID is the ID of the installation, the one record its paths
// reach.
func installationID(ctx context.Context, d Deps) ([16]byte, error) {
	in, err := d.Store.Installation(ctx)
	return in.ID, err
}

// action adapts an action on the installation whose row starts an operation:
// prep checks the request and names the operation's ID, params, hold and
// check; the write starts it, on the installation, once check admits it in
// the same transaction. Its audit records name the installation by its ID.
func action[B any](prep func(context.Context, request[B]) (opStart, error)) adapter {
	w := writing[B]{kind: entity.ResourceInstallation, self: installationID,
		read: func(entity.ControlPlaneTx, [16]byte, string) (B, uint64, bool, error) {
			var b B
			return b, 0, false, nil
		}}
	return write(w, prep, func(_ context.Context, rq request[B], st opStart, tx entity.ControlPlaneTx) (none, [16]byte, error) {
		in, err := tx.Installation()
		if err == nil && st.check != nil {
			err = st.check(tx, in)
		}
		return none{}, rq.ID, err
	})
}

// noneRunning refuses, 409 operation_running naming it, while an operation of
// any of kinds runs on the installation, read tracked in tx.
func noneRunning(tx entity.ControlPlaneTx, in entity.Installation, kinds ...apiv1.OpKind) error {
	for _, k := range kinds {
		op, ok, err := tx.RunningOperation(string(k), installationRef(in))
		if err != nil {
			return err
		}
		if ok {
			return &entity.OperationRunningError{ID: op.ID}
		}
	}
	return nil
}

// installationRead is GET /installation: its ID and its state.
func installationRead(ctx context.Context, d Deps, _ request[none]) (apiv1.Installation, error) {
	if d.Store == nil {
		return apiv1.Installation{}, errNoStore
	}
	in, err := d.Store.Installation(ctx)
	if err != nil {
		return apiv1.Installation{}, err
	}
	j := apiv1.Join{Joined: in.Join.Joined, Account: in.Join.Account, Names: in.Join.Names}
	if r := in.Join.LastRotation; !r.At.IsZero() {
		j.LastRotation = &apiv1.Rotation{At: r.At, Failed: r.Failed, Version: r.Version, Confirmed: r.Confirmed}
	}
	off := apiv1.NTLMOff{Off: in.NTLMOff.Off}
	if in.NTLMOff.Deleted != (entity.OperationID{}) {
		off.Deleted = idString(in.NTLMOff.Deleted)
	}
	return apiv1.Installation{ID: idString(in.ID), Status: apiv1.InstallationStatus{Join: j, NTLMOff: off}}, nil
}

// prepareResolve is resolve-principal's pre-write step: a name or a SID,
// exactly one, recorded as the operation's params, refused 503 while the
// node holds no identity service to drive it.
func prepareResolve(_ context.Context, rq request[apiv1.ResolvePrincipalParams]) (opStart, error) {
	p := rq.Body
	switch {
	case (p.Name == "") == (p.SID == ""):
		return opStart{}, &fieldError{field: "name", detail: "a principal is named by its name or by its SID, exactly one"}
	case len(p.Name) > maxDirectoryName:
		return opStart{}, tooLong("name")
	case p.SID != "":
		if _, err := sid.ParseSIDString(p.SID); err != nil {
			return opStart{}, &fieldError{field: "sid", detail: "not a SID in its string form"}
		}
	}
	if rq.d.Joins == nil {
		return opStart{}, errNoIdentity
	}
	b, err := json.Marshal(p)
	return opStart{params: b}, err
}

// identityDrives is test-directory's pre-write step, which hands identity
// nothing: refused 503 while the node holds no identity service to drive
// the operation.
//
// decision: resolve-principal, test-directory and rotate-machine-password
// are gated on Joins, identity's join hold, since it is the one interface
// the API holds of the service whose runner steps drive them; none of them
// hands identity anything before its write. A node handing in Joins without
// identity's steps would start operations nobody drives. Overturned when the
// API is handed identity whole, and gates on that.
func identityDrives(_ context.Context, rq request[none]) (opStart, error) {
	if rq.d.Joins == nil {
		return opStart{}, errNoIdentity
	}
	return opStart{}, nil
}

// prepareRotate is rotate-machine-password's pre-write step: refused 503
// while the node holds no identity service, and, at its start, 409 while a
// join runs, so a join and a rotation never run at once. A version handed
// to an offline join that no join took is superseded by the rotation, which
// seals a new version and never uses the handed-over one.
func prepareRotate(ctx context.Context, rq request[none]) (opStart, error) {
	st, err := identityDrives(ctx, rq)
	st.check = func(tx entity.ControlPlaneTx, in entity.Installation) error {
		return noneRunning(tx, in, apiv1.OpJoinDomain)
	}
	return st, err
}

// prepareJoin is join-domain's pre-write step. Either mode is refused 503
// while the node holds no identity service to drive the join, and, at its
// start, 409 while a rotation runs. An online join's organisational unit
// outside the directory record's base, or host name that is not a DNS name,
// is refused 400 before anything is held or written, each left empty for
// identity's default; then the operation's ID is picked and the
// administrator credential held under it, so the join's first step finds it,
// and the adapter releases it unless the write creating the operation
// commits. An offline join's computer account password is sealed and written
// as a new version of the machine password, in a write of its own committed
// before the operation's, whose version the operation's params name, and
// its start is refused 409 unless that version is still the current one.
// Neither credential reaches the params, and a refusal before the hold or
// the seal clears the password.
func prepareJoin(ctx context.Context, rq request[apiv1.JoinDomain]) (opStart, error) {
	p, cred := apiv1.JoinDomainParams{JoinDomainSpec: rq.Body.Spec}, rq.Body.Credential
	owned := false // the hold or handOver clears it from there
	defer func() {
		if !owned {
			clear(cred.Password)
		}
	}()
	switch {
	case len(p.Account) > maxDirectoryName:
		return opStart{}, tooLong("spec.account")
	case len(cred.Password) == 0:
		return opStart{}, &fieldError{field: "credential.password", detail: "a join takes a password"}
	}
	switch p.Mode {
	case apiv1.JoinOnline:
		switch {
		case cred.User == "":
			return opStart{}, &fieldError{field: "credential.user", detail: "an online join takes the administrator's name"}
		case len(p.OU) > maxDirectoryName:
			return opStart{}, tooLong("spec.ou")
		case p.DNSHostName != "" && (!hostName(p.DNSHostName) || !strings.Contains(p.DNSHostName, ".")):
			return opStart{}, &fieldError{field: "spec.dns_host_name", detail: "not a DNS host name of two labels or more"}
		}
		base, err := underBase(ctx, rq.d, p.OU)
		if err != nil {
			return opStart{}, err
		}
		if rq.d.Joins == nil {
			return opStart{}, errNoIdentity
		}
		b, err := json.Marshal(p)
		if err != nil {
			return opStart{}, err
		}
		keytab, err := keytabOf(rq.d, p.Account)
		if err != nil {
			return opStart{}, err
		}
		id := entity.OperationID(entity.NewShareID())
		owned = true
		check := func(tx entity.ControlPlaneTx, in entity.Installation) error {
			if err := notRotating(tx, in); err != nil {
				return err
			}
			if err := keytab(tx); err != nil || p.OU == "" {
				return err
			}
			s, ok, err := tx.Setting(installation, directoryBase)
			if err == nil && (!ok || !bytes.Equal(s.Value, base)) {
				err = &refusal{code: apiv1.CodeUnavailable, detail: "the directory's base changed while the join was checked; retry"}
			}
			return err
		}
		return opStart{id: id, params: b, check: check,
			release: rq.d.Joins.HoldJoinCredential(id, identity.JoinCredential{User: cred.User, Password: cred.Password})}, nil
	case apiv1.JoinOffline:
		switch {
		case cred.User != "":
			return opStart{}, &fieldError{field: "credential.user", detail: "an offline join takes the computer account's password alone"}
		case p.Account == "":
			return opStart{}, &fieldError{field: "spec.account", detail: "an offline join names the account an administrator created"}
		case p.OU != "" || p.DNSHostName != "" || p.Takeover:
			return opStart{}, &fieldError{field: "spec", detail: "an offline join creates no account, so it takes no ou, dns_host_name or takeover"}
		case rq.d.Joins == nil:
			return opStart{}, errNoIdentity
		case rq.d.Sealer == nil:
			return opStart{}, errNoSealer
		}
		keytab, err := keytabOf(rq.d, p.Account)
		if err != nil {
			return opStart{}, err
		}
		owned = true
		ref, err := handOver(ctx, rq, cred.Password)
		if err != nil {
			return opStart{}, err
		}
		p.Version = ref.Version
		b, err := json.Marshal(p)
		return opStart{params: b, check: func(tx entity.ControlPlaneTx, in entity.Installation) error {
			if err := notRotating(tx, in); err != nil {
				return err
			}
			if err := keytab(tx); err != nil {
				return err
			}
			cur, ok, err := tx.Secret(in.MachinePassword, 0)
			if err != nil {
				return err
			}
			// decision: a version superseded since its hand-over is refused
			// 409 operation_running naming no operation: another join's
			// hand-over superseded it, and that join may not have started
			// yet, so there is no operation to name; the refusal keeps
			// this join off a password no longer current. Overturned if
			// the refusal must name the superseding join, which needs its
			// operation's ID recorded with the version.
			if !ok || in.MachinePassword != ref.ID || cur.Version != p.Version {
				return &refusal{code: apiv1.CodeOperationRunning, detail: "another join wrote the machine password since this one did"}
			}
			return nil
		}}, err
	}
	return opStart{}, &fieldError{field: "spec.mode", detail: "a join is online or offline"}
}

// keytabOf is a join's check of the stored keytab against account, the
// computer account the join names: it refuses the start, 409 naming
// spec.account, while the keytab holds a key for account, in the realm
// identity.directory.realm records, or in every realm when none is
// recorded, as a keytab's put refuses one for a joined account's name. The
// keytab's names are those of the version identity has applied, asked
// before the write; the write reads the keytab's setting and current
// version tracked, and refuses 503 when that is not the version identity
// applied, so a keytab put since is not missed. An account of "" checks
// nothing.
//
// decision: a join is checked against the account it names only. An online
// join naming none takes identity's default name, which the API does not
// know, and the service names a rotation refreshes, and a later write of
// identity.directory.realm, are checked against no stored keytab, since the
// API cannot open a sealed keytab inside those writes. A key such a gap
// lets through for one of the joined account's names is dropped from
// identity's ring and logged at Apply, so no sign-in is verified with it. Overturned when identity checks the
// stored keytab against the account's names in those writes itself.
func keytabOf(d Deps, account string) (func(entity.ControlPlaneTx) error, error) {
	if account == "" {
		return func(entity.ControlPlaneTx) error { return nil }, nil
	}
	ver, names, err := d.Joins.AppliedKeytab()
	if err != nil {
		return nil, err
	}
	return func(tx entity.ControlPlaneTx) error {
		s, ok, err := tx.Setting(installation, keytabKey)
		if err != nil || !ok {
			return err
		}
		ref, err := refFrom(s.Value)
		if err != nil {
			return err
		}
		cur, ok, err := tx.Secret(ref, 0)
		if err != nil {
			return err
		}
		if !ok || cur.Version != ver {
			return &refusal{code: apiv1.CodeUnavailable, detail: "the keytab changed while the join was checked; retry"}
		}
		realm, rok, err := tx.Setting(installation, directoryRealm)
		if err != nil {
			return err
		}
		n, held, err := accountKey(entity.MachineJoin{Joined: true, Account: account}, realm, rok, names)
		if err != nil || !held {
			return err
		}
		return &componentError{field: "spec.account", detail: "the keytab holds a key for " + n.Name + "@" + n.Realm + ", the account this join derives keys for", err: vfserr.ErrExist}
	}, nil
}

// notRotating refuses a join's start while a rotation runs.
func notRotating(tx entity.ControlPlaneTx, in entity.Installation) error {
	return noneRunning(tx, in, apiv1.OpRotateMachinePassword)
}

// handOver seals password as the machine password's next version, marked
// handed over, clears the password, and writes the version in a write of its
// own, audited as a set of the reference, naming a new reference on the
// installation when it names none yet. That write is refused 409
// operation_running while a join or a rotation runs, read tracked in it, so
// no version a running one did not take becomes current under it. It
// returns the version written, unsealed fields only.
func handOver(ctx context.Context, rq request[apiv1.JoinDomain], password []byte) (entity.Secret, error) {
	defer clear(password)
	in, err := rq.d.Store.Installation(ctx)
	if err != nil {
		return entity.Secret{}, err
	}
	ref := in.MachinePassword
	if ref == (entity.SecretRef{}) {
		ref = entity.SecretRef(entity.NewShareID())
	}
	owner := entity.SecretOwner{Record: installationRef(in), Key: machinePassword}
	sealed, err := rq.d.Sealer.Seal(ctx, entity.SecretMachinePassword, owner, ref, password, true)
	if err != nil {
		return entity.Secret{}, err
	}
	err = rq.own(ctx, installationRef(in), []byte(`{"`+machinePassword+`":"set"}`), func(tx entity.ControlPlaneTx) error {
		cur, err := tx.Installation()
		if err != nil {
			return err
		}
		if err := noneRunning(tx, cur, apiv1.OpJoinDomain, apiv1.OpRotateMachinePassword); err != nil {
			return err
		}
		switch cur.MachinePassword {
		case ref:
		case entity.SecretRef{}:
			if err := tx.SetMachinePassword(ref); err != nil {
				return err
			}
		default:
			return &refusal{code: apiv1.CodeOperationRunning, detail: "another join named the machine password since this one read it"}
		}
		return tx.PutSecret(sealed, sealed.Version-1)
	})
	return entity.Secret{ID: sealed.ID, Version: sealed.Version}, err
}

// underBase refuses an organisational unit that is not a distinguished name
// strictly under the directory record's base, compared RDN by RDN (rdns),
// and one given while the record names no base; an empty one is identity's
// default and passes. It returns the base setting's value as read, nil for
// an empty unit, which the join's start reads again.
func underBase(ctx context.Context, d Deps, ou string) ([]byte, error) {
	if ou == "" {
		return nil, nil
	}
	refuse := func(detail string) error { return &fieldError{field: "spec.ou", detail: detail} }
	o, ok := rdns(ou)
	if !ok {
		return nil, refuse("not a distinguished name")
	}
	all, err := scopeSettings(ctx, d.Store, installation)
	if err != nil {
		return nil, err
	}
	var base string
	s, ok := all[directoryBase]
	if ok {
		if err := json.Unmarshal(s.Value, &base); err != nil {
			return nil, err
		}
	}
	b, ok := rdns(base)
	if !ok {
		return nil, refuse("the installation's directory record names no base to place it under")
	}
	if len(o) <= len(b) || !slices.EqualFunc(o[len(o)-len(b):], b, slices.Equal) {
		return nil, refuse("not under the directory's base " + base)
	}
	return s.Value, nil
}

// rdns splits a distinguished name into its RDNs, each the sorted list of
// its attribute type=value pairs, lower-cased, with an escape decoded (a
// backslash before two hex digits naming a byte, or before any other
// character standing for it) and the spaces around the type and the value
// removed, so two spellings of one name compare equal; false for a name with
// no RDN, a pair lacking its type, its = or its value, or a trailing escape.
func rdns(dn string) ([][]string, bool) {
	var out [][]string
	var rdn []string
	var cur strings.Builder
	typ, inValue := "", false
	pair := func() bool {
		v := strings.TrimSpace(cur.String())
		cur.Reset()
		if !inValue || typ == "" || v == "" {
			return false
		}
		rdn = append(rdn, strings.ToLower(typ+"="+v))
		typ, inValue = "", false
		return true
	}
	for i := 0; i < len(dn); i++ {
		switch c := dn[i]; {
		case c == '\\':
			if i+1 == len(dn) {
				return nil, false
			}
			if b, err := hex.DecodeString(dn[i+1 : min(i+3, len(dn))]); err == nil && len(b) == 1 {
				cur.WriteByte(b[0])
				i += 2
			} else {
				cur.WriteByte(dn[i+1])
				i++
			}
		case c == '=' && !inValue:
			typ, inValue = strings.TrimSpace(cur.String()), true
			cur.Reset()
		case c == '+' || c == ',':
			if !pair() {
				return nil, false
			}
			if c == ',' {
				slices.Sort(rdn)
				out, rdn = append(out, rdn), nil
			}
		default:
			cur.WriteByte(c)
		}
	}
	if !pair() {
		return nil, false
	}
	slices.Sort(rdn)
	return append(out, rdn), true
}
