package bootstrap

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/keyprovider"
)

// passphraseVar names the variable, in the environment Validate is given,
// that holds the passphrase unwrapping the local key files.
const passphraseVar = "DITTOFS_ENCRYPTION_PASSPHRASE"

// providerLoader loads one role's key provider from its configuration and
// a passphrase. Tests of every rule but the key files hand Validate a
// loader that unwraps nothing, so no test does an argon2 pass it does not
// need.
type providerLoader func(
	ctx context.Context,
	cfg keyprovider.Config,
	passphrase string,
) (keyprovider.KeyProvider, error)

// Validate refuses a bootstrap whose facts cannot make one node: a role
// missing, both roles naming one wrapping key, a listener off loopback with
// no certificate, a socket path no operating system could bind, a state
// directory another user could replace. It reads the bootstrap, the
// directories the environ argument names and the key files the bootstrap
// names; it creates nothing.
//
// Every refusal is a line of the load's report, <where>: <key>: <rule>, and
// all are collected in one error.
func (b *Bootstrap) Validate(ctx context.Context, environ []string) error {
	return b.validate(ctx, environ, keyprovider.NewProviderWithPassphrase, processUser())
}

// validate is Validate with its inputs from the host passed in: the loader
// of the key providers, and node, the user id the state directory may
// belong to besides root.
func (b *Bootstrap) validate(
	ctx context.Context,
	environ []string,
	load providerLoader,
	node uint32,
) error {
	var rep report

	b.validateRoles(&rep)
	b.validateRequired(&rep)
	b.validateCertificate(&rep)
	b.validateSocket(&rep)
	b.validateKeys(ctx, environ, load, &rep)
	b.validateDirs(lookupDirs(environ), node, &rep)

	if len(rep) == 0 {
		return nil
	}

	return rep.err()
}

// whereOf is the <where> of a refusal of a key nobody set: the file the
// bootstrap was read from, else the environment.
func (b *Bootstrap) whereOf() string {
	if b.where != "" {
		return b.where
	}

	return whereEnvironment
}

// validateRoles refuses a node that does not run both roles. The roles are
// compared as a set: both is the only accepted value, in any order, and a
// role listed twice is refused.
func (b *Bootstrap) validateRoles(rep *report) {
	var missing []string

	count := map[string]int{}

	for _, r := range b.Node.Roles {
		count[r]++
	}

	for _, role := range []string{"protocol", "storage"} {
		switch {
		case count[role] == 0:
			missing = append(missing, strconv.Quote(role))
		case count[role] > 1:
			rep.add(b.whereOf(), "node.roles", strconv.Quote(role)+" is listed twice")
		}
	}

	switch len(missing) {
	case 0:
	case 1:
		rep.add(b.whereOf(), "node.roles", "both roles are needed, "+missing[0]+" is missing")
	default:
		rep.add(b.whereOf(), "node.roles",
			"both roles are needed, "+strings.Join(missing, " and ")+" are missing")
	}
}

// validateRequired refuses the keys that are required here and not in Load,
// which the schema and show paths run with no node behind them: a node with
// no journal has nowhere to write, and a FoundationDB store with no cluster
// file has nowhere to look.
func (b *Bootstrap) validateRequired(rep *report) {
	if len(b.Journals) == 0 {
		rep.add(b.whereOf(), "journals", "required")
	}

	if b.Metadata.Backend == "foundationdb" && b.Metadata.FoundationDB.ClusterFile == "" {
		rep.add(b.whereOf(), "metadata.foundationdb.cluster_file",
			"required with metadata.backend: foundationdb")
	}
}

// validateCertificate refuses a node whose management API or metrics
// listener opens off loopback with no certificate: the API's password
// sign-in would travel in clear text. Loopback is read as RFC 23 §5.3 reads
// it, 127.0.0.0/8 and ::1 with an IPv4-mapped address read as its IPv4
// address; an unspecified address and a private one are off it.
//
// decision: a host name is off loopback here. Load refuses a host that is
// not an IP literal, so through a load the case never arrives; a bootstrap
// built by hand would need the name resolved, and the answer of a resolver
// is not a fact this check may depend on.
func (b *Bootstrap) validateCertificate(rep *report) {
	key, off := b.offLoopback()
	if !off {
		return
	}

	rule := "required, " + key + " has an address off loopback"

	if b.API.TLS.CertFile == "" {
		rep.add(b.whereOf(), "api.tls.cert_file", rule)
	}

	if b.API.TLS.KeyFile == "" {
		rep.add(b.whereOf(), "api.tls.key_file", rule)
	}
}

// offLoopback names the key of the first listen address that is not on
// loopback, and reports whether there is one. metrics.listen: off listens
// nowhere.
func (b *Bootstrap) offLoopback() (string, bool) {
	for _, a := range b.API.Listen {
		if !loopback(a) {
			return "api.listen", true
		}
	}

	if b.Metrics.Listen != "" && b.Metrics.Listen != "off" && !loopback(b.Metrics.Listen) {
		return "metrics.listen", true
	}

	return "", false
}

// loopback reports whether an ip:port address names the loopback.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// validateSocket refuses an api.socket past the socket-address limit, the
// default one the directories make included: Load checks the paths a source
// gave, and the default is only known once the runtime directory is.
func (b *Bootstrap) validateSocket(rep *report) {
	if len(b.API.Socket) > socketLimit {
		rep.add(b.whereOf(), "api.socket",
			"longer than "+strconv.Itoa(socketLimit)+" bytes, the socket address limit")
	}
}

// validateKeys loads both roles' wrapping keys and refuses two roles that
// name one: a leaked key would then open both kinds of secret. A copied key
// file and a link to one hold the same key, so the keys are compared by the
// current key ID the provider reports, not by path.
//
// A provider's error is turned into a fixed phrase by its sentinel, never
// printed, so no path and no passphrase reach a report. Each provider is
// closed as soon as its key ID is read, a load that fails after the first
// one included: Close zeroes the key material.
func (b *Bootstrap) validateKeys(
	ctx context.Context,
	environ []string,
	load providerLoader,
	rep *report,
) {
	passphrase := environMap(environ)[passphraseVar]

	protocol, protocolOK := b.roleKeyID(ctx, "protocol", b.Keys.Protocol, passphrase, load, rep)
	storage, storageOK := b.roleKeyID(ctx, "storage", b.Keys.Storage, passphrase, load, rep)

	if protocolOK && storageOK && protocol == storage {
		rep.add(b.whereOf(), "keys.storage.kind", "names the same key as keys.protocol")
	}
}

// roleKeyID loads one role's key and returns its current key ID, false when
// the role's key is refused.
func (b *Bootstrap) roleKeyID(
	ctx context.Context,
	role string,
	k WrappingKey,
	passphrase string,
	load providerLoader,
	rep *report,
) (string, bool) {
	where := b.whereOf()
	key := "keys." + role + "." + string(k.Kind)

	switch {
	case k.Kind == "":
		rep.add(where, "keys."+role+".kind", "required")

		return "", false
	case k.Kind == keyprovider.KindLocal && k.Local.File == "":
		rep.add(where, key+".file", "required with local")

		return "", false
	case k.Kind == keyprovider.KindLocal:
		key += ".file"
	case k.Kind == keyprovider.KindKMIP:
		key += ".endpoint"
	}

	p, err := load(ctx, k.Provider(), passphrase)
	if err != nil {
		rep.add(where, key, loadRule(err))

		return "", false
	}

	id := p.CurrentMasterKeyID()

	_ = p.Close()

	return id, true
}

// loadRule names why a key did not load with a fixed phrase.
func loadRule(err error) string {
	switch {
	case errors.Is(err, keyprovider.ErrPassphraseMissing):
		return "the passphrase is unset"
	case errors.Is(err, keyprovider.ErrUnwrapFailed):
		return "the key file does not unwrap"
	case errors.Is(err, keyprovider.ErrKeyFileCorrupt):
		return "the key file is corrupt"
	default:
		return "the key does not load"
	}
}

// validateDirs refuses the two directories when another user could replace
// what sits in them: the state directory a symlink, owned by neither the
// node's user nor root, or writable by others; the runtime directory a
// symlink, and either one a path that is not absolute. A directory that is
// missing passes, nothing is created here.
//
// decision: the runtime directory's owner and writers are not read here. The
// service manager hands over a directory it made, and who may write it is
// the control plane's rule for its group subdirectory; refusing on those
// bits would refuse a node the service started correctly.
func (b *Bootstrap) validateDirs(d resolvedDirs, node uint32, rep *report) {
	switch {
	case d.stateRefused:
		rep.add(whereEnvironment, d.stateVar, "must be an absolute path")

		return
	case d.stateTemp:
		rep.add(whereEnvironment, d.stateVar, "is not set")

		return
	}

	state := dirFacts(d.state)

	for _, rule := range refuseDir(state, node) {
		rep.add(whereEnvironment, d.stateVar, rule)
	}

	// A state directory the host cannot read has no runtime directory
	// under it to read either.
	if state.readErr != nil {
		return
	}

	if d.runtimeRefused {
		rep.add(whereEnvironment, d.runtimeVar, "must be an absolute path")

		return
	}

	switch f := dirFacts(d.runtime); {
	case f.readErr != nil:
		rep.add(whereEnvironment, d.runtimeVar, readRule(f.readErr))
	case f.exists && f.symlink:
		rep.add(whereEnvironment, d.runtimeVar, "is a symlink")
	}
}

// dirState is what the host reports of one directory.
type dirState struct {
	exists  bool
	symlink bool
	mode    fs.FileMode

	// readErr is why the host could not report on the path, other than
	// the path being absent.
	readErr error

	// owner is the owner's user id, ownerSet false where the platform
	// records none and the owner check stays off.
	owner    uint32
	ownerSet bool
}

// dirFacts reads one path without following a symlink. Only a path that is
// absent is read as missing; any other error is kept, and refused.
func dirFacts(path string) dirState {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dirState{}
	case err != nil:
		return dirState{readErr: err}
	}

	f := dirState{
		exists:  true,
		symlink: fi.Mode()&fs.ModeSymlink != 0,
		mode:    fi.Mode().Perm(),
	}

	f.owner, f.ownerSet = dirOwner(fi)

	return f
}

// refuseDir names the refusals of a state directory: a symlink, a world
// write bit, an owner that is neither the node's user nor root. A missing
// directory names nothing, and where the platform records no owner its mode
// bits are not read either: Go reports every directory there as 0777.
func refuseDir(f dirState, node uint32) []string {
	switch {
	case f.readErr != nil:
		return []string{readRule(f.readErr)}
	case !f.exists:
		return nil
	case f.symlink:
		return []string{"is a symlink"}
	}

	var rules []string

	if f.ownerSet && f.mode&0o002 != 0 {
		rules = append(rules, "is writable by others")
	}

	if f.ownerSet && f.owner != 0 && f.owner != node {
		rules = append(rules, "is owned by another user")
	}

	return rules
}

// readRule names why a path could not be read with a fixed phrase, so the
// host's own error text, which holds the path, never reaches a report.
func readRule(err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return "is not readable"
	}

	return "cannot be read"
}
