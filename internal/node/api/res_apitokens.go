package api

import (
	"bytes"
	"context"
	"iter"
	"net/http"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// apiTokenRoutes are an account's API tokens: created, listed and revoked
// by the account itself, and by an admin for any account. Creation answers
// the token, the one time it is shown, so it takes no Idempotency-Key and
// keeps no record of its answer.
var apiTokenRoutes = []route{
	{method: http.MethodGet, pattern: "/api/v1/users/{id}/api-tokens", kind: entity.ResourceAPIToken, right: auth.Self, serve: list(apiTokensList)},
	{method: http.MethodPost, pattern: "/api/v1/users/{id}/api-tokens", kind: entity.ResourceAPIToken, right: auth.Self, keyless: true, serve: write(apiTokens, nil, createAPIToken)},
	{method: http.MethodDelete, pattern: "/api/v1/users/{id}/api-tokens/{sub}", kind: entity.ResourceAPIToken, right: auth.Self, target: tokenRef, sub: subAPIToken,
		serve: write(tagsOf(apiTokens), nil, revokeAPIToken)},
}

var subAPIToken = &param{format: "uuid", desc: "The API token's ID."}

// tokenRef is the target of a row whose {sub} is an API token: the token's
// ID, which names it alone.
func tokenRef(_ [16]byte, sub string) ([]byte, error) {
	id, err := entity.ParseShareID(sub) // every ID is a UUID, which ParseShareID parses
	if err != nil {
		return nil, errNotFound
	}
	return id[:], nil
}

// tokensHidden reports whether the caller ctx carries may not know of u's
// tokens: another account's, to a caller that may not read accounts.
func tokensHidden(ctx context.Context, u entity.User) bool {
	c, _ := auth.CallerFrom(ctx)
	read, _ := auth.Allowed(c, auth.Accounts)
	return u.Principal != c.Principal && !read
}

// tokenVersion is an API token's tag: a token is never rewritten, so it has
// the one version.
const tokenVersion = 1

// apiTokens is what the write adapter knows of an API token: the account
// the path's {id} names, 404 when there is none or the caller may not know
// of its tokens; and the token its {sub} names, or with none the token named
// by id, which for a creation is the account's ID before the write, naming
// no token, and the created token's after it. Its audit change holds the
// spec alone, never the token.
var apiTokens = writing[apiv1.APITokenSpec]{kind: entity.ResourceAPIToken,
	hidden: func(ctx context.Context, tx entity.ControlPlaneTx, id [16]byte) (bool, error) {
		u, ok, err := tx.User(userAt(id))
		if err == nil && (!ok || u.Deleted) {
			return false, errNotFound
		}
		return tokensHidden(ctx, u), err
	},
	read: func(tx entity.ControlPlaneTx, id [16]byte, sub string) (apiv1.APITokenSpec, uint64, bool, error) {
		tid := id
		if sub != "" {
			b, err := tokenRef(id, sub)
			if err != nil {
				return apiv1.APITokenSpec{}, 0, false, err
			}
			tid = [16]byte(b)
		}
		t, ok, err := tx.APIToken(tid)
		if ok && sub != "" && t.Account != userAt(id) {
			ok = false // another account's token is none of this one's
		}
		if !ok {
			return apiv1.APITokenSpec{}, 0, false, err
		}
		return apiv1.APITokenSpec{Expires: t.Expires}, tokenVersion, true, err
	}}

func apiTokenOf(t entity.APIToken, token string) apiv1.APIToken {
	return apiv1.APIToken{ID: idString(t.ID), Generation: tokenVersion, Spec: apiv1.APITokenSpec{Expires: t.Expires},
		Status: apiv1.APITokenStatus{Created: t.Created, Token: token}}
}

// createAPIToken is POST /users/{id}/api-tokens: a token for the account,
// expiring when the spec says, answered with the token itself, which no
// later read shows.
func createAPIToken(_ context.Context, rq request[apiv1.APITokenSpec], _ struct{}, tx entity.ControlPlaneTx) (apiv1.APIToken, [16]byte, error) {
	if rq.d.Credentials == nil {
		return apiv1.APIToken{}, rq.ID, &refusal{code: apiv1.CodeUnavailable, detail: "the node signs no caller in"}
	}
	u, _, err := tx.User(userAt(rq.ID)) // the hidden hook found it, tracked
	if err != nil {
		return apiv1.APIToken{}, rq.ID, err
	}
	t, token, err := rq.d.Credentials.CreateAPIToken(tx, u, rq.Body.Expires)
	if err != nil {
		return apiv1.APIToken{}, rq.ID, err
	}
	return apiTokenOf(t, token), t.ID, nil
}

// revokeAPIToken is DELETE /users/{id}/api-tokens/{sub}: the token deleted,
// so it is refused from the commit on.
func revokeAPIToken(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	b, err := tokenRef(rq.ID, rq.Sub)
	if err != nil {
		return none{}, rq.ID, err
	}
	return none{}, rq.ID, tx.DeleteAPIToken([16]byte(b))
}

// apiTokensList is GET /users/{id}/api-tokens, the account's tokens in ID
// order, none showing its token; 404 when there is no such account or the
// caller may not know of its tokens.
var apiTokensList = listing[entity.APIToken, apiv1.APIToken]{
	under: func(ctx context.Context, d Deps, id [16]byte, after []byte, _ map[string]string) iter.Seq2[entity.APIToken, error] {
		return func(yield func(entity.APIToken, error) bool) {
			u, ok, err := d.Store.User(ctx, userAt(id))
			switch {
			case err != nil:
				yield(entity.APIToken{}, err)
				return
			case !ok || u.Deleted:
				yield(entity.APIToken{}, errNotFound)
				return
			case tokensHidden(ctx, u):
				yield(entity.APIToken{}, errHidden)
				return
			}
			// ponytail: the store lists an account's tokens from the first,
			// so a page skips those up to its cursor; an account holds few.
			// Resume in the store if an account's tokens fill many pages.
			for t, err := range d.Store.APITokens(ctx, u.Principal) {
				if err == nil && bytes.Compare(t.ID[:], after) <= 0 {
					continue
				}
				if !yield(t, err) {
					return
				}
			}
		}
	},
	key:  func(t entity.APIToken) []byte { return t.ID[:] },
	item: func(t entity.APIToken, _ perPage) apiv1.APIToken { return apiTokenOf(t, "") },
}
