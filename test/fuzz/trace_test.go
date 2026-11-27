package fuzz

import (
	"reflect"
	"strings"
	"testing"
)

func TestTraceRoundTrip(t *testing.T) {
	target := toy()

	// The bytes of every parameter kind, one of them two bytes wide.
	program := Decode(target, []byte{
		0, 1, 2, 3, 4,
		192, 1, 0, 0, 0,
		240, 0, 0, 0, 0,
	})
	want := "#00 write(file=1, off=1190, pattern=0x04)\n" +
		"#01 fault(kind=fail)\n" +
		"#02 crash()\n"
	if got := program.String(); got != want {
		t.Fatalf("trace:\n%q\nwant:\n%q", got, want)
	}

	for seed := range uint64(500) {
		program := Decode(target, random(seed, int(seed)))

		parsed, err := Parse(target, program.String())
		if err != nil {
			t.Fatalf("seed %d: parse of the printed trace: %v", seed, err)
		}

		if !reflect.DeepEqual(parsed, program) {
			t.Fatalf("seed %d: parsed trace:\n%swant:\n%s", seed, parsed, program)
		}

		if got := Decode(target, Seed(target, program.String())); !reflect.DeepEqual(got, program) {
			t.Fatalf("seed %d: the encoded seed decodes to:\n%swant:\n%s", seed, got, program)
		}
	}

	t.Run("a hand written seed", func(t *testing.T) {
		text := `
			write(file=2,off=7, pattern=0x3a)

			#07 sync( file = 0 )   -> ErrIO
			crash()
		`
		want := "#00 write(file=2, off=7, pattern=0x3a)\n" +
			"#01 sync(file=0)\n" +
			"#02 crash()\n"
		if got := Decode(target, Seed(target, text)).String(); got != want {
			t.Fatalf("seed decodes to:\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("a seed takes the smallest bytes", func(t *testing.T) {
		// Writes own opcodes 0 to 127 and crash 240 to 255. Offset 1197 is
		// value 171 of 300.
		want := []byte{
			0, 2, 171, 0, 5,
			240, 0, 0, 0, 0,
		}
		got := Seed(target, "write(file=2, off=1197, pattern=0x05)\ncrash()")
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed = %v, want %v", got, want)
		}
	})
}

func TestParseRefusesWhatNoInputDecodesTo(t *testing.T) {
	short := toy()
	short.MaxSteps = 2

	cases := []struct {
		text string
		want string // a part of the error
	}{
		{
			text: "chmod(file=1)",
			want: `no operation "chmod"`,
		},
		{
			text: "sync(file=3)",
			want: `file="3"`,
		},
		{
			text: "fault(kind=melt)",
			want: `kind="melt"`,
		},
		{
			text: "sync(off=0)",
			want: `want parameter "file"`,
		},
		{
			text: "sync()",
			want: "0 parameters, want 1",
		},
		{
			text: "crash(file=0)",
			want: "1 parameters, want 0",
		},
		{
			text: "crash",
			want: "not a call",
		},
		{
			text: "crash() and more",
			want: "not a call",
		},
		{
			text: "crash()\ncrash()\ncrash()",
			want: "more than 2 steps",
		},
	}
	for _, c := range cases {
		_, err := Parse(short, c.text)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%q): error %v, want one holding %q", c.text, err, c.want)
		}
	}

	defer func() {
		if recover() == nil {
			t.Errorf("Seed accepted a text Parse refuses, want a panic")
		}
	}()

	Seed(short, "chmod(file=1)")
}
