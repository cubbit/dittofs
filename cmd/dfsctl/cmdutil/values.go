package cmdutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// JSONValue is s as a setting's value: s itself when it is JSON, such as
// 8080, true or {"a":1}, and otherwise s as a JSON string, so a plain word
// needs no quotes. A string that would read as JSON, such as "true", is
// given quoted: '"true"'. A bare null is refused: it would be written as
// the value rather than remove the setting, which --unset and
// 'dfsctl setting unset' do.
func JSONValue(s string) (json.RawMessage, error) {
	if strings.TrimSpace(s) == "null" {
		return nil, errors.New("null is no value; remove a setting with --unset or 'dfsctl setting unset', or give the string as '\"null\"'")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s), nil
	}
	b, _ := json.Marshal(s) // a string always encodes
	return b, nil
}

// Settings is pairs, each key=value, as settings by key, each value read by
// JSONValue, refusing a pair with no '=' or no key, or one JSONValue refuses.
func Settings(pairs []string) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("setting %q: want key=value", p)
		}
		jv, err := JSONValue(v)
		if err != nil {
			return nil, fmt.Errorf("setting %q: %w", p, err)
		}
		out[k] = jv
	}
	return out, nil
}
