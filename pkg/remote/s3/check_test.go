package s3

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/remote"
)

// tieringPage answers one ListBucketIntelligentTieringConfigurations page of
// one configuration, id, truncated with next when next is not "".
func tieringPage(w http.ResponseWriter, id, next string) {
	trunc := ""
	if next != "" {
		trunc = "<IsTruncated>true</IsTruncated><NextContinuationToken>" + next + "</NextContinuationToken>"
	}

	w.Header().Set("Content-Type", "application/xml")
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+
		`<ListBucketIntelligentTieringConfigurationsOutput xmlns="http://s3.amazonaws.com/doc/2006-03-01/">%s`+
		`<IntelligentTieringConfiguration><Id>%s</Id><Status>Enabled</Status>`+
		`<Tiering><Days>90</Days><AccessTier>ARCHIVE_ACCESS</AccessTier></Tiering></IntelligentTieringConfiguration>`+
		`</ListBucketIntelligentTieringConfigurationsOutput>`, trunc, id)
}

// The tiering read follows every page's token to the last page, and fails a
// service that never stops truncating once it has read the bound.
func TestTieringConfigsReadsEveryPage(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("two_pages", func(t *testing.T) {
		var tokens []string
		c, _ := testClient(t, tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
			tok := r.URL.Query().Get("continuation-token")
			tokens = append(tokens, tok)
			if tok == "" {
				tieringPage(w, "first", "page-2")
				return
			}
			tieringPage(w, "second", "")
		}, false, nil), nil)

		got, err := tieringConfigs(t.Context(), c)
		if err != nil || len(got) != 2 || aws.ToString(got[0].Id) != "first" || aws.ToString(got[1].Id) != "second" {
			t.Fatalf("tieringConfigs = %d configurations, %v; want first and second", len(got), err)
		}

		if len(tokens) != 2 || tokens[1] != "page-2" {
			t.Errorf("requests carried tokens %q, want none then page-2", tokens)
		}
	})

	t.Run("never_ends", func(t *testing.T) {
		var n atomic.Int64
		c, _ := testClient(t, tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
			tieringPage(w, "again", fmt.Sprintf("page-%d", n.Add(1)))
		}, false, nil), nil)

		if _, err := tieringConfigs(t.Context(), c); !errors.Is(err, remote.ErrTransient) {
			t.Errorf("tieringConfigs of an endless listing = %v, want ErrTransient", err)
		}

		if got := n.Load(); got != tieringPages {
			t.Errorf("the endless listing was read %d times, want the bound, %d", got, tieringPages)
		}
	})
}
