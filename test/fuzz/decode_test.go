package fuzz

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

var (
	files   = []int{0, 1, 2}
	offsets = wide(300) // more than 256 values, so it takes two bytes
	faults  = []string{"torn", "fail", "delay"}
)

func wide(n int) []int {
	values := make([]int, n)
	for i := range values {
		values[i] = 7 * i
	}

	return values
}

// toy declares operations with every parameter width. Its weights add to 16, so
// each operation owns a whole number of the 256 opcodes.
func toy() Target[struct{}, struct{}] {
	return Target[struct{}, struct{}]{
		Name: "toy",
		Ops: []Op[struct{}, struct{}]{
			{
				Name:   "write",
				Weight: 8,
				Params: []Param{
					Enum("file", files...),
					Choice("off", offsets...),
					Pattern("pattern"),
				},
			},
			{
				Name:   "read",
				Weight: 4,
				Params: []Param{
					Enum("file", files...),
					Choice("off", offsets...),
				},
			},
			{
				Name:   "fault",
				Weight: 2,
				Params: []Param{
					Named("kind", faults...),
				},
			},
			{
				Name:   "sync",
				Weight: 1,
				Params: []Param{
					Enum("file", files...),
				},
			},
			{
				Name: "crash", // no weight: it counts as 1
			},
		},
	}
}

// toyStride is one opcode byte and the four bytes of write's parameters.
const toyStride = 5

func random(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, 1))
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(r.Uint32())
	}

	return data
}

func opcodesPerOp[R, M any](target Target[R, M]) map[string]int {
	count := map[string]int{}
	for b := range 256 {
		for _, step := range Decode(target, []byte{byte(b)}) {
			count[step.Name]++
		}
	}

	return count
}

func checkArgs(t *testing.T, step Step) {
	t.Helper()

	switch step.Name {
	case "write", "read":
		if f := step.Args.Int("file"); !slices.Contains(files, f) {
			t.Fatalf("%s: file = %d, outside its domain %v", step.Name, f, files)
		}

		if off := step.Args.Int("off"); !slices.Contains(offsets, off) {
			t.Fatalf("%s: off = %d, outside its domain", step.Name, off)
		}

		if step.Name == "write" {
			if p := step.Args.Int("pattern"); p < 0 || p > 255 {
				t.Fatalf("write: pattern = %d, outside 0 to 255", p)
			}
		}
	case "fault":
		if k := step.Args.Named("kind"); !slices.Contains(faults, k) {
			t.Fatalf("fault: kind = %q, outside its domain %v", k, faults)
		}
	}
}

func TestDecodeAnyBytes(t *testing.T) {
	target := toy()

	inputs := [][]byte{nil, {}}
	for n := 1; n <= 3*toyStride; n++ {
		inputs = append(inputs, random(uint64(n), n))
	}

	for seed := range uint64(500) {
		inputs = append(inputs, random(seed, int(seed)))
	}

	for _, data := range inputs {
		program := Decode(target, data)

		want := min((len(data)+toyStride-1)/toyStride, 64)
		if len(program) != want {
			t.Fatalf("%d bytes decoded to %d steps, want %d", len(data), len(program), want)
		}

		for _, step := range program {
			if step.Op < 0 || step.Op >= len(target.Ops) || target.Ops[step.Op].Name != step.Name {
				t.Fatalf("step names operation %d %q, which the table does not hold", step.Op, step.Name)
			}

			checkArgs(t, step)
		}

		padded := append(slices.Clone(data), make([]byte, (toyStride-len(data)%toyStride)%toyStride)...)
		if !reflect.DeepEqual(Decode(target, padded), program) {
			t.Fatalf("%d bytes: a short last step is not read as padded with zeros", len(data))
		}
	}

	t.Run("max steps", func(t *testing.T) {
		short := toy()
		short.MaxSteps = 3

		if got := len(Decode(short, random(1, 100))); got != 3 {
			t.Fatalf("MaxSteps 3: decoded %d steps", got)
		}
	})

	t.Run("opcodes follow the weights", func(t *testing.T) {
		want := map[string]int{
			"write": 128,
			"read":  64,
			"fault": 32,
			"sync":  16,
			"crash": 16,
		}

		if got := opcodesPerOp(target); !reflect.DeepEqual(got, want) {
			t.Fatalf("opcodes per operation = %v, want %v", got, want)
		}
	})

	t.Run("a rare operation keeps one opcode", func(t *testing.T) {
		skewed := toy()
		skewed.Ops = []Op[struct{}, struct{}]{
			{
				Name:   "often",
				Weight: 100000,
			},
			{
				Name:   "rare",
				Weight: 1,
			},
		}

		want := map[string]int{
			"often": 255,
			"rare":  1,
		}

		if got := opcodesPerOp(skewed); !reflect.DeepEqual(got, want) {
			t.Fatalf("opcodes per operation = %v, want %v", got, want)
		}
	})
}

// changed names the parameters two steps of one operation differ in.
func changed(target Target[struct{}, struct{}], a, b Step) []string {
	var names []string
	for _, p := range target.Ops[a.Op].Params {
		if a.Args.Int(p.name) != b.Args.Int(p.name) {
			names = append(names, p.name)
		}
	}

	return names
}

func TestOneByteOneOperation(t *testing.T) {
	target := toy()
	const steps = 6

	opcodeChanges, paramChanges := 0, 0
	for seed := range uint64(20) {
		data := random(seed, steps*toyStride)
		before := Decode(target, data)
		if len(before) != steps {
			t.Fatalf("decoded %d steps, want %d", len(before), steps)
		}

		for at := range data {
			for delta := 1; delta < 256; delta++ {
				mutated := slices.Clone(data)
				mutated[at] += byte(delta)

				after := Decode(target, mutated)
				if len(after) != steps {
					t.Fatalf("byte %d changed: decoded %d steps, want %d", at, len(after), steps)
				}

				for i := range after {
					if reflect.DeepEqual(before[i], after[i]) {
						continue
					}

					if i != at/toyStride {
						t.Fatalf("byte %d changed step %d, outside step %d", at, i, at/toyStride)
					}

					if at%toyStride == 0 {
						opcodeChanges++
						continue
					}

					if before[i].Op != after[i].Op {
						t.Fatalf("parameter byte %d changed the operation of step %d", at, i)
					}

					if names := changed(target, before[i], after[i]); len(names) != 1 {
						t.Fatalf("parameter byte %d changed parameters %v, want one", at, names)
					}

					paramChanges++
				}
			}
		}
	}

	// A decoder that ignores its input would pass every check above.
	if opcodeChanges == 0 || paramChanges == 0 {
		t.Fatalf("opcode changes = %d and parameter changes = %d, want both above zero", opcodeChanges, paramChanges)
	}
}

func TestDecodeRefusesAnOversizedDeclaration(t *testing.T) {
	refused := func(what string, declare func()) {
		t.Helper()

		defer func() {
			t.Helper()

			if recover() == nil {
				t.Errorf("%s: accepted, want a refusal", what)
			}
		}()

		declare()
	}

	refused("a choice of 65537 values", func() {
		Choice("off", wide(65537)...)
	})
	refused("an enum of 257 values", func() {
		Enum("file", wide(257)...)
	})
	refused("257 names", func() {
		Named("kind", make([]string, 257)...)
	})
	refused("an empty domain", func() {
		Choice("off")
	})
	refused("257 operations", func() {
		Decode(Target[struct{}, struct{}]{Ops: make([]Op[struct{}, struct{}], 257)}, nil)
	})
	refused("no operation", func() {
		Decode(Target[struct{}, struct{}]{}, nil)
	})
}
