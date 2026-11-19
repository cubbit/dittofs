package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	KerberosLinkSID(ctx context.Context, name string) (identity.SID, error)
}

// joins holds an online join's administrator credential in memory under the
// operation it is handed to, until the join's step takes it or release is
// called; release zeroes the password.
type joins interface {
	HoldJoinCredential(id entity.OperationID, cred identity.JoinCredential) (release func())
}

// opStart is a pre-write step's result for a row that starts an operation:
// the operation's ID, picked before the write so the step can hand it to
// identity, zero for one the write picks; its params, as its kind decodes
// them; and the release of what the step holds under the ID, which the write
// adapter calls when the write is refused, nil for none.
type opStart struct {
	id      entity.OperationID
	params  []byte
	release func()
}

// The settings and secret owner the domain's routes read and write.
const (
	directoryBase   = "identity.directory.base"
	machinePassword = "machine_password"
)

// maxDirectoryName bounds a principal's name or Kerberos name, in bytes, as
// the directory's own user principal names are bounded.
const maxDirectoryName = 1024

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

// onInstallation is what the write adapter knows of an action on the
// installation: a record it compares no tag of, so it reads none.
func onInstallation[B any]() writing[B] {
	return writing[B]{kind: entity.ResourceInstallation, read: func(entity.ControlPlaneTx, [16]byte, string) (B, uint64, bool, error) {
		var b B
		return b, 0, false, nil
	}}
}

// startOn is an action on the installation that writes only the operation its
// row starts, on the installation read in tx.
func startOn[B any](_ context.Context, _ request[B], _ opStart, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	in, err := tx.Installation()
	return none{}, in.ID, err
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
		return opStart{}, &fieldError{field: "name", detail: "a name is at most 1024 bytes"}
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

// identityDrives is the pre-write step of test-directory and
// rotate-machine-password, which hand identity nothing: refused 503 while
// the node holds no identity service to drive the operation.
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

// prepareJoin is join-domain's pre-write step. An online join's
// organisational unit outside the directory record's base, or host name that
// is not a DNS name, is refused 400 before anything is held or written, each
// left empty for identity's default; then the operation's ID is picked and the
// administrator credential held under it, so the join's first step finds it,
// and the adapter releases it when the write creating the operation is
// refused. An offline join's computer account password is sealed and written
// as a new version of the machine password, in a write of its own committed
// before the operation's, whose version the operation's params name. Neither
// credential reaches the params.
func prepareJoin(ctx context.Context, rq request[apiv1.JoinDomain]) (opStart, error) {
	p, cred := rq.Body.Spec, rq.Body.Credential
	switch {
	case p.Version != 0:
		return opStart{}, &fieldError{field: "spec.version", detail: "the machine password's version is the server's to write"}
	case len(p.Account) > maxDirectoryName:
		return opStart{}, &fieldError{field: "spec.account", detail: "an account name is at most 1024 bytes"}
	case len(cred.Password) == 0:
		return opStart{}, &fieldError{field: "credential.password", detail: "a join takes a password"}
	}
	switch p.Mode {
	case apiv1.JoinOnline:
		if cred.User == "" {
			return opStart{}, &fieldError{field: "credential.user", detail: "an online join takes the administrator's name"}
		}
		if err := underBase(ctx, rq.d, p.OU); err != nil {
			return opStart{}, err
		}
		if p.DNSHostName != "" && !hostName(p.DNSHostName) {
			return opStart{}, &fieldError{field: "spec.dns_host_name", detail: "not a DNS host name"}
		}
		if rq.d.Joins == nil {
			return opStart{}, errNoIdentity
		}
		b, err := json.Marshal(p)
		if err != nil {
			return opStart{}, err
		}
		id := entity.OperationID(entity.NewShareID())
		return opStart{id: id, params: b, release: rq.d.Joins.HoldJoinCredential(id, identity.JoinCredential{User: cred.User, Password: cred.Password})}, nil
	case apiv1.JoinOffline:
		switch {
		case cred.User != "":
			return opStart{}, &fieldError{field: "credential.user", detail: "an offline join takes the computer account's password alone"}
		case p.Account == "":
			return opStart{}, &fieldError{field: "spec.account", detail: "an offline join names the account an administrator created"}
		case p.OU != "" || p.DNSHostName != "" || p.Takeover:
			return opStart{}, &fieldError{field: "spec", detail: "an offline join creates no account, so it takes no ou, dns_host_name or takeover"}
		}
		if rq.d.Sealer == nil {
			return opStart{}, errNoSealer
		}
		var err error
		if p.Version, err = handOver(ctx, rq, cred.Password); err != nil {
			return opStart{}, err
		}
		b, err := json.Marshal(p)
		return opStart{params: b}, err
	}
	return opStart{}, &fieldError{field: "spec.mode", detail: "a join is online or offline"}
}

// handOver seals password as the machine password's next version, marked
// handed over, and writes it in a write of its own, audited as a set of the
// reference, naming a new reference on the installation when it names none
// yet. That write is refused 409 operation_running while a join or a
// rotation runs, read tracked in it, so no version a running one did not
// take becomes current under it. It returns the version written.
func handOver(ctx context.Context, rq request[apiv1.JoinDomain], password []byte) (uint64, error) {
	in, err := rq.d.Store.Installation(ctx)
	if err != nil {
		return 0, err
	}
	ref := in.MachinePassword
	if ref == (entity.SecretRef{}) {
		ref = entity.SecretRef(entity.NewShareID())
	}
	owner := entity.SecretOwner{Record: installationRef(in), Key: machinePassword}
	sealed, err := rq.d.Sealer.Seal(ctx, entity.SecretMachinePassword, owner, ref, password, true)
	if err != nil {
		return 0, err
	}
	err = rq.own(ctx, installationRef(in), []byte(`{"`+machinePassword+`":"set"}`), func(tx entity.ControlPlaneTx) error {
		cur, err := tx.Installation()
		if err != nil {
			return err
		}
		for _, k := range []apiv1.OpKind{apiv1.OpJoinDomain, apiv1.OpRotateMachinePassword} {
			op, ok, err := tx.RunningOperation(string(k), installationRef(cur))
			if err != nil {
				return err
			}
			if ok {
				return &entity.OperationRunningError{ID: op.ID}
			}
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
	return sealed.Version, err
}

// underBase refuses an organisational unit that is not a distinguished name
// at or under the directory record's base, compared RDN by RDN, ignoring case
// and the spaces around each; an empty one is identity's default and passes.
func underBase(ctx context.Context, d Deps, ou string) error {
	if ou == "" {
		return nil
	}
	refuse := func(detail string) error { return &fieldError{field: "spec.ou", detail: detail} }
	o, ok := rdns(ou)
	if !ok {
		return refuse("not a distinguished name")
	}
	if d.Store == nil {
		return errNoStore
	}
	all, err := scopeSettings(ctx, d.Store, entity.SettingScope{Kind: entity.ScopeInstallation})
	if err != nil {
		return err
	}
	var base string
	if s, ok := all[directoryBase]; ok {
		if err := json.Unmarshal(s.Value, &base); err != nil {
			return err
		}
	}
	b, ok := rdns(base)
	if !ok {
		return refuse("the installation's directory record names no base to place it under")
	}
	if len(o) < len(b) {
		return refuse("not under the directory's base " + base)
	}
	for i, r := range b {
		if !strings.EqualFold(o[len(o)-len(b)+i], r) {
			return refuse("not under the directory's base " + base)
		}
	}
	return nil
}

// rdns splits a distinguished name into its RDNs, each type=value with the
// spaces around its parts removed; false for a name with no RDN, an RDN
// lacking its type or value, or a trailing escape.
func rdns(dn string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	esc := false
	flush := func() bool {
		t, v, ok := strings.Cut(cur.String(), "=")
		t, v = strings.TrimSpace(t), strings.TrimSpace(v)
		if !ok || t == "" || v == "" {
			return false
		}
		out = append(out, t+"="+v)
		cur.Reset()
		return true
	}
	for _, c := range dn {
		switch {
		case esc:
			esc = false
		case c == '\\':
			esc = true
		case c == ',':
			if !flush() {
				return nil, false
			}
			continue
		}
		cur.WriteRune(c)
	}
	if esc || !flush() {
		return nil, false
	}
	return out, true
}

// kerberosLink is the pre-write step of a user's write: the Kerberos name the
// spec states, linked for the user SID the directory holds under it, asked
// before the transaction, unless the user already holds that name, which
// keeps its link and asks nothing. A name the directory cannot answer for is
// refused 503 and one it holds no entry for 400 naming it, each before
// anything is written; with no directory, a new name is refused 503.
func kerberosLink(ctx context.Context, rq request[userDoc]) (entity.ProtocolID, error) {
	name := rq.Body.Spec.Kerberos
	if name == "" {
		return entity.ProtocolID{}, nil
	}
	if len(name) > maxDirectoryName {
		return entity.ProtocolID{}, &fieldError{field: "spec.kerberos", detail: "a Kerberos name is at most 1024 bytes"}
	}
	if rq.ID != ([16]byte{}) && rq.d.Store != nil {
		u, ok, err := rq.d.Store.User(ctx, userAt(rq.ID))
		if err != nil {
			return entity.ProtocolID{}, err
		}
		if id, held := krbOf(u.IDs); ok && held && id.Value == name {
			return id, nil
		}
	}
	if rq.d.Directory == nil {
		return entity.ProtocolID{}, errNoDirectory
	}
	s, err := rq.d.Directory.KerberosLinkSID(ctx, name)
	switch {
	case errors.Is(err, vfserr.ErrUnknownPrincipal):
		return entity.ProtocolID{}, &componentError{field: "spec.kerberos", detail: "the directory holds no user " + name, err: err}
	case err != nil:
		return entity.ProtocolID{}, err
	}
	id := entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name}
	if s.Revision != 0 {
		var b bytes.Buffer
		sid.EncodeSID(&b, &s)
		id.Linked = b.String()
	}
	return id, nil
}

// krbOf is the krb spelling ids hold, false for none.
func krbOf(ids entity.ProtocolIDs) (entity.ProtocolID, bool) {
	for _, id := range ids {
		if id.Scheme == entity.SchemeKerberos {
			return id, true
		}
	}
	return entity.ProtocolID{}, false
}

// withKerberos is ids with its krb spelling the one the spec names: removed
// when it names none, kept, link and all, when it names the one held, and
// otherwise link, which the pre-write step asked for that name. A link asked
// for another name than the spec's, the user's name changed under the step,
// is refused 503, as a directory that could not answer for it is.
func withKerberos(ids entity.ProtocolIDs, name string, link entity.ProtocolID) (entity.ProtocolIDs, error) {
	old, held := krbOf(ids)
	out := make(entity.ProtocolIDs, 0, len(ids)+1)
	for _, id := range ids {
		if id.Scheme != entity.SchemeKerberos {
			out = append(out, id)
		}
	}
	switch {
	case name == "":
		return out, nil
	case held && old.Value == name:
		return append(out, old), nil
	case link.Value != name:
		return nil, &refusal{code: apiv1.CodeUnavailable, detail: "the user's Kerberos name changed while its link was asked; retry"}
	}
	return append(out, link), nil
}
