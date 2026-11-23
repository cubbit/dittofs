package api

import (
	"context"
	"fmt"
	"iter"
	"net/http"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// apiTokenRoutes are an account's API tokens: created, listed and revoked
// by the account itself, and by an admin; an admin creates one for another
// account only when that account is a service account. Creation answers the
// token, the one time it is shown, so it takes no Idempotency-Key and keeps
// no record of its answer.
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

// tokenAccount refuses the account u, read with ok and err, whose tokens a
// call names: 404 when there is none or it is marked deleted, and when the
// caller ctx carries may not know of its tokens, another account's to a
// caller that may not read accounts.
//
// decision: an admin lists and revokes any account's tokens, wider than the
// role table's "service tokens of others", because both only end or reveal
// the existence of credentials, so an admin can find and end a leaked token
// of any account. Creating one for another account stays a service
// account's only (createAPIToken). Overturned if a listing must hide a
// human account's token metadata from an admin.
func tokenAccount(ctx context.Context, u entity.User, ok bool, err error) error {
	switch c, _ := auth.CallerFrom(ctx); {
	case err != nil:
		return err
	case !ok || u.Deleted:
		return errNotFound
	case u.Principal != c.Principal && !holds(c, auth.Accounts):
		return errHidden
	}
	return nil
}

// tokenVersion is an API token's tag: a token is never rewritten, so it has
// the one version.
const tokenVersion = 1

// apiTokens is what the write adapter knows of an API token: the account
// the path's {id} names, refused by tokenAccount; and the token its {sub}
// names, or with none the token named by id, which for a creation is the
// account's ID before the write, naming no token, and the created token's
// after it. Its audit change holds the spec alone, never the token.
//
// decision: a refused creation's audit record targets an API token by the
// account's ID, the token it would have created having none yet; its
// request path names the account too. Overturned if the audit must be
// queryable by target for an account's refused creations, which would give
// the write adapter a target kind for a refusal apart from its row's.
var apiTokens = writing[apiv1.APITokenSpec]{kind: entity.ResourceAPIToken,
	hidden: func(ctx context.Context, tx entity.ControlPlaneTx, id [16]byte) (bool, error) {
		u, ok, err := tx.User(userAt(id))
		return false, tokenAccount(ctx, u, ok, err)
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
// later read shows. A caller creates its own; another account's only an
// admin does, and only for a service account, one holding no password.
//
// decision: a caller that presented an API token creates none, so a token
// cannot mint its successor: without it, a one-minute token makes one of the
// maximum lifetime, which makes the next, and the chain outlives every
// expiry with no human in it. A session still creates one, so a stolen
// access token can mint a token until a password change or a revocation
// ends it. Overturned if creation must be tied to a fresh password check.
func createAPIToken(ctx context.Context, rq request[apiv1.APITokenSpec], _ struct{}, tx entity.ControlPlaneTx) (apiv1.APIToken, [16]byte, error) {
	if rq.d.Credentials == nil {
		return apiv1.APIToken{}, rq.ID, &refusal{code: apiv1.CodeUnavailable, detail: "the node signs no caller in"}
	}
	c, _ := auth.CallerFrom(ctx)
	if c.Kind == auth.CredentialAPIToken {
		return apiv1.APIToken{}, rq.ID, fmt.Errorf("%w: an API token creates no API token; sign in to create one", vfserr.ErrInvalid)
	}
	u, _, err := tx.User(userAt(rq.ID)) // the hidden hook found it, tracked
	if err != nil {
		return apiv1.APIToken{}, rq.ID, err
	}
	if u.Principal != c.Principal && u.Password != (entity.SecretRef{}) {
		return apiv1.APIToken{}, rq.ID, fmt.Errorf("%w: another account's API token is created only for a service account, one holding no password", vfserr.ErrInvalid)
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
// order, none showing its token; refused by tokenAccount.
var apiTokensList = listing[entity.APIToken, apiv1.APIToken]{
	under: func(ctx context.Context, d Deps, id [16]byte, after []byte, _ map[string]string) iter.Seq2[entity.APIToken, error] {
		u, ok, err := d.Store.User(ctx, userAt(id))
		if err := tokenAccount(ctx, u, ok, err); err != nil {
			return func(yield func(entity.APIToken, error) bool) { yield(entity.APIToken{}, err) }
		}
		return d.Store.APITokens(ctx, u.Principal, after16(after))
	},
	key:  func(t entity.APIToken) []byte { return t.ID[:] },
	item: func(t entity.APIToken, _ perPage) apiv1.APIToken { return apiTokenOf(t, "") },
}
