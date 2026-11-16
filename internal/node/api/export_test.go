package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
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

// testRows are a read for viewer, two verbs on one path for operator, the
// writes of a knob and of a vault for operator, a read of accounts for
// admin, a call a caller that must change its password may make, and a read
// failing with an error no row of the problem table names.
var testRows = []route{
	{method: http.MethodGet, pattern: "/api/v1/things/{id}", right: auth.View, serve: read(func(ctx context.Context, rq request[none]) (map[string]string, error) {
		c, _ := auth.CallerFrom(ctx)
		return map[string]string{"id": entity.ShareID(rq.ID).String(), "role": c.Role.String()}, nil
	})},
	{method: http.MethodPost, pattern: "/api/v1/things/{id}", verb: "poke", right: auth.Shares, serve: call(verb("poke"))},
	{method: http.MethodPost, pattern: "/api/v1/things/{id}", verb: "prod", right: auth.Shares, serve: call(verb("prod"))},
	{method: http.MethodPut, pattern: "/api/v1/knobs/{id}", right: auth.Stores, serve: write(knobs, nil, putKnob)},
	{method: http.MethodPatch, pattern: "/api/v1/knobs/{id}", right: auth.Stores, serve: write(knobs, nil, putKnob)},
	{method: http.MethodDelete, pattern: "/api/v1/knobs/{id}", right: auth.Stores, serve: write(knobs, nil, func(ctx context.Context, rq request[knob], _ struct{}, tx entity.ControlPlaneTx) (knob, error) {
		return putKnob(ctx, request[knob]{ID: rq.ID, Expect: rq.Expect}, struct{}{}, tx)
	})},
	{method: http.MethodPost, pattern: "/api/v1/knobs/{id}", verb: "turn", right: auth.Stores, serve: write(knobs, nil, func(ctx context.Context, rq request[knob], _ struct{}, tx entity.ControlPlaneTx) (knob, error) {
		k, _, _, err := knobs.read(tx, rq.ID)
		if err != nil {
			return knob{}, err
		}
		k.Spec.Turns++
		return putKnob(ctx, request[knob]{ID: rq.ID, Body: k, Expect: rq.Expect}, struct{}{}, tx)
	})},
	{method: http.MethodPut, pattern: "/api/v1/vaults/{id}", right: auth.Stores, serve: write(vaults, seal, func(_ context.Context, rq request[vault], sealed string, tx entity.ControlPlaneTx) (vault, error) {
		_, err := tx.PutSetting(entity.Setting{Scope: nodeScope(rq.ID), Key: "vault", Value: []byte(sealed)}, rq.Expect)
		return vault{Spec: vaultSpec{Password: sealed}}, err
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

// knob is a test resource: a setting at the node scope its path's ID names,
// its spec JSON in the setting's value, its tag the setting's generation. A
// PUT writes it whole, a PATCH merges into it, a DELETE writes its empty
// spec, and :turn counts one more turn. Its writes raise the settings
// generation, as every setting's does.
type knob struct {
	Spec knobSpec `json:"spec"`
}

type knobSpec struct {
	Value string `json:"value"`
	Turns int    `json:"turns,omitempty"`
}

// KnobBreaks is the value whose PUT writes the knob and then a setting the
// store refuses, so its transaction fails after a write.
const KnobBreaks = "break"

func nodeScope(id [16]byte) entity.SettingScope {
	return entity.SettingScope{Kind: entity.ScopeNode, ID: id}
}

var knobs = writing[knob]{kind: entity.ResourceSetting, raises: true, read: func(tx entity.ControlPlaneTx, id [16]byte) (knob, uint64, bool, error) {
	s, ok, err := tx.Setting(nodeScope(id), "knob")
	if err != nil || !ok {
		return knob{}, 0, false, err
	}
	var k knob
	return k, s.Generation, true, json.Unmarshal(s.Value, &k.Spec)
}}

func putKnob(_ context.Context, rq request[knob], _ struct{}, tx entity.ControlPlaneTx) (knob, error) {
	v, err := json.Marshal(rq.Body.Spec)
	if err != nil {
		return knob{}, err
	}
	if _, err := tx.PutSetting(entity.Setting{Scope: nodeScope(rq.ID), Key: "knob", Value: v}, rq.Expect); err != nil {
		return knob{}, err
	}
	if rq.Body.Spec.Value == KnobBreaks {
		_, err = tx.PutSetting(entity.Setting{}, 0)
	}
	return rq.Body, err
}

// vault is a test resource holding a secret: its PUT carries the password,
// which its pre-write step seals, and the record holds only the sealed
// reference.
type vault struct {
	Spec vaultSpec `json:"spec"`
}

type vaultSpec struct {
	Password string `json:"password"`
}

// Unreachable is the password whose sealing fails ErrUnavailable.
const Unreachable = "unreachable"

var vaults = writing[vault]{kind: entity.ResourceSecret, read: func(tx entity.ControlPlaneTx, id [16]byte) (vault, uint64, bool, error) {
	s, ok, err := tx.Setting(nodeScope(id), "vault")
	if err != nil || !ok {
		return vault{}, 0, false, err
	}
	return vault{Spec: vaultSpec{Password: string(s.Value)}}, s.Generation, true, nil
}}

var (
	sealsMu sync.Mutex
	seals   = map[[16]byte]int{}
)

// seal is the vault's pre-write step: the password's reference, a digest
// of it, counted per vault.
func seal(_ context.Context, rq request[vault]) (string, error) {
	sealsMu.Lock()
	seals[rq.ID]++
	sealsMu.Unlock()
	if rq.Body.Spec.Password == Unreachable {
		return "", vfserr.ErrUnavailable
	}
	sum := sha256.Sum256([]byte(rq.Body.Spec.Password))
	return "sealed:" + hex.EncodeToString(sum[:8]), nil
}

// Seals is how many times the pre-write step sealed a password for the
// vault id names.
func Seals(id string) int {
	sid, _ := entity.ParseShareID(id)
	sealsMu.Lock()
	defer sealsMu.Unlock()
	return seals[sid]
}
