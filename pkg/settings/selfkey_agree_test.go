package settings_test

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// skNodeLimit is the node's connection limit section: one field whose key
// is the section's prefix, as RFC 13's Appendix B writes the bare
// connection_limit.
type skNodeLimit struct {
	Limit uint32 `json:"limit" class:"live"`
}

// Validate refuses a limit past the node's bound, printing the value alone,
// so a refusal never names the key it already sits under.
func (s skNodeLimit) Validate() error {
	if s.Limit > 4096 {
		return fmt.Errorf("limit must be at most 4096, got %d", s.Limit)
	}

	return nil
}

// The bare key reads like any declared key: no row leaves the default, a
// stored row decodes, and Get hands the value back.
func TestSelfKeyRead(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("connection_limit", settings.At(entity.ScopeNode),
			skNodeLimit{Limit: 512}, settings.SelfKey()),
	}

	snap, refused := rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)

	got, err := settings.Get[skNodeLimit](snap, "connection_limit", rdNodeA)
	if err != nil {
		t.Fatalf("Get with no row: %v", err)
	}

	if got.Limit != 512 {
		t.Fatalf("Get with no row: limit %d, want 512", got.Limit)
	}

	s.put(t, rdNodeScope(rdNodeA), "connection_limit", "64")

	snap, refused = rdRead(t, s.nodes, table, rdNodeA)
	rdWantNoRefusal(t, refused)

	got, err = settings.Get[skNodeLimit](snap, "connection_limit", rdNodeA)
	if err != nil {
		t.Fatalf("Get with the row: %v", err)
	}

	if got.Limit != 64 {
		t.Fatalf("Get with the row: limit %d, want 64", got.Limit)
	}
}

// SelfKey leaves KeyRole's gate standing on the bare key: an operator is
// refused the put an admin passes, under the key the section declares.
func TestSelfKeyKeyRole(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := settings.Table{
		settings.Of("connection_limit", settings.At(entity.ScopeNode),
			skNodeLimit{Limit: 512},
			settings.SelfKey(), settings.KeyRole("Limit", entity.RoleAdmin),
		),
	}

	refused(t, "operator's put", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "connection_limit", "64"))), settings.ErrForbidden,
		fmt.Sprintf("setting (%s): connection_limit: written by admin only", scopeName(nodeA)))

	passed(t, "admin's put", s.check(tb, entity.RoleAdmin,
		settings.Put(setting(nodeA, "connection_limit", "64"))))
}

// A selfkey section's change names the bare key, both as its section and as
// the changed key.
func TestSelfKeyDiff(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("connection_limit", settings.At(entity.ScopeNode),
			skNodeLimit{Limit: 512}, settings.SelfKey()),
	}
	node := rdNodeScope(rdNodeA)

	prev, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfPut(t, s, node, "connection_limit", "64", 0)
	cur, _ := rdRead(t, s.nodes, table, rdNodeA)

	dfWant(t, "connection_limit", settings.Diff(prev, cur), []settings.Change{
		dfChange("connection_limit", "connection_limit", settings.Live, node),
	})
}

// The bare key behaves through Check and Read like any declared key: an
// accepted write, a stored row that answers the same refusal a write of it
// gets, and a Validate refusal that names its key once.
func TestSelfKeyCheckReadAgree(t *testing.T) {
	prof.Watch(t, prof.Options{})

	tb := settings.Table{
		settings.Of("connection_limit", settings.At(entity.ScopeNode),
			skNodeLimit{Limit: 512}, settings.SelfKey()),
	}

	where := "setting (node " + hex.EncodeToString(nodeA.ID[:]) + "): "

	cases := []struct {
		name     string
		row      entity.Setting
		want     error
		wantLine string
	}{
		{
			"a value in range",
			setting(nodeA, "connection_limit", "64"),
			nil,
			"",
		},
		{
			"an unknown key under the section's prefix",
			setting(nodeA, "connection_limit.x", "1"),
			settings.ErrUnknownKey,
			where + `"connection_limit.x": unknown key; did you mean connection_limit?`,
		},
		{
			"a value past the bound",
			setting(nodeA, "connection_limit", "70000"),
			settings.ErrInvalid,
			where + "connection_limit: limit must be at most 4096, got 70000",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := open(t)

			checkErr := s.check(tb, entity.RoleOperator, settings.Put(c.row))
			s.raw(t, c.row.Scope, c.row.Key, rowValue(string(c.row.Value), 1, c.row.Schema))
			readErr := s.readRefused(t, tb)["connection_limit"]

			if !errors.Is(checkErr, c.want) || !errors.Is(readErr, c.want) {
				t.Fatalf("Check %v, Read %v, want both %v", checkErr, readErr, c.want)
			}

			if c.wantLine == "" {
				return
			}

			if checkErr.Error() != c.wantLine {
				t.Fatalf("Check %q, want %q", checkErr.Error(), c.wantLine)
			}

			if readErr.Error() != c.wantLine {
				t.Fatalf("Read %q, want %q", readErr.Error(), c.wantLine)
			}

			// The refusal prints under the key the section declares, which a
			// selfkey section makes the prefix itself, so Validate's refusal
			// names its key once.
			if c.want == settings.ErrInvalid {
				if n := strings.Count(checkErr.Error(), "connection_limit"); n != 1 {
					t.Fatalf("Check refusal names its key %d times: %q", n, checkErr.Error())
				}
			}
		})
	}
}
