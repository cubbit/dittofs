package journal

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cubbit/dittofs/test/harness/known"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// The repository root, from this package's directory, and the one known file
// classified here.
const (
	suitesRoot   = "../.."
	journalKnown = "test/journal/known.yaml"
)

// openOwners maps each issue a known failure may carry to what it names. The
// test runs with no network and no second source of truth, so this set is
// itself what enforces the open-owner rule: it holds the open epics and the
// dedicated issues that still owe a known failure, and a row carrying any
// other issue fails on it.
//
// decision: the change that lands 7b or 9b removes that epic's issue here and
// clears its rows from known.yaml in the same change — nothing else fires
// when an issue closes. A row 7b or 9b turns green fails the run through the
// harness's own rule that a listed test passing fails it, while a row the
// epics leave red would name a closed issue, and that leak is what this
// marker points at.
var openOwners = map[string]string{
	"#3596": "epic 7b",
	"#3379": "epic 9b",
	"#3395": "epic 12 (T12.2)",
	"#4859": "the open's loss records for synced damage",
	"#4860": "the record-less take's return to the pool",
	"#4861": "the identity refusal and error",
	"#4862": "the same-process reopen's loss record",
	"#4863": "the open's removal markers",
}

// openOwnerList names each open owner with its issue, so a failure that
// rejects a row's owner says what the row may carry instead.
var openOwnerList = func() string {
	var out []string
	for _, issue := range slices.Sorted(maps.Keys(openOwners)) {
		out = append(out, openOwners[issue]+" ("+issue+")")
	}
	return strings.Join(out, ", ")
}()

// classOwner maps the call a `stub: <Call>` message names to the open epic
// that writes it. Repack, Forget and repack_copy are landed code on this
// branch, so a row still failing on one is a defect there and the burn-down
// owns it.
var classOwner = map[string]string{
	"MarkOffloaded":  "#3379",
	"Offload":        "#3379",
	"Release":        "#3379",
	"Hold":           "#3379",
	"Stamp":          "#3379",
	"Restamp":        "#3596",
	"Inspect.Verify": "#3596",
	"Forget":         "#3395",
	"Repack":         "#3395",
	"repack_copy":    "#3395",
}

// stubClass names the call a `stub: ` message stops at: the identifier up to
// the first character an identifier cannot hold, so `stub: the repack` reads
// as the call `the`.
var stubClass = regexp.MustCompile(`^stub: ([A-Za-z_.]+)`)

// testOwner maps each test whose message names no call to the owner its
// reason names: the recovery rows of the epic that owes them, and the rows on
// code that has landed, whose defect the burn-down owns. A row that stopped at
// a stub runs further when the call lands and fails on its own assertion, so
// the tables, never its issue, carry it across: retagging is by message.
//
// decision: one message can span owners, so the message cannot route these
// rows. The ceiling is that a new row has no entry until someone adds it, and
// the test fails on it.
var testOwner = map[string]string{
	"TestRFC1A_SinceYieldsOver":                               "#4863",
	"TestRFC1A_SyncedLossMasksSuperseded":                     "#3395",
	"TestRFC1B_HeadroomIsAllocated":                           "#3395",
	"TestRFC1B_ReadOnlyProbe":                                 "#3395",
	"TestRFC1B_ReadOnlyStopsTheDrain":                         "#3395",
	"TestRFC1B_RecordsWithoutBytesAtTheLimit":                 "#3395",
	"TestRFC1C_IncrementalReport":                             "#3395",
	"TestRFC1C_LossSurvivesRestart":                           "#4862",
	"TestRFC1D_MetricsAreReachable":                           "#3395",
	"TestRFC28A_AZeroSlotIsNotTheEnd":                         "#3596",
	"TestRFC28A_FooterIsNotAuthoritative":                     "#3395",
	"TestRFC28B_SegmentReclaimable":                           "#3395",
	"TestRFC28C_BadCopyDoesNotMaskAGoodOne":                   "#3395",
	"TestRFC28C_SyncedDamageIsNotATornTail":                   "#4859",
	"TestRFC28C_SparesAreReAdopted":                           "#4860",
	"TestRFC28C_SpareIsWholeOrAbsent/durable_temporary_entry": "#3395",
	"TestRFC28C_RetiredSegmentsAreRecycled":                   "#3395",
	"TestRFC28C_RecycledSegmentsOldCatalogIsNeverRead":        "#3395",
	"TestRFC28C_JournalGone":                                  "#4861",
	"TestRFC28C_InterruptedInit":                              "#4861",
	"TestDamage_LostWrites/failed_unlink":                     "#3395",
	"TestEdge_ZeroLengthWrite":                                "#3395",
	"TestEdge_RecordsWithoutBytesAtMaximum":                   "#3395",
	"TestEdge_FilesystemENOSPCIsNotErrNoSpace":                "#3395",
}

// owner returns the open issue that owns e's failure, and whether any table
// holds it: the call its message stops at, a recovery message that names no
// record, or its test.
func owner(e known.Entry) (string, bool) {
	if m := stubClass.FindStringSubmatch(e.Message); m != nil {
		issue, ok := classOwner[m[1]]
		return issue, ok
	}
	if strings.HasSuffix(e.Message, "names no record") {
		return "#3596", true
	}
	issue, ok := testOwner[e.Test]
	return issue, ok
}

// fixState says how the journal's ownership tables treat a row's fix value:
// a planned row owes its failure to an open epic, a wontfix row is exempt,
// and any other value is unknown. The parse layer refuses every other value
// already, so an unknown one reaching these tables means the two layers have
// drifted apart, and the classifier reports it rather than exempting the row.
// The value matches exactly — no trim, no case folding — since only a
// spelling the parser accepted can reach here.
func fixState(fix string) (exempt, unknown bool) {
	switch fix {
	case "planned":
		return false, false
	case "wontfix":
		return true, false
	default:
		return false, true
	}
}

// TestFixState holds the fix classifier to exactly two states: only a
// wontfix row is exempt from owning its failure, and only a planned row
// owns one.
func TestFixState(t *testing.T) {
	for _, c := range []struct{ fix, want string }{
		{"planned", "owned"},
		{"wontfix", "exempt"},
		{"typo", "unknown"},
		{"", "unknown"},
	} {
		exempt, unknown := fixState(c.fix)
		got := "owned"
		if exempt {
			got = "exempt"
		}
		if unknown {
			got = "unknown"
		}
		if got != c.want {
			t.Errorf("fix %q: got %s, want %s", c.fix, got, c.want)
		}
	}
}

// TestKnownFilesClassified holds the journal's known failures to one owner.
// Every row of every known file the registry lists names its failure, since a
// bare `stub:` matches any stub failure and the registry grades by Contains.
// Each planned row of the journal's own file names the open epic that owns
// its failure, and no row of trace.yaml is owned by a superseded epic.
func TestKnownFilesClassified(t *testing.T) {
	reg, err := registry.Load(suitesRoot)
	if err != nil {
		t.Fatalf("test/suites.yaml: %v", err)
	}
	if len(reg.Known) == 0 {
		t.Fatal("test/suites.yaml lists no known file")
	}

	// The suites and options that name each file, to name in a failure.
	by := map[string]string{}
	for _, s := range reg.Suites {
		for _, u := range reg.Units(s) {
			if u.Known != "" {
				by[u.Known] += " " + u.Name
			}
		}
	}

	for _, path := range slices.Sorted(maps.Keys(reg.Known)) {
		for _, e := range reg.Known[path] {
			// decision: the bare-message check compares after trim, not by
			// Contains like the registry, because a message that merely
			// mentions `stub:` names one specific stub and so is not bare, and
			// `stub:Offload` without the trailing space is a different message.
			// The ceiling is that a new bare message worded differently, not
			// one of the two exact texts, passes unnamed; widen only when such
			// a row exists, never to silence a naming failure.
			if m := strings.TrimSpace(e.Message); m == "stub:" || m == "journal: not implemented" {
				t.Errorf("%s (%s): %s: got %q in %s, want a message that names its failure",
					strings.TrimSpace(by[path]), path, e.Test, m, path)
			}
		}
	}

	// decision: the journal's file alone is classified. The engine's is
	// another plan's to re-point, overturned if it is still red once its
	// issues close, and then it joins by name. Every row of it names its
	// failure — here, not in the bare check above, because an entry that
	// stands for any output of the test it names is the other files' norm.
	rows := map[string][]known.Entry{}
	for _, e := range reg.Known[journalKnown] {
		rows[e.Test] = append(rows[e.Test], e)
		if strings.TrimSpace(e.Message) == "" {
			t.Errorf("%s: %s has no message, so it stands for any output of the test; want one that names its failure",
				journalKnown, e.Test)
			continue
		}
		exempt, unknown := fixState(e.Fix)
		if unknown {
			t.Errorf("%s: %s carries fix: %q, which is neither planned nor wontfix, so it can neither own its failure nor be exempt from owning one",
				journalKnown, e.Test, e.Fix)
			continue
		}
		if exempt {
			continue // a wontfix row carries no owner
		}
		if _, ok := openOwners[e.Issue]; !ok {
			t.Errorf("%s: %s names %q, which is not an open owner of a known failure; carry one of %s",
				journalKnown, e.Test, e.Issue, openOwnerList)
			continue
		}
		want, ok := owner(e)
		if !ok {
			if m := stubClass.FindStringSubmatch(e.Message); m != nil {
				t.Errorf("%s: %s: the message %q names the call %s, which no owner table holds",
					journalKnown, e.Test, e.Message, m[1])
			} else {
				t.Errorf("%s: %s: the message %q names no call, and the test names no owner",
					journalKnown, e.Test, e.Message)
			}
			continue
		}
		if want != e.Issue {
			t.Errorf("%s: %s: the failure the message %q names is %s's (%s), the entry names %s",
				journalKnown, e.Test, e.Message, openOwners[want], want, e.Issue)
		}
	}

	// The test-name table is routing machinery, so an entry that no row would
	// ever reach fails loudly: a row whose row left the file in the change
	// that turned it green, or one whose message is routed by its class
	// alone.
	for _, name := range slices.Sorted(maps.Keys(testOwner)) {
		rs := rows[name]
		if len(rs) == 0 {
			t.Errorf("%s: %s has no row, but the test-name table still routes it; remove the entry with the row",
				journalKnown, name)
			continue
		}
		consulted := false
		for _, e := range rs {
			if stubClass.FindStringSubmatch(e.Message) == nil {
				consulted = true
			}
		}
		if !consulted {
			t.Errorf("%s: %s is routed by its message's class alone, so the test-name table's entry for it is never consulted",
				journalKnown, name)
		}
	}

	// trace.yaml's owner field is free text, so a superseded epic is rejected
	// by name.
	//
	// decision: only the superseded epic this re-point removes is refused.
	// The ledger's other owners map whole RFC rows, not known failures, and
	// trace_test.go holds each to a reason.
	var trace map[string]traceEntry
	f, err := os.Open("trace.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&trace); err != nil {
		t.Fatalf("trace.yaml: %v", err)
	}
	for _, row := range slices.Sorted(maps.Keys(trace)) {
		if strings.Contains(trace[row].Owner, "epic 11") {
			t.Errorf("trace.yaml: %s names the owner %q, a superseded epic, want the work that owns it now", row, trace[row].Owner)
		}
	}
}
