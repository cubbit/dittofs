package assembler_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/assembler"
)

// valid is a configuration New accepts; each case changes one field of it.
func valid() carver.AssemblerConfig {
	return carver.AssemblerConfig{
		BlockTarget: 4 << 20,
		MaxChunks:   512,
		KeyBudget:   10_000,
		BlockKeys:   3,
		FileKeys:    1,
		MaxAge:      time.Minute,
	}
}

// checkNew asserts New accepts c when field is empty, and otherwise refuses it
// with ErrInvalidSettings, a nil fold and an error naming field.
func checkNew(t *testing.T, c carver.AssemblerConfig, field string) {
	t.Helper()
	f, err := assembler.New(c)
	if field == "" {
		if err != nil || f == nil {
			t.Fatalf("New(%+v): got (%v, %v), want a fold and no error", c, f, err)
		}
		return
	}
	if !errors.Is(err, carver.ErrInvalidSettings) || f != nil {
		t.Fatalf("New(%+v): got (%v, %v), want (nil, ErrInvalidSettings)", c, f, err)
	}
	if !strings.Contains(err.Error(), field) {
		t.Errorf("New(%+v): error %q does not name %s", c, err, field)
	}
}

func TestNewRefusesBlockTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target int64
		field  string // "BlockTarget" when refused, empty when accepted
	}{
		{"0", 0, "BlockTarget"},
		{"512 KiB", 512 << 10, "BlockTarget"},
		{"128 MiB", 128 << 20, "BlockTarget"},
		{"1 MiB - 1", 1<<20 - 1, "BlockTarget"},
		{"64 MiB + 1", 64<<20 + 1, "BlockTarget"},
		{"1 MiB", 1 << 20, ""},
		{"64 MiB", 64 << 20, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			c.BlockTarget = tc.target
			checkNew(t, c, tc.field)
		})
	}
}

func TestNewRefusesConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string // the field a refusal names; empty for an accepted config
		edit  func(*carver.AssemblerConfig)
	}{
		{"valid", "", func(*carver.AssemblerConfig) {}},
		{"no age, no keys per block or file", "", func(c *carver.AssemblerConfig) { c.BlockKeys, c.FileKeys, c.MaxAge = 0, 0, 0 }},
		{"count cap 1", "", func(c *carver.AssemblerConfig) { c.MaxChunks = 1 }},
		{"count cap 1024", "", func(c *carver.AssemblerConfig) { c.MaxChunks = 1024 }},
		{"key budget 1", "", func(c *carver.AssemblerConfig) { c.KeyBudget = 1 }},
		{"count cap 0", "MaxChunks", func(c *carver.AssemblerConfig) { c.MaxChunks = 0 }},
		{"count cap 1025", "MaxChunks", func(c *carver.AssemblerConfig) { c.MaxChunks = 1025 }},
		{"key budget 0", "KeyBudget", func(c *carver.AssemblerConfig) { c.KeyBudget = 0 }},
		{"per-block keys -1", "BlockKeys", func(c *carver.AssemblerConfig) { c.BlockKeys = -1 }},
		{"per-file keys -1", "FileKeys", func(c *carver.AssemblerConfig) { c.FileKeys = -1 }},
		{"max age -1ns", "MaxAge", func(c *carver.AssemblerConfig) { c.MaxAge = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.edit(&c)
			checkNew(t, c, tc.field)
		})
	}
}
