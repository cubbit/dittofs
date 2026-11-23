package settings

// ErrNoHoldsForTest is errNoHolds for the tests outside this package, so they
// can assert that a bound change with no holds is refused by the missing
// check's own error, never as a bound refusal.
var ErrNoHoldsForTest = errNoHolds
