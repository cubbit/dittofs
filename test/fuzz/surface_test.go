package fuzz

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// store is a surface declared as an interface.
type store interface {
	Write()
	Read()
	reset()
}

// disk is a surface declared as a concrete type, as a component that
// implements no interface is.
type disk struct{}

func (*disk) Sync()  {}
func (*disk) Close() {}

// covered is the register target over both kinds of surface and a fault source,
// with every name accounted for.
func covered() Target[*register, belief] {
	target := registerTarget(register{})
	target.Covers = []Surface{
		Interface[store](),
		Interface[*disk](),
		Faults("fs", []string{"fail", "torn"}),
	}
	target.Ops[0].Covers = []string{"fuzz.store.Write", "fault:fs.fail"}
	target.Ops[1].Covers = []string{"fuzz.disk.Sync", "fault:fs.torn"}
	target.Ops[2].Covers = []string{"fuzz.store.Read"}
	target.Uncovered = []Omission{
		{
			Name: "fuzz.disk.Close",
			Why:  "the crash closes it",
		},
	}

	return target
}

func TestCoverageNamesTheMissingMethod(t *testing.T) {
	t.Run("every name accounted for", func(t *testing.T) {
		if err := coverage(covered()); err != nil {
			t.Fatalf("coverage: %v", err)
		}
	})

	cases := []struct {
		name   string
		change func(*Target[*register, belief])
		want   []string
	}{
		{
			name: "a method and a fault point nothing covers",
			change: func(target *Target[*register, belief]) {
				target.Ops[0].Covers = nil
				target.Uncovered = nil
			},
			want: []string{
				"fuzz.store.Write is covered by no operation and no omission",
				"fault:fs.fail is covered by no operation and no omission",
				"fuzz.disk.Close is covered by no operation and no omission",
			},
		},
		{
			name: "an operation covers a name no surface has",
			change: func(target *Target[*register, belief]) {
				target.Ops[2].Covers = append(target.Ops[2].Covers, "fuzz.store.Flush")
			},
			want: []string{`operation "read" covers fuzz.store.Flush, which no surface has`},
		},
		{
			name: "an omission names what no surface has",
			change: func(target *Target[*register, belief]) {
				target.Uncovered = append(target.Uncovered, Omission{
					Name: "fault:fs.slow",
					Why:  "gone",
				})
			},
			want: []string{"omission fault:fs.slow is in no surface"},
		},
		{
			name: "an omission without a reason",
			change: func(target *Target[*register, belief]) {
				target.Uncovered[0].Why = ""
			},
			want: []string{"omission fuzz.disk.Close says no reason"},
		},
		{
			name: "an omission an operation covers",
			change: func(target *Target[*register, belief]) {
				target.Ops[0].Covers = append(target.Ops[0].Covers, "fuzz.disk.Close")
			},
			want: []string{"omission fuzz.disk.Close is covered by an operation"},
		},
		{
			// The methods of disk are on the pointer, so the plain type has
			// none and would demand nothing.
			name: "a surface with no names",
			change: func(target *Target[*register, belief]) {
				target.Covers = append(target.Covers, Interface[disk](), Faults("remote", nil))
			},
			want: []string{
				"surface fuzz.disk has no names",
				"surface fault:remote has no names",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target := covered()
			c.change(&target)

			mustHold(t, coverage(target), c.want...)
		})
	}
}

// uncoveredChild makes FuzzAnUncoveredTarget run in the process that expects
// it to fail.
const uncoveredChild = "FUZZ_TEST_UNCOVERED_TARGET"

func FuzzAnUncoveredTarget(f *testing.F) {
	if os.Getenv(uncoveredChild) == "" {
		f.Skip("run by TestFuzzChecksCoverage")
	}
	target := covered()
	target.Uncovered = nil

	Fuzz(f, target)
}

func TestFuzzChecksCoverage(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^FuzzAnUncoveredTarget$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), uncoveredChild+"=1")

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a target with an uncovered method passed:\n%s", out)
	}
	if want := "fuzz.disk.Close is covered by no operation"; !strings.Contains(string(out), want) {
		t.Errorf("output does not hold %q:\n%s", want, out)
	}
}
