// Package keyprovider declares GenerateWeakKeyFile, which is not a use, and
// re-exports it by its bare name, which is: the rule names the file.
package keyprovider

func GenerateWeakKeyFile(string) ([]byte, error) { return nil, nil }

var Weak = GenerateWeakKeyFile
