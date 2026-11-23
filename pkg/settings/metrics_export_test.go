package settings

// ReasonOf is reasonOf for the tests outside this package: the reason label
// a refusal counts err under, which is false when err wraps no row of the
// reasons table.
func ReasonOf(err error) (string, bool) {
	return reasonOf(err)
}

// ReasonNames are the reasons table's names, in the order a refusal that
// matches several takes its reason from.
func ReasonNames() []string {
	names := make([]string, len(reasons))

	for i, r := range reasons {
		names[i] = r.name
	}

	return names
}
