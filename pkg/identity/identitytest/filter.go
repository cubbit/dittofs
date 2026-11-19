package identitytest

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// filter is a parsed RFC 4515 search filter.
type filter struct {
	op    byte     // '&', '|', '!', '=' (equality), '*' (presence) or 's' (substrings)
	subs  []filter // the operands of '&', '|' and '!'
	attr  string
	value []byte   // equality's value
	parts [][]byte // substrings' pieces: initial, any..., final; an empty first or last is absent
}

// parseFilter parses s, which must be one whole filter. It reads equality,
// presence, substrings, and, or and not, with \xx escapes, and refuses
// anything else, approximate, ordering and extensible matches among it, as an
// error rather than an answer the in-memory directory cannot give.
func parseFilter(s string) (filter, error) {
	f, rest, err := parseOne(s)
	if err != nil {
		return filter{}, fmt.Errorf("filter %q: %w", s, err)
	}
	if rest != "" {
		return filter{}, fmt.Errorf("filter %q: %q follows the filter", s, rest)
	}
	return f, nil
}

func parseOne(s string) (filter, string, error) {
	if !strings.HasPrefix(s, "(") {
		return filter{}, "", errors.New("a filter does not start with (")
	}
	s = s[1:]
	if s == "" {
		return filter{}, "", errors.New("unterminated filter")
	}
	var f filter
	switch s[0] {
	case '&', '|':
		f.op = s[0]
		s = s[1:]
		for strings.HasPrefix(s, "(") {
			sub, rest, err := parseOne(s)
			if err != nil {
				return filter{}, "", err
			}
			f.subs = append(f.subs, sub)
			s = rest
		}
	case '!':
		sub, rest, err := parseOne(s[1:])
		if err != nil {
			return filter{}, "", err
		}
		f.op, f.subs, s = '!', []filter{sub}, rest
	default:
		end := strings.IndexByte(s, ')')
		if end < 0 {
			return filter{}, "", errors.New("unterminated filter")
		}
		item, err := parseItem(s[:end])
		if err != nil {
			return filter{}, "", err
		}
		return item, s[end+1:], nil
	}
	if !strings.HasPrefix(s, ")") {
		return filter{}, "", errors.New("unterminated filter")
	}
	return f, s[1:], nil
}

// parseItem parses a simple, presence or substrings item, the text between
// its parentheses. An item holds no unescaped ( or ), so its end is the first
// ) after it.
func parseItem(s string) (filter, error) {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return filter{}, fmt.Errorf("item %q has no attribute and =", s)
	}
	attr, raw := s[:eq], s[eq+1:]
	if strings.ContainsAny(attr, "~<>:(\\*") || strings.TrimSpace(attr) != attr {
		return filter{}, fmt.Errorf("item %q: approximate, ordering and extensible matches, and attribute %q, are not read", s, attr)
	}
	if raw == "*" {
		return filter{op: '*', attr: attr}, nil
	}
	pieces := strings.Split(raw, "*")
	parts := make([][]byte, len(pieces))
	for i, p := range pieces {
		v, err := unescape(p)
		if err != nil {
			return filter{}, fmt.Errorf("item %q: %w", s, err)
		}
		parts[i] = v
	}
	if len(parts) == 1 {
		return filter{op: '=', attr: attr, value: parts[0]}, nil
	}
	for _, p := range parts[1 : len(parts)-1] {
		if len(p) == 0 {
			return filter{}, fmt.Errorf("item %q holds two * in a row", s)
		}
	}
	return filter{op: 's', attr: attr, parts: parts}, nil
}

// unescape reads an assertion value: \xx is the byte xx, and an unescaped
// NUL, ( , ) or \ not starting an escape is refused, as RFC 4515 requires.
func unescape(s string) ([]byte, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+2 >= len(s) {
				return nil, fmt.Errorf("escape %q is cut short", s[i:])
			}
			b, ok := hexByte(s[i+1], s[i+2])
			if !ok {
				return nil, fmt.Errorf("escape %q is not two hex digits", s[i:i+3])
			}
			out = append(out, b)
			i += 2
		case 0, '(', ')':
			return nil, fmt.Errorf("value %q holds an unescaped %q", s, c)
		default:
			out = append(out, c)
		}
	}
	return out, nil
}

func hexByte(a, b byte) (byte, bool) {
	h, ok1 := hexDigit(a)
	l, ok2 := hexDigit(b)
	return h<<4 | l, ok1 && ok2
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// match reports whether e satisfies f.
func (f filter) match(e *entry) bool {
	switch f.op {
	case '&':
		for _, s := range f.subs {
			if !s.match(e) {
				return false
			}
		}
		return true
	case '|':
		for _, s := range f.subs {
			if s.match(e) {
				return true
			}
		}
		return false
	case '!':
		return !f.subs[0].match(e)
	case '*':
		return len(e.get(f.attr)) > 0
	}
	fold := !binaryAttr(f.attr)
	for _, v := range e.get(f.attr) {
		if f.op == '=' && equal(v, f.value, fold) || f.op == 's' && substrings(v, f.parts, fold) {
			return true
		}
	}
	return false
}

func equal(a, b []byte, fold bool) bool {
	if fold {
		return bytes.EqualFold(a, b)
	}
	return bytes.Equal(a, b)
}

// substrings matches v against initial*any*...*final, each piece in order
// and not overlapping.
func substrings(v []byte, parts [][]byte, fold bool) bool {
	if fold {
		v = bytes.ToLower(v)
	}
	low := func(p []byte) []byte {
		if fold {
			return bytes.ToLower(p)
		}
		return p
	}
	first, last := low(parts[0]), low(parts[len(parts)-1])
	if !bytes.HasPrefix(v, first) {
		return false
	}
	v = v[len(first):]
	if len(v) < len(last) {
		return false
	}
	for _, p := range parts[1 : len(parts)-1] {
		p = low(p)
		i := bytes.Index(v, p)
		if i < 0 {
			return false
		}
		v = v[i+len(p):]
	}
	return bytes.HasSuffix(v, last)
}
