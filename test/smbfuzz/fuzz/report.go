package fuzz

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Trace writes both sides' results, one step per line.
func Trace(res *Result) string {
	var b strings.Builder
	for _, s := range res.Steps {
		mark := "  "
		if s.Diverge != nil {
			mark = "!!"
		}
		fmt.Fprintf(&b, "%s %s\n", mark, s.Line)
		fmt.Fprintf(&b, "     ref: %s\n", replySummary(s.Ref))
		fmt.Fprintf(&b, "     dut: %s\n", replySummary(s.Dut))
		for _, n := range s.Notes {
			fmt.Fprintf(&b, "     note: %s\n", n)
		}
		if s.Diverge != nil {
			fmt.Fprintf(&b, "     DIVERGES on %s: ref %s, dut %s\n", s.Diverge.Field, s.Diverge.Ref, s.Diverge.Dut)
		}
	}
	return b.String()
}

func replySummary(r Reply) string {
	if r.Fatal != "" {
		return "FATAL " + r.Fatal
	}
	s := errName(r.Errno)
	o := r.Out
	if o == nil {
		return s
	}
	switch {
	case o.Stat != nil:
		st := o.Stat
		s += fmt.Sprintf(" %s size=%d nlink=%d perm=%04o blocks=%d", st.Type, st.Size, st.Nlink, st.Perm, st.Blocks)
	case o.Dir != nil:
		s += " " + fmtDir(o.Dir)
	case o.Hash != "":
		s += fmt.Sprintf(" n=%d sha=%s", o.N, o.Hash)
	case o.Lock != nil:
		s += fmt.Sprintf(" lock=%+v", *o.Lock)
	case o.Names != nil:
		s += " " + strings.Join(o.Names, ",")
	case o.Bytes != nil:
		s += " " + short(o.Bytes)
	case o.N != 0:
		s += fmt.Sprintf(" n=%d", o.N)
	}
	return s
}

// Report is what a failing round leaves behind (RFC 31 §11.4).
type Report struct {
	Round     uint64
	Gen       int
	Profile   string
	Auth      string
	Kernel    string
	Commit    string
	Stable    bool
	Sigs      []string
	Original  *Result
	Minimized *Minimized
}

// Write saves the report under dir/round-<N>/.
func (rp *Report) Write(dir string) (string, error) {
	d := filepath.Join(dir, fmt.Sprintf("round-%d", rp.Round))
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	var sum strings.Builder
	fmt.Fprintf(&sum, "round %d, generator %d, profile %s, auth %s\n", rp.Round, rp.Gen, rp.Profile, rp.Auth)
	fmt.Fprintf(&sum, "commit %s, kernel %s\n", rp.Commit, rp.Kernel)
	fmt.Fprintf(&sum, "signature: %s\n", rp.Original.Signature())
	if rp.Stable {
		fmt.Fprintf(&sum, "stable over %d runs\n", Replays)
	} else {
		fmt.Fprintf(&sum, "UNSTABLE: %s\n", strings.Join(rp.Sigs, " | "))
	}
	if rp.Original.DutLog != "" {
		fmt.Fprintf(&sum, "\nDittoFS was not running; log tail:\n%s\n", rp.Original.DutLog)
	}
	files := map[string]string{
		"original.plan":  rp.Original.Plan.Format(),
		"original.trace": Trace(rp.Original),
	}
	if m := rp.Minimized; m != nil {
		files["minimized.plan"] = m.Plan.Format()
		files["minimized.trace"] = Trace(m.Result)
		fmt.Fprintf(&sum, "minimised to %d steps in %d runs\n", len(m.Plan.Steps), m.Runs)
		if len(m.Leads) > 0 {
			fmt.Fprintf(&sum, "leads (other divergences met while minimising): %s\n", strings.Join(m.Leads, ", "))
		}
		fmt.Fprintf(&sum, "\nreplay: smbfuzz run --plan %s\n\n%s", filepath.Join(d, "minimized.plan"), m.Plan.Format())
	}
	files["summary.txt"] = sum.String()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return d, nil
}
