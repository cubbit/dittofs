package api

import (
	"context"
	"net/http"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// route is one row of the route table: a method and a pattern of
// net/http.ServeMux, the action a POST on {id}:verb names, the kind of
// resource it reaches, which a refusal before its adapter is audited
// against and only a public row leaves zero, the right its caller
// must hold, whether it answers a caller that must change its password, and
// its adapter.
type route struct {
	method, pattern string
	verb            string
	kind            entity.ResourceKind
	right           auth.Right
	mustChange      bool
	serve           adapter
}

// op names the row in its metrics and its span: its method and pattern, and
// its verb.
func (rt *route) op() string {
	op := rt.method + " " + rt.pattern
	if rt.verb != "" {
		op += ":" + rt.verb
	}
	return op
}

// routes is the API's route table, which alone drives the mux: a path the
// table does not name is 404, after authentication, so the table is not
// revealed to a caller that has none.
//
// decision: paths take IDs, never names. A path that took a name or an ID
// would make "does this exist?" ambiguous, a share named like another's ID,
// and a rename would move the path under a client holding it; so the {id} of
// every row is parsed as an ID, and a name there is 404. A name is found by a
// listing's filter. Overturned only if a resource ever lacks a stable ID,
// which no record of the metadata store does.
var routes = []route{
	{method: http.MethodGet, pattern: "/api/version", right: auth.Public, serve: read(version)},
}

// version answers the API's major versions and nothing else.
func version(context.Context, request[none]) (apiv1.Version, error) {
	return apiv1.Version{Majors: []string{"v1"}}, nil
}
