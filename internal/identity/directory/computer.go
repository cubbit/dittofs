package directory

import "unicode/utf16"

// unicodePwd renders password as the value Active Directory's unicodePwd
// attribute takes: the password in double quotes, encoded UTF-16LE, written
// only over a verified TLS connection. go-ldap sends a string's bytes as the
// value, so the UTF-16LE bytes are packed into one.
func unicodePwd(password []byte) string {
	codes := utf16.Encode([]rune(`"` + string(password) + `"`))
	buf := make([]byte, 0, 2*len(codes))
	for _, c := range codes {
		buf = append(buf, byte(c), byte(c>>8))
	}
	return string(buf)
}
