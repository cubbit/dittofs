package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/auth/sid"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
)

// resolveOp is a resolve-principal operation whose parameters are p.
func resolveOp(t *testing.T, p apiv1.ResolvePrincipalParams) entity.Operation {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return entity.Operation{Kind: string(apiv1.OpResolvePrincipal), Params: b}
}

// A directory SID no principal holds is minted by the step, in its one turn,
// and the ID it answers is the principal Spell spells as that SID.
func TestResolvePrincipalMintsADirectorySID(t *testing.T) {
	e := newAD(t)
	e.d.AddUser(t, "alice", nil)
	want := sidOf(t, e.entry(t, directory.ByUserName, "alice")).SID

	res, err := e.s.ResolvePrincipalStep(context.Background(), resolveOp(t, apiv1.ResolvePrincipalParams{SID: sid.FormatSID(&want)}))
	if err != nil {
		t.Fatalf("resolve-principal of alice's SID: %v", err)
	}
	if !res.Done {
		t.Fatalf("resolve-principal Done = false, want one turn")
	}
	var got apiv1.Principal
	if err := json.Unmarshal(res.Result, &got); err != nil {
		t.Fatalf("result %s: %v", res.Result, err)
	}
	id, err := entity.ParsePrincipalID(got.ID)
	if err != nil {
		t.Fatalf("result ID %q: %v", got.ID, err)
	}
	sp, err := e.spell(t, entity.Principal{Kind: entity.PrincipalUser, ID: id}, pid.SchemeSID)
	if err != nil {
		t.Fatalf("Spell of the answered %v: %v", got, err)
	}
	if !sp.SID.Equal(&want) {
		t.Errorf("the answered principal's SID %s, want alice's %s", sid.FormatSID(&sp.SID), sid.FormatSID(&want))
	}
}

// A name the directory does not hold fails the operation with a problem
// naming it, which the runner does not retry.
func TestResolvePrincipalUnknownNameFails(t *testing.T) {
	e := newAD(t)
	_, err := e.s.ResolvePrincipalStep(context.Background(), resolveOp(t, apiv1.ResolvePrincipalParams{Name: `KORP\nobody`}))
	p, ok := errors.AsType[*apiv1.Problem](err)
	if !ok {
		t.Fatalf("resolve-principal of an unknown name = %v, want a *apiv1.Problem", err)
	}
	if p.Status != http.StatusNotFound || !strings.Contains(p.Detail, `KORP\nobody`) {
		t.Errorf("problem %d %q, want 404 naming KORP\\nobody", p.Status, p.Detail)
	}
	if _, failed := step.Failure(err); !failed {
		t.Errorf("step.Failure(%v) = retried, want the operation failed", err)
	}
}

// The directory down is a plain error the runner retries, so an outage
// never fails the operation for good.
func TestResolvePrincipalOutageRetried(t *testing.T) {
	e := newAD(t)
	e.d.AddUser(t, "alice", nil)
	e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
	_, err := e.s.ResolvePrincipalStep(context.Background(), resolveOp(t, apiv1.ResolvePrincipalParams{Name: `KORP\alice`}))
	if err == nil {
		t.Fatal("resolve-principal with the directory down succeeded, want an error")
	}
	if _, failed := step.Failure(err); failed {
		t.Errorf("step.Failure(%v) = failed, want retried", err)
	}
}

// test-directory answers this node's result in one turn: reached while the
// directory answers, and not reached, with the reason, while it does not,
// which is still the operation's answer rather than a retry.
func TestTestDirectoryAnswersThisNode(t *testing.T) {
	e := newAD(t)
	for _, tc := range []struct {
		name string
		down bool
	}{{"up", false}, {"down", true}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.down {
				e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
			}
			res, err := e.s.TestDirectoryStep(context.Background(), entity.Operation{Kind: string(apiv1.OpTestDirectory)})
			if err != nil || !res.Done {
				t.Fatalf("test-directory = Done %v, %v; want done", res.Done, err)
			}
			var got identity.TestDirectoryResult
			if err := json.Unmarshal(res.Result, &got); err != nil {
				t.Fatalf("result %s: %v", res.Result, err)
			}
			if got.Reached == tc.down || (got.Error != "") != tc.down {
				t.Errorf("result %+v, want reached %v", got, !tc.down)
			}
		})
	}
}
