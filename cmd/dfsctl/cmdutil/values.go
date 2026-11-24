package cmdutil

import (
	"encoding/json"
	"fmt"
	"strings"
)

// JSONValue is s as a setting's value: s itself when it is JSON, such as
// 8080, true or {"a":1}, and otherwise s as a JSON string, so a plain word
// needs no quotes. A string that would read as JSON, such as "true", is
// given quoted: '"true"'.
func JSONValue(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s) // a string always encodes
	return b
}

// Settings is pairs, each key=value, as settings by key, each value read by
// JSONValue, refusing a pair with no '=' or no key.
func Settings(pairs []string) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("setting %q: want key=value", p)
		}
		out[k] = JSONValue(v)
	}
	return out, nil
}
