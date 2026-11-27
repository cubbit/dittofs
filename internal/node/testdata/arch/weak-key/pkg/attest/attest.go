// Package attest is a production package whose name ends in test, which
// exempts its reference below; a binary links it, so the rule names it.
// keyprovider is not imported, so this module resolves on its own.
package attest

func Key() ([]byte, error) { return keyprovider.GenerateWeakKeyFile("p") }
