package section_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest"
	"github.com/cubbit/dittofs/pkg/carver/section"
)

// Every supported Target validates, and each refused one is refused with
// ErrInvalidSettings, as carve.CheckTarget refuses it, with no key material.
func TestValidate(t *testing.T) {
	for _, target := range carvetest.Targets {
		if err := (section.Settings{Target: target}).Validate(); err != nil {
			t.Errorf("Validate(Target %d) = %v, want nil", target, err)
		}
	}

	for _, target := range []int{48 << 10, 96 << 10, 32 << 10, 8 << 20, 0, -1} {
		if err := (section.Settings{Target: target}).Validate(); !errors.Is(err, carver.ErrInvalidSettings) {
			t.Errorf("Validate(Target %d) = %v, want ErrInvalidSettings", target, err)
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

// Target is stored under the key target and is bound, never live, so a
// namespace cannot re-cut its content by a quiet change.
func TestTargetTags(t *testing.T) {
	f, ok := reflect.TypeFor[section.Settings]().FieldByName("Target")
	if !ok {
		t.Fatal("Settings has no Target field")
	}

	if got := f.Tag.Get("json"); got != "target" {
		t.Errorf("Target's json tag = %q, want %q", got, "target")
	}

	if got := f.Tag.Get("class"); got != "bound" {
		t.Errorf("Target's class tag = %q, want %q", got, "bound")
	}

	if section.Prefix != "carver" {
		t.Errorf("Prefix = %q, want %q", section.Prefix, "carver")
	}
}
