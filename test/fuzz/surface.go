package fuzz

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Surface is a set of names a target has to account for, each covered by an
// operation or omitted with a reason.
type Surface struct {
	label string
	names []string
}

// Interface is the exported methods of T, named "package.Type.Method". T is an
// interface, or a pointer to a type, for a component that implements none: the
// methods declared on a pointer are not the plain type's.
func Interface[T any]() Surface {
	t := reflect.TypeFor[T]()
	label := strings.TrimPrefix(t.String(), "*")

	var names []string
	for i := range t.NumMethod() {
		// An interface lists its unexported methods too.
		if m := t.Method(i); m.IsExported() {
			names = append(names, label+"."+m.Name)
		}
	}

	return Surface{
		label: label,
		names: names,
	}
}

// Faults is the fault points of one source, named "fault:source.point".
func Faults(source string, points []string) Surface {
	label := "fault:" + source

	names := make([]string, len(points))
	for i, point := range points {
		names[i] = label + "." + point
	}

	return Surface{
		label: label,
		names: names,
	}
}

// Omission is a name of a surface that no operation covers.
type Omission struct {
	Name string
	Why  string
}

// CheckCoverage fails the test when a name of the target's surfaces is neither
// covered by an operation nor omitted, or when a Covers or an Omission names
// what no surface has. A method added to a surface then fails the target until
// someone decides whether to fuzz it.
func CheckCoverage[R, M any](t testing.TB, target Target[R, M]) {
	t.Helper()

	if err := coverage(target); err != nil {
		t.Fatal(err)
	}
}

// coverage reports every problem at once, in the order of the declaration.
func coverage[R, M any](target Target[R, M]) error {
	var problems []error
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf("fuzz: "+target.Name+": "+format, args...))
	}

	var names []string
	for _, surface := range target.Covers {
		if len(surface.names) == 0 {
			report("surface %s has no names", surface.label)
		}
		names = append(names, surface.names...)
	}

	covered := map[string]bool{}
	for _, op := range target.Ops {
		for _, name := range op.Covers {
			if !slices.Contains(names, name) {
				report("operation %q covers %s, which no surface has", op.Name, name)
			}
			covered[name] = true
		}
	}

	omitted := map[string]bool{}
	for _, omission := range target.Uncovered {
		switch {
		case !slices.Contains(names, omission.Name):
			report("omission %s is in no surface", omission.Name)
		case covered[omission.Name]:
			report("omission %s is covered by an operation", omission.Name)
		case omission.Why == "":
			report("omission %s says no reason", omission.Name)
		}
		omitted[omission.Name] = true
	}

	for _, name := range names {
		if !covered[name] && !omitted[name] {
			report("%s is covered by no operation and no omission", name)
		}
	}

	return errors.Join(problems...)
}
