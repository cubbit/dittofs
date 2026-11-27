package settings

import (
	"errors"
	"slices"

	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// unknownComponent is the component label of a refusal whose key no section
// claims, the one fixed value an unknown key takes.
const unknownComponent = "unknown"

// reasons is the closed set of the reason label, one row for each error
// Check and Read refuse with, the name spelled as RFC 25 §2.1 names things,
// lower-case words joined by underscores. The table is explicit, never a
// name derived from a Go identifier, so renaming a sentinel does not rename a
// dashboard series, and its order is the one a refusal that matches several
// names takes its reason from.
var reasons = []struct {
	err  error
	name string
}{
	{ErrUnknownKey, "unknown_key"},
	{ErrWrongScope, "wrong_scope"},
	{ErrNewerSchema, "newer_schema"},
	{ErrForbidden, "forbidden"},
	{ErrRequired, "required"},
	{ErrInvalid, "invalid"},
	{ErrBound, "bound"},
	{ErrSecretRef, "secret_ref"},
	{ErrFormatVersion, "format_version"},
	{ErrUnproven, "unproven"},
}

// reasonOf is err's name in the reason label, the first row of the reasons
// table err wraps, read as errors.Is reads a joined error: a cause's position
// in the join does not pick the reason, the table's order does. An error
// wrapping none of them, a store error, is no refusal and names none.
func reasonOf(err error) (string, bool) {
	for _, r := range reasons {
		if errors.Is(err, r.err) {
			return r.name, true
		}
	}

	return "", false
}

// refusalSeries are the two counters a table's refusals raise, RFC 13 §8's
// series: config_refused_total by component and reason, and
// config_bound_refusals_total beside it, the bound gate's refusals among the
// writes Check refused. WithMetrics gives a table's sections one of these, so
// Read and Check find the series on the section they already hold, with no
// global, no registry lookup and no value in a context (RFC 25 §8's rule,
// which pkg/settings follows by analogy).
type refusalSeries struct {
	refused *prometheus.CounterVec
	bound   prometheus.Counter
}

// WithMetrics returns a copy of t whose sections carry the two refusal
// series, registered under m: config_refused_total counts a refusal once per
// refused write in Check and once per refused section per pass in Read, and
// config_bound_refusals_total, bound at zero here since it holds no label,
// counts the bound gate's refusals of changes. The receiver is not changed:
// a table this was never called on counts nothing and never panics. Called
// twice on one registry, the second copy shares the first's series.
func (t Table) WithMetrics(m *metrics.Registry) Table {
	s := &refusalSeries{
		refused: m.Counter(metrics.Spec{
			Name: "config_refused_total",
			Help: "Records refused, by component and reason. Read counts a " +
				"refused section once per pass, so a section that stays refused " +
				"counts again on every read: it counts passes, not refusals. " +
				"Check counts once per refused write.",
			Labels: []string{"component", "reason"},
		}),
		bound: m.Counter(metrics.Spec{
			Name: "config_bound_refusals_total",
			Help: "Writes refused because the setting is bound and content exists.",
		}).WithLabelValues(),
	}

	out := slices.Clone(t)
	for i := range out {
		out[i].series = s
	}

	return out
}

// count raises the refusal series for err, the refusal Check answered for a
// write whose lookup the table's index resolved to sec, nil when no section
// claims its key: the component label is sec's declared prefix, or
// unknownComponent when none does, never part of the key, which an operator
// chose and a label may not hold (RFC 25 §2.4). An error wrapping no row of
// the reasons table, a store error, counts nothing. The series are found on
// sec, or on the table's first section when the write claimed none; a table
// WithMetrics never bound counts nothing and never panics. A bound refusal
// raises config_bound_refusals_total too.
func (t Table) count(sec *Section, err error) {
	var s *refusalSeries

	switch {
	case sec != nil:
		s = sec.series
	case len(t) > 0:
		s = t[0].series
	}

	component := unknownComponent
	if sec != nil {
		component = sec.prefix
	}

	s.raise(component, err)
}

// raise counts one refusal Read answered for this section, err its entry's
// refusal, the errors of every refused instance joined: counted once, under
// the section's prefix and the first reason in the reasons table's order the
// join wraps, so a section refused in several instances, for several causes,
// counts once. Read never raises the bound series: a bound refusal answers a
// write, which Read does not judge. A table WithMetrics never bound counts
// nothing, raise reading a nil series.
func (sec *Section) raise(err error) {
	sec.series.raise(sec.prefix, err)
}

// raise counts err's refusal under component: config_refused_total under the
// component and reason labels, and config_bound_refusals_total too when err
// is a bound refusal. A nil series, a table WithMetrics never bound, counts
// nothing, and an error matching no row of the reasons table counts nothing.
func (s *refusalSeries) raise(component string, err error) {
	if s == nil {
		return
	}

	reason, ok := reasonOf(err)
	if !ok {
		return
	}

	s.refused.WithLabelValues(component, reason).Inc()

	if errors.Is(err, ErrBound) {
		s.bound.Inc()
	}
}
