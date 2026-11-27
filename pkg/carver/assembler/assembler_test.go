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
		MaxChunks:   1024,
		KeyBudget:   10_000,
		BlockKeys:   3,
		FileKeys:    1,
		MaxAge:      time.Minute,
	}
}

func TestNewRefusesBlockTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target int64
		ok     bool
	}{
		{"512 KiB", 512 << 10, false},
		{"128 MiB", 128 << 20, false},
		{"1 MiB - 1", 1<<20 - 1, false},
		{"64 MiB + 1", 64<<20 + 1, false},
		{"1 MiB", 1 << 20, true},
		{"64 MiB", 64 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			c.BlockTarget = tc.target
			f, err := assembler.New(c)
			if tc.ok {
				if err != nil || f == nil {
					t.Fatalf("New(BlockTarget=%d): got (%v, %v), want a fold and no error", tc.target, f, err)
				}
				return
			}
			if !errors.Is(err, carver.ErrInvalidSettings) || f != nil {
				t.Fatalf("New(BlockTarget=%d): got (%v, %v), want (nil, ErrInvalidSettings)", tc.target, f, err)
			}
			if !strings.Contains(err.Error(), "BlockTarget") {
				t.Errorf("New(BlockTarget=%d): error %q does not name BlockTarget", tc.target, err)
			}
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
		{"no ages, no keys per block or file", "", func(c *carver.AssemblerConfig) { c.BlockKeys, c.FileKeys, c.MaxAge = 0, 0, 0 }},
		{"count cap 1", "", func(c *carver.AssemblerConfig) { c.MaxChunks = 1 }},
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
			f, err := assembler.New(c)
			if tc.field == "" {
				if err != nil || f == nil {
					t.Fatalf("New(%+v): got (%v, %v), want a fold and no error", c, f, err)
				}
				return
			}
			if !errors.Is(err, carver.ErrInvalidSettings) || f != nil {
				t.Fatalf("New(%+v): got (%v, %v), want (nil, ErrInvalidSettings)", c, f, err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("New(%+v): error %q does not name %s", c, err, tc.field)
			}
		})
	}
}
