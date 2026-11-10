// Package config holds the adapters' settings shapes: what every adapter
// reads, and what only NFS or only SMB reads. It is a leaf, importing only the
// standard library, so the node, the control plane and every adapter can take
// it.
//
// Every field's json tag names its key under adapters.<protocol>. The defaults
// are DefaultNFS and DefaultSMB: the loader decodes a record over them, so an
// omitted field keeps its default, and Validate refuses a zero value where one
// is required, never filling it.
//
// decision: every adapter setting is live, so there is no restart-only mark
// and no comparison of two shapes. The first restart-only setting brings a
// sentinel error and a one-field comparison in Reconfigure.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"
)

// Adapter is what every adapter reads; core reads only this.
type Adapter struct {
	// Addrs are the addresses each listener binds, supplied by the node from
	// the host's bootstrap and the addresses it took over, never from the
	// settings record.
	//
	// decision: Addrs is live, though a host's listen addresses are
	// restart-only in its bootstrap file: a floating-address takeover must add
	// an address to a serving adapter, and a restart would drop every
	// connection on the others. The node, not the operator, changes Addrs
	// while serving. Withdraw it if a bind on a live adapter is found to
	// disturb the connections already accepted.
	Addrs []netip.Addr `json:"-"`

	// DrainTimeout is the longest a draining adapter waits for requests
	// already handed to the service; at least the break deadline plus the
	// request deadline, so a request held on a break can finish.
	DrainTimeout time.Duration `json:"drain_timeout"`
	// IdleTimeout closes a connection with no request and no held state.
	IdleTimeout time.Duration `json:"idle_timeout"`
	// WriteTimeout is the longest one message may wait to be written before
	// its connection is closed.
	WriteTimeout time.Duration `json:"write_timeout"`
	// HandshakeTimeout bounds negotiate and authenticate, from accept.
	HandshakeTimeout time.Duration `json:"handshake_timeout"`
	// ReadTimeout bounds the rest of a request once its first byte arrived.
	ReadTimeout time.Duration `json:"read_timeout"`
	// DisabledOperations are switched off by name; the pipeline answers one as
	// an operation its table lacks.
	DisabledOperations []string `json:"disabled_operations"`
}

// Common returns the Adapter; NFS and SMB promote it.
func (a Adapter) Common() Adapter { return a }

// TLS is NFS over TLS. Any Mode but off requires CertFile and KeyFile, and
// none of the files is set while off. The version floor is TLS 1.3, fixed.
type TLS struct {
	Mode     string `json:"mode"` // "off", "opportunistic" or "require"
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	ClientCA string `json:"client_ca"` // set: a client certificate is required and verified
}

// VersionRange is the versions an adapter serves, inclusive, as the protocol
// names them. A change applies to connections accepted afterwards; a
// connection keeps what it negotiated.
type VersionRange struct {
	Min string `json:"min"`
	Max string `json:"max"`
}

// NFS is the NFS adapter's whole shape: the common part, NFS over TLS and the
// versions served.
type NFS struct {
	Adapter
	TLS TLS `json:"tls"`
	// Versions are among "3", "4.0", "4.1" and "4.2"; "3" includes its
	// auxiliary programs MOUNT, NLM and NSM.
	Versions VersionRange `json:"versions"`
}

// SMB is the SMB adapter's whole shape: the common part, the dialect range
// and how long a request is held pending.
type SMB struct {
	Adapter
	Dialects VersionRange `json:"dialects"` // among "2.1", "3.0", "3.0.2" and "3.1.1"
	// PendingTimeout is the longest a request refused for a transient cause
	// is held pending before it is answered.
	PendingTimeout time.Duration `json:"pending_timeout"`
	// RecallTimeout is the longest a request held on a recall may wait; it
	// exceeds the break deadline.
	RecallTimeout time.Duration `json:"recall_timeout"`
}

// Settings is what Reconfigure takes, so that a protocol's own live settings
// reach it. Reconfigure refuses a Settings of the other protocol, or of any
// other type, with an error.
type Settings interface {
	Validate() error
	Common() Adapter
}

func defaultAdapter() Adapter {
	return Adapter{
		DrainTimeout:     90 * time.Second,
		IdleTimeout:      15 * time.Minute,
		WriteTimeout:     60 * time.Second,
		HandshakeTimeout: 30 * time.Second,
		ReadTimeout:      30 * time.Second,
	}
}

// DefaultNFS is every NFS setting's default.
func DefaultNFS() NFS {
	return NFS{
		Adapter:  defaultAdapter(),
		TLS:      TLS{Mode: "off"},
		Versions: VersionRange{Min: "3", Max: "4.2"},
	}
}

// DefaultSMB is every SMB setting's default.
func DefaultSMB() SMB {
	return SMB{
		Adapter:        defaultAdapter(),
		Dialects:       VersionRange{Min: "2.1", Max: "3.1.1"},
		PendingTimeout: 10 * time.Minute,
		RecallTimeout:  60 * time.Second,
	}
}

// The drain timeout's floor: SMB's break deadline plus the request deadline.
const (
	breakDeadline   = 35 * time.Second
	requestDeadline = 30 * time.Second
)

// tlsModes, nfsVersions and dialects are the values those fields take;
// versions and dialects in order.
var (
	tlsModes    = []string{"off", "opportunistic", "require"}
	nfsVersions = []string{"3", "4.0", "4.1", "4.2"}
	dialects    = []string{"2.1", "3.0", "3.0.2", "3.1.1"}
)

// refusals collects Validate's errors, each naming its field and rule.
type refusals []error

func (r *refusals) refuse(field, format string, args ...any) {
	*r = append(*r, fmt.Errorf("config: %s: "+format, append([]any{field}, args...)...))
}

// Validate checks every field against its rule, shapes and ranges only,
// touching no file, and returns an error naming the field and the rule; it
// never clamps or defaults. New, Reconfigure and the control plane call it;
// nfs.New loads the TLS files.
func (s NFS) Validate() error {
	var r refusals
	s.validate(&r)
	s.Versions.validate(&r, "Versions", nfsVersions)
	t := s.TLS
	switch {
	case !slices.Contains(tlsModes, t.Mode):
		r.refuse("TLS.Mode", "%q is not one of %q", t.Mode, tlsModes)
	case t.Mode == "off":
		for _, f := range []struct{ field, v string }{
			{"TLS.CertFile", t.CertFile}, {"TLS.KeyFile", t.KeyFile}, {"TLS.ClientCA", t.ClientCA},
		} {
			if f.v != "" {
				r.refuse(f.field, "set while Mode is off")
			}
		}
	default:
		if t.CertFile == "" {
			r.refuse("TLS.CertFile", "empty while Mode is %s", t.Mode)
		}
		if t.KeyFile == "" {
			r.refuse("TLS.KeyFile", "empty while Mode is %s", t.Mode)
		}
	}
	return errors.Join(r...)
}

// Validate checks every field against its rule and returns an error naming
// the field and the rule; it never clamps or defaults. That PendingTimeout
// exceeds the migration freeze timeout plus the break deadline is a rule
// across components, which the control plane checks: Validate sees no
// migration setting.
func (s SMB) Validate() error {
	var r refusals
	s.validate(&r)
	s.Dialects.validate(&r, "Dialects", dialects)
	if s.PendingTimeout <= 0 {
		r.refuse("PendingTimeout", "%s is not positive", s.PendingTimeout)
	}
	if s.RecallTimeout <= breakDeadline {
		r.refuse("RecallTimeout", "%s does not exceed the break deadline (%s)", s.RecallTimeout, breakDeadline)
	}
	return errors.Join(r...)
}

// validate refuses a bound not among known, which is in order, and Min above
// Max, naming field.Min or field.Max.
func (v VersionRange) validate(r *refusals, field string, known []string) {
	lo, hi := slices.Index(known, v.Min), slices.Index(known, v.Max)
	if lo < 0 {
		r.refuse(field+".Min", "%q is not one of %q", v.Min, known)
	}
	if hi < 0 {
		r.refuse(field+".Max", "%q is not one of %q", v.Max, known)
	}
	if lo >= 0 && hi >= 0 && lo > hi {
		r.refuse(field+".Max", "%s is below %s.Min %s", v.Max, field, v.Min)
	}
}

func (a Adapter) validate(r *refusals) {
	// An IPv4-mapped IPv6 address is its IPv4 address, and an unspecified
	// address already binds every other address of its family.
	for i, addr := range a.Addrs {
		field := fmt.Sprintf("Addrs[%d]", i)
		if !addr.IsValid() {
			r.refuse(field, "not a valid address")
			continue
		}
		addr = addr.Unmap()
		for _, prev := range a.Addrs[:i] {
			prev = prev.Unmap()
			if prev == addr {
				r.refuse(field, "%s is listed twice", addr)
				break
			}
			if prev.Is4() == addr.Is4() && (prev.IsUnspecified() || addr.IsUnspecified()) {
				r.refuse(field, "%s and %s overlap: an unspecified address binds its whole family", prev, addr)
				break
			}
		}
	}
	if floor := breakDeadline + requestDeadline; a.DrainTimeout < floor {
		r.refuse("DrainTimeout", "%s is below %s, the break deadline (%s) plus the request deadline (%s)",
			a.DrainTimeout, floor, breakDeadline, requestDeadline)
	}
	for _, d := range []struct {
		field string
		d     time.Duration
	}{
		{"IdleTimeout", a.IdleTimeout},
		{"WriteTimeout", a.WriteTimeout},
		{"HandshakeTimeout", a.HandshakeTimeout},
		{"ReadTimeout", a.ReadTimeout},
	} {
		if d.d <= 0 {
			r.refuse(d.field, "%s is not positive", d.d)
		}
	}
	for i, op := range a.DisabledOperations {
		field := fmt.Sprintf("DisabledOperations[%d]", i)
		if op == "" {
			r.refuse(field, "empty operation name")
		} else if slices.Contains(a.DisabledOperations[:i], op) {
			r.refuse(field, "%q is listed twice", op)
		}
	}
}
