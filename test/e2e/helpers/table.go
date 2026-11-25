//go:build e2e

package helpers

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Access is a grant's access, and what a mount of a share gets: refused,
// read or read-write.
type Access string

const (
	Refused   Access = "none"
	Read      Access = "read"
	ReadWrite Access = "read-write"
)

// Expect is one account's mount of a case's share and what it must get.
type Expect struct {
	Who  Account
	Want Access
}

// Case is one step of an admission row of RFC 23 §12: Arrange creates its
// share and accounts on the server and returns the share and each mount's
// expectation. The table names no protocol or flavour: each protocol's test
// runs every case once per flavour it presents, presenting Who as that
// flavour spells it (over AUTH_SYS, its UID, which the process reading the
// mount runs as) and asserting Want.
type Case struct {
	Row     string // the row, as RFC 23 §12 names it
	Name    string // the step
	Arrange func(t *testing.T) (Share, []Expect)
}

// Admission is the admission rows' table, which every protocol's mount
// tests share.
func Admission() []Case {
	return []Case{
		{"§3.2 create admits nobody", "a new share refuses alice", func(t *testing.T) (Share, []Expect) {
			sh, alice := NewShare(t, "created"), NewAccount(t, "alice", "", 0)
			return sh, []Expect{{alice, Refused}}
		}},
		{"§3.2 create admits nobody", "alice granted read-write is admitted", func(t *testing.T) (Share, []Expect) {
			sh, alice := NewShare(t, "created"), NewAccount(t, "alice", "", 0)
			Grant(t, sh, alice.ID, ReadWrite)
			return sh, []Expect{{alice, ReadWrite}}
		}},
		{"§3.5 grants", "alice granted none is refused though Finance holds read", func(t *testing.T) (Share, []Expect) {
			sh, alice := financeShare(t)
			Grant(t, sh, alice.ID, Refused)
			return sh, []Expect{{alice, Refused}}
		}},
		{"§3.5 grants", "alice's none deleted, she is admitted read through Finance", func(t *testing.T) (Share, []Expect) {
			sh, alice := financeShare(t)
			Grant(t, sh, alice.ID, Refused)
			Ungrant(t, sh, alice.ID)
			return sh, []Expect{{alice, Read}}
		}},
		{"§5.1 roles grant nothing", "the bootstrap administrator and every role are refused", func(t *testing.T) (Share, []Expect) {
			sh := NewShare(t, "roles")
			want := []Expect{{Admin(), Refused}}
			for _, r := range []apiv1.Role{apiv1.RoleViewer, apiv1.RoleOperator, apiv1.RoleAdmin} {
				want = append(want, Expect{NewAccount(t, string(r), r, 0), Refused})
			}
			return sh, want
		}},
	}
}

// financeShare is a new share on which a new group, Finance, holds read and
// whose member alice holds no grant yet.
func financeShare(t *testing.T) (Share, Account) {
	t.Helper()
	sh, alice, finance := NewShare(t, "finance"), NewAccount(t, "alice", "", 0), Group(t, "finance")
	Member(t, finance, alice.ID)
	Grant(t, sh, finance, Read)
	return sh, alice
}
