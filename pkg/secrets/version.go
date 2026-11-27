package secrets

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// Meta is written with a version: a machine password's KeyVersion, set once
// and never changed, and HandedOver, which Seal binds into the sealed value's
// additional data with the role, kind, owner, reference and version (RFC 16
// §2.3), so a flipped mark fails to open.
type Meta struct {
	KeyVersion uint32
	HandedOver bool
}

// Version is one opened version of a secret.
type Version struct {
	Number uint64
	Value  []byte
	Meta   Meta
}

// view is what a Version shows of itself: its number and Meta, never its
// value. Value is an exported byte slice that json and yaml would otherwise
// print, so every printing path goes through view.
type view struct {
	Number uint64   `json:"number" yaml:"number"`
	Meta   metaView `json:"meta"   yaml:"meta"`
}

type metaView struct {
	KeyVersion uint32 `json:"key_version" yaml:"key_version"`
	HandedOver bool   `json:"handed_over" yaml:"handed_over"`
}

func (v Version) view() view {
	return view{
		Number: v.Number,
		Meta: metaView{
			KeyVersion: v.Meta.KeyVersion,
			HandedOver: v.Meta.HandedOver,
		},
	}
}

// String prints the version's number and Meta, never its value.
func (v Version) String() string {
	return fmt.Sprintf(
		"{number:%d meta:{key_version:%d handed_over:%t}}",
		v.Number,
		v.Meta.KeyVersion,
		v.Meta.HandedOver,
	)
}

// Format prints String for every verb, and a Go-syntax form without Value
// for %#v, so no fmt verb reaches the value.
func (v Version) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = fmt.Fprintf(
			f,
			"secrets.Version{Number:%d, Meta:secrets.Meta{KeyVersion:%d, HandedOver:%t}}",
			v.Number,
			v.Meta.KeyVersion,
			v.Meta.HandedOver,
		)

		return
	}

	_, _ = fmt.Fprint(f, v.String())
}

// LogValue hands slog the number and Meta as a group, never the struct.
func (v Version) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Uint64("number", v.Number),
		slog.Group(
			"meta",
			slog.Uint64("key_version", uint64(v.Meta.KeyVersion)),
			slog.Bool("handed_over", v.Meta.HandedOver),
		),
	)
}

// MarshalJSON writes the number and Meta, never the value.
func (v Version) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.view())
}

// MarshalYAML writes the number and Meta, never the value.
func (v Version) MarshalYAML() (any, error) {
	return v.view(), nil
}
