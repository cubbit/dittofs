package section_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/carve"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/carver/section"
)

// Every supported Target validates, and each refused one is refused with
// ErrInvalidSettings, as carve.CheckTarget refuses it, with no key material;
// the refusal names the field, since the settings table prints it under the
// section's key, which a second "carver:" would repeat.
func TestValidate(t *testing.T) {
	for _, target := range carvetest.Targets {
		if err := (section.Settings{Target: target}).Validate(); err != nil {
			t.Errorf("Validate(Target %d) = %v, want nil", target, err)
		}
	}

	for _, target := range []int{48 << 10, 96 << 10, 32 << 10, 8 << 20, 0, -1, 64<<10 + 1, 3 << 20, 1 << 30} {
		err := (section.Settings{Target: target}).Validate()
		if !errors.Is(err, carver.ErrInvalidSettings) {
			t.Errorf("Validate(Target %d) = %v, want ErrInvalidSettings", target, err)
			continue
		}

		if msg := err.Error(); !strings.HasPrefix(msg, "target: ") || strings.Contains(msg, "carver") {
			t.Errorf("Validate(Target %d) says %q, want it keyed by target and not naming the section", target, msg)
		}
	}
}

// Validate and carve.CheckTarget agree on every power of two, its neighbours,
// zero and minus one, so the validator cannot drift from the constructor.
func TestValidateAgreesWithCheckTarget(t *testing.T) {
	targets := []int{0, -1}
	for p := 1; p <= 1<<30; p <<= 1 {
		targets = append(targets, p-1, p, p+1)
	}

	for _, target := range targets {
		got := errors.Is((section.Settings{Target: target}).Validate(), carver.ErrInvalidSettings)
		want := errors.Is(carve.CheckTarget(target), carver.ErrInvalidSettings)
		if got != want {
			t.Errorf("Target %d: Validate refuses %v, CheckTarget refuses %v", target, got, want)
		}
	}
}

// The default is RFC 13's proposed 256 KiB, and a namespace can bind it.
func TestDefault(t *testing.T) {
	d := section.Default()
	if d.Target != 256<<10 {
		t.Errorf("Default().Target = %d, want %d", d.Target, 256<<10)
	}

	if err := d.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}

// The section's one field is Target, stored under the key target and bound,
// never live, so a namespace cannot re-cut its content by a quiet change; it
// names no location, so no store gate applies.
func TestTargetTags(t *testing.T) {
	typ := reflect.TypeFor[section.Settings]()
	if typ.NumField() != 1 || typ.Field(0).Name != "Target" {
		t.Fatalf("Settings has %d fields, the first %q, want exactly Target", typ.NumField(), typ.Field(0).Name)
	}

	f := typ.Field(0)

	if got := f.Tag.Get("json"); got != "target" {
		t.Errorf("Target's json tag = %q, want %q", got, "target")
	}

	if got := f.Tag.Get("class"); got != "bound" {
		t.Errorf("Target's class tag = %q, want %q", got, "bound")
	}

	if got, ok := f.Tag.Lookup("location"); ok {
		t.Errorf("Target's location tag = %q, want none", got)
	}
}

// The section's key is carver.target: the prefix carver, the field target.
func TestKey(t *testing.T) {
	if section.Prefix != "carver" {
		t.Errorf("Prefix = %q, want %q", section.Prefix, "carver")
	}
}
