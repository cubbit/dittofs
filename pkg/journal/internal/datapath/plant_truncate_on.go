//go:build journal_plant_truncate

package datapath

// plantTruncateBug plants a known bug in Truncate's removal marker, so a
// test can show the model's many-seed run finds one: the marker reaches one
// byte past the size the caller named, so the byte at the size itself stays
// held after the truncate.
const plantTruncateBug = true
