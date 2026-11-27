// Package keyfile declares Generate, which is not a use, and re-exports it by
// its bare name, which is: the rule names the file.
package keyfile

func Generate(string, bool) ([]byte, error) { return nil, nil }

var Gen = Generate
