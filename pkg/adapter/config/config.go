// Package config holds the adapters' settings shapes: what every adapter
// reads, and what only NFS or only SMB reads. It is a leaf, importing only the
// standard library, so the node, the control plane and every adapter can take
// it.
//
// Each field carries its mark: a restart-only field the tag restart:"true", a
// live one none. Each field read from the settings record carries its default
// in a default tag.
//
// decision: a default is a struct tag, not code that fills a zero value, so
// Validate never defaults: the record loader fills a field the record omits
// from its tag, and Validate refuses a zero it is handed. A default is then
// one reviewed string beside its field, and a test compares each with the
// value its RFC states. Replace the tags with a constructor if a default is
// found that cannot be written as one string.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
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
	Addrs []netip.Addr

	// DrainDeadline is the longest a draining adapter waits for requests
	// already handed to the service; at least the break deadline plus the
	// request deadline, so a request held on a break can finish.
	DrainDeadline time.Duration `default:"90s"`
	// IdleTimeout closes a connection with no request and no held state.
	IdleTimeout time.Duration `default:"15m"`
	// WriteDeadline is the longest one message may wait to be written before
	// its connection is closed.
	WriteDeadline time.Duration `default:"60s"`
	// HandshakeDeadline bounds negotiate and authenticate, from accept.
	HandshakeDeadline time.Duration `default:"30s"`
	// FrameDeadline bounds the rest of a request once its first byte arrived.
	FrameDeadline time.Duration `default:"30s"`
	// Disabled are operations switched off by name; the pipeline answers one
	// as an operation its table lacks.
	Disabled []string `default:""`
}

// NFS is the NFS adapter's whole shape: the common part and NFS over TLS.
type NFS struct {
	Adapter
	// TLS is off while CertFile is empty.
	TLS struct {
		CertFile   string `default:""`
		KeyFile    string `default:""`
		ClientCA   string `default:""` // set: a client certificate is required and verified
		MinVersion string `default:"1.3"`
		Mode       string `default:"opportunistic"` // or "require"
	}
}

// SMB is the SMB adapter's whole shape: the common part and the dialect range.
type SMB struct {
	Adapter
	// Dialects are the lowest and highest dialect served.
	Dialects struct {
		Min string `default:"2.1"`
		Max string `default:"3.1.1"`
	}
}

// Settings is NFS or SMB: its one method is unexported, so no other type
// satisfies it. Reconfigure takes it so that a protocol's own live settings
// reach it; Reconfigure refuses the other protocol's shape.
type Settings interface{ common() Adapter }

func (s NFS) common() Adapter { return s.Adapter }
func (s SMB) common() Adapter { return s.Adapter }

// AdapterOf returns the Adapter s embeds.
func AdapterOf(s Settings) Adapter { return s.common() }

// The drain deadline's floor: SMB's break deadline plus the request deadline.
const (
	breakDeadline   = 35 * time.Second
	requestDeadline = 30 * time.Second
)

// tlsModes and dialects are the values those fields take; dialects in order.
var (
	tlsModes = []string{"opportunistic", "require"}
	dialects = []string{"2.1", "3.0", "3.0.2", "3.1.1"}
)

// Validate checks every field of s against its rule and returns an error
// naming the field and the rule; it never clamps or defaults. New,
// Reconfigure and the control plane call it.
//
// ponytail: the TLS block is checked field by field, not by loading its
// files: this package is a leaf, and the certificate loader is the shared
// one the NFS adapter builds TLS with. Load the files at the NFS constructor
// when it builds TLS, so a certificate that does not load is still refused
// at configured.
func Validate(s Settings) error {
	var errs []error
	refuse := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("config: %s: "+format, append([]any{field}, args...)...))
	}
	switch v := s.(type) {
	case NFS:
		validateAdapter(v.Adapter, refuse)
		t := v.TLS
		if !slices.Contains(tlsModes, t.Mode) {
			refuse("TLS.Mode", "%q is not one of %q", t.Mode, tlsModes)
		}
		if t.MinVersion != "1.3" {
			refuse("TLS.MinVersion", "%q is not 1.3, the floor of NFS over TLS", t.MinVersion)
		}
		if (t.CertFile == "") != (t.KeyFile == "") {
			refuse("TLS.KeyFile", "CertFile and KeyFile are set together or not at all")
		}
		if t.ClientCA != "" && t.CertFile == "" {
			refuse("TLS.ClientCA", "set while TLS is off (CertFile empty)")
		}
	case SMB:
		validateAdapter(v.Adapter, refuse)
		lo, hi := slices.Index(dialects, v.Dialects.Min), slices.Index(dialects, v.Dialects.Max)
		if lo < 0 {
			refuse("Dialects.Min", "%q is not one of %q", v.Dialects.Min, dialects)
		}
		if hi < 0 {
			refuse("Dialects.Max", "%q is not one of %q", v.Dialects.Max, dialects)
		}
		if lo >= 0 && hi >= 0 && lo > hi {
			refuse("Dialects.Max", "%s is below Dialects.Min %s", v.Dialects.Max, v.Dialects.Min)
		}
	default:
		return fmt.Errorf("config: %T is not config.NFS or config.SMB", s)
	}
	return errors.Join(errs...)
}

func validateAdapter(a Adapter, refuse func(field, format string, args ...any)) {
	for i, addr := range a.Addrs {
		if !addr.IsValid() {
			refuse(fmt.Sprintf("Addrs[%d]", i), "not a valid address")
		} else if slices.Contains(a.Addrs[:i], addr) {
			refuse(fmt.Sprintf("Addrs[%d]", i), "%s is listed twice", addr)
		}
	}
	if floor := breakDeadline + requestDeadline; a.DrainDeadline < floor {
		refuse("DrainDeadline", "%s is below %s, the break deadline (%s) plus the request deadline (%s)",
			a.DrainDeadline, floor, breakDeadline, requestDeadline)
	}
	for _, d := range []struct {
		field string
		d     time.Duration
	}{
		{"IdleTimeout", a.IdleTimeout},
		{"WriteDeadline", a.WriteDeadline},
		{"HandshakeDeadline", a.HandshakeDeadline},
		{"FrameDeadline", a.FrameDeadline},
	} {
		if d.d <= 0 {
			refuse(d.field, "%s is not positive", d.d)
		}
	}
	for i, op := range a.Disabled {
		if op == "" {
			refuse(fmt.Sprintf("Disabled[%d]", i), "empty operation name")
		} else if slices.Contains(a.Disabled[:i], op) {
			refuse(fmt.Sprintf("Disabled[%d]", i), "%q is listed twice", op)
		}
	}
}

// ErrRestartRequired is wrapped by an error refusing a change to a
// restart-only field.
var ErrRestartRequired = errors.New("config: restart required")

// RestartRequired compares two shapes of one protocol, field by field. It
// returns nil when no field tagged restart:"true" differs, and otherwise an
// error wrapping ErrRestartRequired that names each one that does. Two
// protocols' shapes are refused with an error that does not wrap it.
//
// decision: no adapter field is restart-only today. Every adapter setting is
// live, and Addrs, restart-only in the bootstrap file, is live here; so
// RestartRequired returns nil for every pair of NFS or SMB shapes, and the tag
// and the comparison exist so that the first restart-only field needs no new
// mechanism. Tag one when a setting is found that a connection accepted after
// Reconfigure cannot pick up.
func RestartRequired(old, next Settings) error {
	if old == nil || next == nil || reflect.TypeOf(old) != reflect.TypeOf(next) {
		return fmt.Errorf("config: %T and %T are not one protocol's settings", old, next)
	}
	var changed []string
	restartFields(reflect.ValueOf(old), reflect.ValueOf(next), "", &changed)
	if len(changed) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s changed", ErrRestartRequired, strings.Join(changed, ", "))
}

// restartFields appends the path of each field tagged restart-only whose
// value differs between a and b. It descends into embedded structs, which add
// nothing to the path, and into unnamed nested ones, such as NFS.TLS.
func restartFields(a, b reflect.Value, prefix string, changed *[]string) {
	t := a.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		switch {
		case f.Tag.Get("restart") == "true":
			if !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
				*changed = append(*changed, prefix+f.Name)
			}
		case f.Anonymous:
			restartFields(a.Field(i), b.Field(i), prefix, changed)
		case f.Type.Kind() == reflect.Struct && f.Type.Name() == "":
			restartFields(a.Field(i), b.Field(i), prefix+f.Name+".", changed)
		}
	}
}
