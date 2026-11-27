// Package s3 is the remote store over S3 and the services compatible with it
// (RFC 4 Appendix C).
package s3

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Settings is a store configuration's settings (RFC 13 Appendix B) that the S3
// profile reads; Validate is the rule the control plane, the client and the
// check share (RFC 13 §6).
type Settings struct {
	// Endpoint is the service's URL: https and a host.
	Endpoint string `json:"endpoint" class:"live" location:"store"`
	// Region is the region requests are signed for.
	Region string `json:"region" class:"live"`
	// TrustRoots is a PEM bundle of CA certificates the client verifies the
	// endpoint against. The client dials only HTTPS and never skips
	// verification, so set it when the endpoint's certificate comes from a CA
	// the system does not trust: a private or company CA, or a self-signed
	// test endpoint. It replaces the system's roots for this store rather than
	// adding to them, so it holds every CA the endpoint's chain needs; "" uses
	// the system's roots.
	TrustRoots string `json:"trust_roots" class:"live"`
	// Bucket is the bucket every object of the store lives in.
	Bucket string `json:"bucket" class:"bound" location:"store"`
	// StorageClass is the class every object is written in (RFC 4 C.1).
	StorageClass string `json:"storage_class" class:"next write"`
}

// Validate refuses an endpoint checkEndpoint refuses, an empty region, bucket
// or storage class, a storage class whose gets may need a restore, and trust
// roots checkTrustRoots refuses. Each error names the field and the rule, never
// the endpoint or the PEM text, and nothing is clamped or defaulted: an
// explicit empty value is neither the default nor a value.
func (s Settings) Validate() error {
	if err := checkEndpoint(s.Endpoint); err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	if s.Region == "" {
		return errors.New("region: is empty")
	}
	if s.Bucket == "" {
		return errors.New("bucket: is empty")
	}
	if s.StorageClass == "" {
		return errors.New("storage_class: is empty")
	}
	if !restoreFree(s.StorageClass) {
		return fmt.Errorf("storage_class: %q is not a class whose gets need no restore", s.StorageClass)
	}
	if err := checkTrustRoots(s.TrustRoots); err != nil {
		return fmt.Errorf("trust_roots: %w", err)
	}
	return nil
}

// NamespaceSettings is a namespace's location in the bucket (RFC 4 §4.2).
type NamespaceSettings struct {
	// Prefix is the key prefix every object of the namespace carries.
	Prefix string `json:"prefix" class:"bound" location:"store"`
}

// Validate refuses a prefix checkPrefix refuses, naming the field.
func (s NamespaceSettings) Validate() error {
	if err := checkPrefix(s.Prefix); err != nil {
		return fmt.Errorf("prefix: %w", err)
	}
	return nil
}

// checkEndpoint refuses an endpoint that is not an https URL with a host
// (RFC 4 C.3), and one carrying user info, a query or a fragment: user info or
// a pre-signed query would put a credential in a setting that is no secret
// (RFC 13 §7), and the client builds every request URL from the endpoint. Its error never holds the endpoint,
// which url.Parse's would.
func checkEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	switch {
	case err != nil:
		return errors.New("is not a URL")
	case u.Scheme != "https":
		return errors.New("is not https")
	case u.Hostname() == "":
		return errors.New("names no host")
	case u.User != nil:
		return errors.New("holds user info")
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("holds a query")
	case strings.Contains(endpoint, "#"):
		return errors.New("holds a fragment")
	}
	return nil
}

// checkPrefix refuses a prefix that is empty or does not end in "/", so a
// namespace's keys never run on into a sibling's (ns/ beside ns2/). It does not
// judge whether one prefix nests inside another (a/ beside a/b/). It also
// refuses a prefix that is not valid UTF-8 or holds a control character.
func checkPrefix(prefix string) error {
	switch {
	case !strings.HasSuffix(prefix, "/"):
		return errors.New(`does not end in "/"`)
	case !utf8.ValidString(prefix):
		return errors.New("is not valid UTF-8, which an S3 key must be")
	case strings.ContainsFunc(prefix, unicode.IsControl):
		return errors.New("holds a control character")
	}
	return nil
}

// restoreFree reports whether class is one whose gets need no restore
// (RFC 4 C.1), matched exactly as the service spells it.
//
// decision: an allowlist, not a list of archive classes. A class a service
// adds or names its own way (OUTPOSTS, SNOW, a provider's archive class) is
// refused when written until someone shows its gets need no restore, rather
// than admitted and found archived at the first get. GLACIER and DEEP_ARCHIVE
// are off it because the store never requests a restore; INTELLIGENT_TIERING
// is on it because its own tiers need none, and its optional archive tiers are
// bucket configuration the capability check refuses. A class joins with an
// RFC 4 C.1 amendment.
func restoreFree(class string) bool {
	switch class {
	case "STANDARD", "REDUCED_REDUNDANCY", "STANDARD_IA", "ONEZONE_IA",
		"INTELLIGENT_TIERING", "GLACIER_IR", "EXPRESS_ONEZONE":
		return true
	}
	return false
}

// checkTrustRoots refuses a non-empty PEM bundle holding no certificate, a
// block that is not a CERTIFICATE, a block x509.ParseCertificate refuses, a
// block pem.Decode skipped as malformed, and non-whitespace text before,
// between or after the blocks, naming the block's position. x509's
// AppendCertsFromPEM alone accepts a bundle once any block parses and drops
// the rest silently, so a mistyped root would surface only as TLS failures.
// "" is the system's roots and passes.
func checkTrustRoots(roots string) error {
	if roots == "" {
		return nil
	}
	rest := []byte(roots)
	n := 0
	for {
		rest = bytes.TrimLeft(rest, " \t\r\n")
		if len(rest) == 0 {
			break
		}
		if !bytes.HasPrefix(rest, []byte("-----BEGIN ")) {
			if n == 0 {
				return errors.New("text before the first block is not a PEM block")
			}
			return fmt.Errorf("text after block %d is not a PEM block", n)
		}
		n++
		block, after := pem.Decode(rest)
		// pem.Decode skips a malformed block and returns the next one, so a
		// second BEGIN line inside what it consumed is a block it dropped.
		if block == nil || bytes.Contains(rest[:len(rest)-len(after)], []byte("\n-----BEGIN ")) {
			return fmt.Errorf("block %d is not a well-formed PEM block", n)
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("block %d is a %q, not a CERTIFICATE", n, block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("block %d is not a certificate that parses", n)
		}
		rest = after
	}
	if n == 0 {
		return errors.New("holds no certificate")
	}
	return nil
}
