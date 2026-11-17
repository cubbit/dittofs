package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// SetWriteTimeout sets the API listener's write timeout; call it before
// Start.
func (s *Server) SetWriteTimeout(d time.Duration) { s.api.WriteTimeout = d }

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
// writes of a knob, its collection and of a vault for operator, a read of
// accounts for admin, a call a caller that must change its password may
// make, and a read failing with an error no row of the problem table names.
var testRows = []route{
	{method: http.MethodGet, pattern: "/api/v1/things/{id}", kind: entity.ResourceShare, right: auth.View, serve: read(func(ctx context.Context, rq request[none]) (map[string]string, error) {
		c, _ := auth.CallerFrom(ctx)
		return map[string]string{"id": entity.ShareID(rq.ID).String(), "role": c.Role.String()}, nil
	})},
	{method: http.MethodPost, pattern: "/api/v1/things/{id}", verb: "poke", kind: entity.ResourceShare, right: auth.Shares, serve: call(verb("poke"))},
	{method: http.MethodPost, pattern: "/api/v1/things/{id}", verb: "prod", kind: entity.ResourceShare, right: auth.Shares, serve: call(verb("prod"))},
	{method: http.MethodPost, pattern: "/api/v1/knobs", kind: entity.ResourceSetting, right: auth.Stores, serve: write(knobs, nil, func(ctx context.Context, rq request[knob], _ struct{}, tx entity.ControlPlaneTx) (knob, [16]byte, error) {
		rq.ID = entity.NewShareID()
		return putKnob(ctx, rq, struct{}{}, tx)
	})},
	{method: http.MethodPut, pattern: "/api/v1/knobs/{id}", kind: entity.ResourceSetting, right: auth.Stores, serve: write(knobs, nil, putKnob)},
	{method: http.MethodPatch, pattern: "/api/v1/knobs/{id}", kind: entity.ResourceSetting, right: auth.Stores, serve: write(knobs, nil, putKnob)},
	{method: http.MethodDelete, pattern: "/api/v1/knobs/{id}", kind: entity.ResourceSetting, right: auth.Stores, serve: write(knobs, nil, func(ctx context.Context, rq request[knob], _ struct{}, tx entity.ControlPlaneTx) (knob, [16]byte, error) {
		_, err := tx.PutSetting(entity.Setting{Scope: nodeScope(rq.ID), Key: "knob"}, rq.Expect)
		return knob{}, rq.ID, err
	})},
	{method: http.MethodPost, pattern: "/api/v1/knobs/{id}", verb: "turn", kind: entity.ResourceSetting, right: auth.Stores, serve: write(turns, nil, func(ctx context.Context, rq request[knob], _ struct{}, tx entity.ControlPlaneTx) (knob, [16]byte, error) {
		k, _, _, err := knobs.read(tx, rq.ID)
		if err != nil {
			return knob{}, rq.ID, err
		}
		k.Spec.Turns++
		return putKnob(ctx, request[knob]{ID: rq.ID, Body: k, Expect: rq.Expect}, struct{}{}, tx)
	})},
	{method: http.MethodPut, pattern: "/api/v1/vaults/{id}", kind: entity.ResourceSecret, right: auth.Stores, serve: write(vaults, seal, func(_ context.Context, rq request[vault], sealed string, tx entity.ControlPlaneTx) (vault, [16]byte, error) {
		_, err := tx.PutSetting(entity.Setting{Scope: nodeScope(rq.ID), Key: "vault", Value: []byte(sealed)}, rq.Expect)
		return vault{Spec: vaultSpec{Password: sealed}}, rq.ID, err
	})},
	{method: http.MethodGet, pattern: "/api/v1/accounts/{id}", kind: entity.ResourceUser, right: auth.Accounts, serve: read(func(context.Context, request[none]) (none, error) {
		return none{}, nil
	})},
	{method: http.MethodPost, pattern: "/api/v1/password", kind: entity.ResourceUser, right: auth.Self, mustChange: true, serve: call(verb("password"))},
	{method: http.MethodGet, pattern: "/api/v1/knobs/{id}", kind: entity.ResourceSetting, right: auth.View, serve: watch(watching[knobRead]{read: readKnob, tag: func(k knobRead) uint64 { return k.Generation }, until: knobWait})},
	{method: http.MethodGet, pattern: "/api/v1/vaults/{id}", kind: entity.ResourceSecret, right: auth.View, serve: watch(watching[vaultRead]{read: readVault, tag: func(v vaultRead) uint64 { return v.Generation }, until: changes(func(vaultRead, vaultRead) bool { return false })})},
	{method: http.MethodGet, pattern: "/api/v1/readers/{id}", kind: entity.ResourceSetting, right: auth.View, serve: watch(watching[reader]{read: readReader, tag: func(r reader) uint64 { return r.Generation }, until: changes(func(first, now reader) bool { return first.Generation != now.Generation })})},
	{method: http.MethodGet, pattern: "/api/v1/tags", kind: entity.ResourceNetgroup, right: auth.View, serve: list(tags)},
	{method: http.MethodGet, pattern: "/api/v1/fail", kind: entity.ResourceInstallation, right: auth.View, serve: read(func(context.Context, request[none]) (none, error) {
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
// PUT writes it whole, a PATCH merges into it, a DELETE empties its value,
// which reads as no record, :turn counts one more turn and a POST on the collection creates one
// under a new ID. Its writes raise the settings generation, as every
// setting's does.
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
	if err != nil || !ok || len(s.Value) == 0 {
		return knob{}, 0, false, err
	}
	var k knob
	return k, s.Generation, true, json.Unmarshal(s.Value, &k.Spec)
}}

// turns is knobs for :turn, an action that changes the spec.
var turns = func() writing[knob] { w := knobs; w.changesSpec = true; return w }()

func putKnob(_ context.Context, rq request[knob], _ struct{}, tx entity.ControlPlaneTx) (knob, [16]byte, error) {
	v, err := json.Marshal(rq.Body.Spec)
	if err != nil {
		return knob{}, rq.ID, err
	}
	if _, err := tx.PutSetting(entity.Setting{Scope: nodeScope(rq.ID), Key: "knob", Value: v}, rq.Expect); err != nil {
		return knob{}, rq.ID, err
	}
	if rq.Body.Spec.Value == KnobBreaks {
		_, err = tx.PutSetting(entity.Setting{}, 0)
	}
	return rq.Body, rq.ID, err
}

// vault is a test resource holding a secret: its PUT carries the password,
// which its pre-write step seals, and the record holds only the sealed
// reference, which its audit record holds as "set".
type vault struct {
	Spec vaultSpec `json:"spec"`
}

type vaultSpec struct {
	Password string `json:"password"`
}

// Unreachable is the password whose sealing fails ErrUnavailable.
const Unreachable = "unreachable"

var vaults = writing[vault]{kind: entity.ResourceSecret, secret: func(p string) bool { return p == "spec.password" }, read: func(tx entity.ControlPlaneTx, id [16]byte) (vault, uint64, bool, error) {
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

// Hold is a node's Applied that a test holds: each call is delivered on
// Reached, and returns Err once Release is closed, or its context's error
// once that ends.
type Hold struct {
	Reached, Release chan struct{}
	Err              error
}

// NewHold is a Hold returning err, delivering up to 8 calls no one has
// received.
func NewHold(err error) *Hold {
	return &Hold{Reached: make(chan struct{}, 8), Release: make(chan struct{}), Err: err}
}

func (h *Hold) Applied(ctx context.Context) error {
	h.Reached <- struct{}{}
	select {
	case <-h.Release:
		return h.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// knobRead is a knob as GET reads it: its tag and its spec, generation 0
// when there is none.
type knobRead struct {
	Generation uint64   `json:"generation"`
	Spec       knobSpec `json:"spec"`
}

func readKnob(ctx context.Context, st reads, rq request[none]) (knobRead, error) {
	s, err := settingIn(ctx, st, rq.ID, "knob")
	if err != nil || s.Generation == 0 {
		return knobRead{}, err
	}
	k := knobRead{Generation: s.Generation}
	return k, json.Unmarshal(s.Value, &k.Spec)
}

// KnobOn is the wait word ?wait= takes on a knob, beside a duration: until
// the knob's value is on, for at most the longest wait.
const KnobOn = "on"

// knobWait is a knob's ?wait=: a duration, until its tag changes, or KnobOn.
func knobWait(w string) (time.Duration, func(first, now knobRead) bool, error) {
	if w == KnobOn {
		return maxWait, func(_, now knobRead) bool { return now.Spec.Value == KnobOn }, nil
	}
	return changes(func(first, now knobRead) bool { return first.Generation != now.Generation })(w)
}

// reader is a knob as GET /api/v1/readers/{id} reads it, with the role of
// the caller the context of each read carries.
type reader struct {
	knobRead
	Role string `json:"role"`
}

func readReader(ctx context.Context, st reads, rq request[none]) (reader, error) {
	k, err := readKnob(ctx, st, rq)
	c, _ := auth.CallerFrom(ctx)
	return reader{k, c.Role.String()}, err
}

// vaultRead is a vault as GET reads it: the reference its password was
// sealed under, and its version.
type vaultRead struct {
	Generation uint64    `json:"generation"`
	Spec       vaultSpec `json:"spec"`
}

func readVault(ctx context.Context, st reads, rq request[none]) (vaultRead, error) {
	s, err := settingIn(ctx, st, rq.ID, "vault")
	return vaultRead{Generation: s.Generation, Spec: vaultSpec{Password: string(s.Value)}}, err
}

// settingIn is the setting key at the node scope id names, the zero one
// when there is none.
func settingIn(ctx context.Context, st reads, id [16]byte, key string) (entity.Setting, error) {
	for s, err := range st.Settings(ctx, nodeScope(id)) {
		if err != nil || s.Key == key {
			return s, err
		}
	}
	return entity.Setting{}, nil
}

// TagsFrom and TagsTo bound the keys a cursor of the test listing may
// name: [a, {), every lowercase name.
const TagsFrom, TagsTo = "a", "{"

// tag is a netgroup as the test listing GET /api/v1/tags answers it, in
// name order, filtered by name, with where it is applied: a netgroup carries
// no position, and its reference is its name.
type tag struct {
	Name    string        `json:"name"`
	Members []string      `json:"members"`
	Applied apiv1.Applied `json:"applied"`
}

var tags = listing[entity.Netgroup, tag]{
	lo:      []byte(TagsFrom),
	hi:      []byte(TagsTo),
	filters: []string{"name"},
	records: func(ctx context.Context, st reads, after []byte, f map[string]string) iter.Seq2[entity.Netgroup, error] {
		all := st.Netgroups(ctx, string(after))
		name, ok := f["name"]
		if !ok {
			return all
		}
		// The store has no lookup of one netgroup, so the test listing
		// finds a name by walking to it; a production listing's name
		// filter reads its name index.
		return func(yield func(entity.Netgroup, error) bool) {
			for g, err := range all {
				if err != nil || g.Name >= name {
					if err != nil || g.Name == name {
						yield(g, err)
					}
					return
				}
			}
		}
	},
	key: func(g entity.Netgroup) []byte { return []byte(g.Name) },
	item: func(g entity.Netgroup, p perPage) tag {
		return tag{g.Name, g.Members, p.applied(entity.ResourceRef{Kind: entity.ResourceNetgroup, ID: []byte(g.Name)}, 0, nil)}
	},
}
