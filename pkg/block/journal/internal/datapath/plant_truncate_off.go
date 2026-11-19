//go:build !journal_plant_truncate

package datapath

// plantTruncateBug, when set, plants a known bug in Truncate's removal
// marker, so a test can show the model's many-seed run finds one. Only a
// build with the journal_plant_truncate tag sets it; everywhere else it is
// false and the branch it guards compiles away.
const plantTruncateBug = false
