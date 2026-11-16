package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// NewWithTestRows is New serving testRows beside the production table, so
// the adapters run, through the production mux and authentication, on rows
// of each kind before production rows of that kind exist.
func NewWithTestRows(d Deps) *Server {
	return newServer(d, append(slices.Clone(routes), testRows...))
}

// Row is a request that reaches one row, and whether the row is public.
type Row struct {
	Method, Path string
	Public       bool
}

// TestID is the ID every Row's path names.
const TestID = "0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b"

// Rows returns a request reaching each row NewWithTestRows serves.
func Rows() []Row {
	var out []Row
	for _, rt := range append(slices.Clone(routes), testRows...) {
		id := TestID
		if rt.verb != "" {
			id += ":" + rt.verb
		}
		out = append(out, Row{rt.method, strings.Replace(rt.pattern, "{id}", id, 1), rt.right == auth.Public})
	}
	return out
}

// testRows are a read for viewer, two verbs on one path for operator, a
// write of a netgroup named by the path's ID for operator, a write of that
// netgroup and then of one the store refuses, a read of accounts for admin,
// a call a caller that must change its password may make, and a read failing
// with an error no row of the problem table names.
var testRows = []route{
	{method: http.MethodGet, pattern: "/api/v1/things/{id}", right: auth.View, serve: read(func(ctx context.Context, rq request[none]) (map[string]string, error) {
		c, _ := auth.CallerFrom(ctx)
		return map[string]string{"id": entity.ShareID(rq.ID).String(), "role": c.Role.String()}, nil
	})},
	{method: http.MethodPost, pattern: "/api/v1/things/{id}", verb: "poke", right: auth.Shares, serve: call(verb("poke"))},
	{method: http.MethodPost, pattern: "/api/v1/things/{id}", verb: "prod", right: auth.Shares, serve: call(verb("prod"))},
	{method: http.MethodPut, pattern: "/api/v1/netgroups/{id}", right: auth.Stores, serve: write(func(_ context.Context, rq request[struct {
		Members []string `json:"members"`
	}], tx entity.ControlPlaneTx) (map[string]uint64, error) {
		v, err := tx.PutNetgroup(entity.Netgroup{Name: entity.ShareID(rq.ID).String(), Members: rq.Body.Members}, 0)
		return map[string]uint64{"version": v}, err
	})},
	{method: http.MethodPut, pattern: "/api/v1/pairs/{id}", right: auth.Stores, serve: write(func(_ context.Context, rq request[none], tx entity.ControlPlaneTx) (none, error) {
		if _, err := tx.PutNetgroup(entity.Netgroup{Name: entity.ShareID(rq.ID).String()}, 0); err != nil {
			return none{}, err
		}
		_, err := tx.PutNetgroup(entity.Netgroup{}, 0)
		return none{}, err
	})},
	{method: http.MethodGet, pattern: "/api/v1/accounts/{id}", right: auth.Accounts, serve: read(func(context.Context, request[none]) (none, error) {
		return none{}, nil
	})},
	{method: http.MethodPost, pattern: "/api/v1/password", right: auth.Self, mustChange: true, serve: call(verb("password"))},
	{method: http.MethodGet, pattern: "/api/v1/fail", right: auth.View, serve: read(func(context.Context, request[none]) (none, error) {
		return none{}, errors.New(UnnamedError)
	})},
}

// UnnamedError is the text of the error GET /api/v1/fail fails with.
const UnnamedError = "no row names this, s3cr3t-7a1f"

// verb answers 200 with the verb's name.
func verb(name string) func(context.Context, request[none]) (answer, error) {
	return func(context.Context, request[none]) (answer, error) {
		return jsonAnswer(http.StatusOK, map[string]string{"verb": name})
	}
}
