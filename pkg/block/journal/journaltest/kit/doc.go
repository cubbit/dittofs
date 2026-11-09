// Package kit holds the journal-free components the journal's tests are built
// on: the storage seam with its fault rules, holds and crashes, the test
// clock and the sim gate.
//
// It imports no package that imports the journal's public package, so the
// in-package tests of the packages the journal itself imports can use it
// without an import cycle. journaltest holds the components that need the
// public package.
package kit
