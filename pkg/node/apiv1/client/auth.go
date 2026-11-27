package client

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// SignIn signs name in with password and returns its access and refresh
// tokens. It is sent once, with no idempotency key, and its 401 is a refused
// password, returned as it is.
func (c *Client) SignIn(ctx context.Context, name string, password []byte) (apiv1.Tokens, error) {
	return c.credential(ctx, "/api/v1/auth/sign-in", apiv1.SignIn{Name: name, Password: string(password)})
}

// Refresh exchanges a session's refresh token for new tokens; the one
// presented refreshes no more. It is sent as SignIn is: a second sending
// would present a rotated token and revoke the session.
func (c *Client) Refresh(ctx context.Context, refresh string) (apiv1.Tokens, error) {
	return c.credential(ctx, "/api/v1/auth/refresh", apiv1.Refresh{Token: refresh})
}

// SignOut ends the session of the token the client's source holds. It is
// sent with no idempotency key, which the server refuses on it, and so is
// never sent twice after a lost answer.
func (c *Client) SignOut(ctx context.Context) error {
	_, err := c.write(ctx, http.MethodPost, "/api/v1/auth/sign-out", nil, "", []CallOption{WithoutIdempotencyKey()})
	return err
}

// ChangePassword changes the password of the account the client's source
// signs in, presenting its current one, and returns the tokens of the
// session the change opens; every earlier session of the account, the
// source's included, refreshes no more. The source refreshes first, so an
// expired access token is never what a 401 answers. The change is then sent
// once, with no idempotency key, and its 401 is a refused current password,
// returned as it is and never refreshed.
func (c *Client) ChangePassword(ctx context.Context, current, next []byte) (apiv1.Tokens, error) {
	if c.tokens != nil {
		tk, err := c.tokens.Token(ctx)
		if err != nil {
			return apiv1.Tokens{}, err
		}
		if _, err := c.tokens.Refresh(ctx, tk); err != nil {
			return apiv1.Tokens{}, err
		}
	}
	return c.credentialCall(ctx, call{path: "/api/v1/auth/password", noRefresh: true},
		apiv1.PasswordChange{Current: string(current), New: string(next)})
}

// credential sends a sign-in or a refresh carrying body and decodes the
// tokens it answers.
func (c *Client) credential(ctx context.Context, path string, body any) (apiv1.Tokens, error) {
	return c.credentialCall(ctx, call{path: path, credential: true}, body)
}

// credentialCall sends cl, a POST with no idempotency key, carrying body and
// decodes the tokens it answers.
func (c *Client) credentialCall(ctx context.Context, cl call, body any) (apiv1.Tokens, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return apiv1.Tokens{}, err
	}
	cl.method, cl.body, cl.noKey = http.MethodPost, b, true
	rp, err := c.do(ctx, cl)
	if err != nil {
		return apiv1.Tokens{}, err
	}
	var t apiv1.Tokens
	return t, json.Unmarshal(rp.body, &t)
}
