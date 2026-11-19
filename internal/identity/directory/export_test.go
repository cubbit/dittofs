package directory

import (
	"context"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// DialTLSWithin is DialTLS with ceiling d in place of the package's.
func DialTLSWithin(ctx context.Context, rec Record, d time.Duration) (Conn, error) {
	return dialTLS(ctx, rec, d)
}

// Attrs is the attribute list of rec's "who holds this" question.
func Attrs(rec Record) []string { return rec.attrs() }

// SearchRequest is req as go-ldap sends it.
func SearchRequest(req Request) *ldap.SearchRequest {
	sr, _ := searchRequest(req)
	return sr
}

// SearchError is a failed search's error as Search returns it.
var SearchError = searchError
