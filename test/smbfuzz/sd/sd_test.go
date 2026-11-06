package sd

import (
	"bytes"
	"testing"
)

func testResolver() *Resolver {
	dom := MustSID("S-1-5-21-1-2-3")
	return &Resolver{
		Domain: dom,
		Accounts: map[string]SID{
			"alice": MustSID("S-1-5-21-1-2-3-1104"),
			"bob":   MustSID("S-1-5-21-1-2-3-1105"),
			"devs":  MustSID("S-1-5-21-1-2-3-1102"),
		},
	}
}

func TestTextRoundTrip(t *testing.T) {
	r := testResolver()
	for _, text := range []string{
		"O:aliceG:devsD:P(A;OICI;FA;;;alice)(D;;FR;;;bob)(A;;0x1200a9;;;WD)",
		"O:bobG:DUD:NO_ACCESS_CONTROL",
		"D:",
		"D:PAI",
		"D:AIS:P(AU;SAFA;FW;;;bob)(ML;;0x1;;;ME)",
		"O:ghostG:unix-1000D:(A;0x2b;0x200;;;CO)(D;IO;GA;;;CG)",
		"O:S-1-5-21-9-9-9-500D:(A;;FA;;;dom-1234)",
		"D:(0x05;;raw:0102;;;)",
	} {
		d, err := r.Parse(text)
		if err != nil {
			t.Fatalf("parse %q: %v", text, err)
		}
		bin := d.Marshal()
		d2, err := Unmarshal(bin)
		if err != nil {
			t.Fatalf("unmarshal %q: %v", text, err)
		}
		if !bytes.Equal(d2.Marshal(), bin) {
			t.Errorf("%q: binary round trip differs", text)
		}
		want := text
		if text == "O:S-1-5-21-9-9-9-500D:(A;;FA;;;dom-1234)" {
			want = "O:local-500D:(A;;FA;;;dom-1234)"
		}
		if got := r.Format(d2); got != want {
			t.Errorf("format: got %q, want %q", got, want)
		}
	}
}

func TestNullAndEmptyDACLDiffer(t *testing.T) {
	r := testResolver()
	null, _ := r.Parse("D:NO_ACCESS_CONTROL")
	empty, _ := r.Parse("D:")
	if bytes.Equal(null.Marshal(), empty.Marshal()) {
		t.Fatal("a null DACL and an empty DACL marshal the same")
	}
	n2, _ := Unmarshal(null.Marshal())
	if !n2.NullDACL() {
		t.Fatal("null DACL lost in round trip")
	}
	e2, _ := Unmarshal(empty.Marshal())
	if e2.NullDACL() || e2.DACL == nil || len(e2.DACL.ACEs) != 0 {
		t.Fatal("empty DACL lost in round trip")
	}
}
