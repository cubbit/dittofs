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

// Step is one step of a case: Change, nil for none, changes the server, and
// then each mount must get what Want says.
type Step struct {
	Name   string
	Change func(t *testing.T)
	Want   []Expect
}

// Case is an admission row of RFC 23 §12: Arrange creates its share and
// accounts on the server and returns the share and the row's steps, run in
// order on that same share and those accounts, so a refusal or an admission
// the server keeps past a change shows. The table names no protocol or
// flavour: each protocol's test runs every case once per flavour it
// presents, presenting each Who as that flavour spells it (over AUTH_SYS,
// its UID, through Reach) and asserting Want.
type Case struct {
	Row     string // the row, as RFC 23 §12 names it
	Arrange func(t *testing.T) (Share, []Step)
}

// Admission is the admission rows' table, which every protocol's mount
// tests share.
func Admission() []Case {
	return []Case{
		{"§3.2 create admits nobody", func(t *testing.T) (Share, []Step) {
			sh, alice := NewShare(t, "created"), NewAccount(t, "alice", "", 0)
			return sh, []Step{
				{"a new share refuses alice", nil, []Expect{{alice, Refused}}},
				{"granted read-write, alice is admitted", func(t *testing.T) { Grant(t, sh, alice.ID, ReadWrite) }, []Expect{{alice, ReadWrite}}},
			}
		}},
		{"§3.5 grants", func(t *testing.T) (Share, []Step) {
			sh, alice, finance := NewShare(t, "finance"), NewAccount(t, "alice", "", 0), NewGroup(t, "finance")
			Member(t, finance, &alice)
			Grant(t, sh, finance.ID, Read)
			Grant(t, sh, alice.ID, Refused)
			return sh, []Step{
				{"alice granted none is refused though Finance holds read", nil, []Expect{{alice, Refused}}},
				{"alice's grant deleted, she is admitted read through Finance", func(t *testing.T) { Ungrant(t, sh, alice.ID) }, []Expect{{alice, Read}}},
			}
		}},
		{"§5.1 roles grant nothing", func(t *testing.T) (Share, []Step) {
			// The mount group holds read, so the share is not one nobody
			// is admitted to: a refusal shows that the role gave nothing.
			sh := NewShare(t, "roles")
			want := []Expect{{Admin(), Refused}}
			for _, r := range []apiv1.Role{apiv1.RoleViewer, apiv1.RoleOperator, apiv1.RoleAdmin} {
				want = append(want, Expect{NewAccount(t, string(r), r, 0), Refused})
			}
			return sh, []Step{{"the bootstrap administrator and every role are refused", nil, want}}
		}},
	}
}
