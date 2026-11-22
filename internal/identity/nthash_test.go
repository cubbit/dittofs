package identity_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/node/step"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
)

// ntDrop is a server over a real store, in memory or on disk, with the
// secret provider over local key files, reopened over the same directory as
// a restart does.
type ntDrop struct {
	t     *testing.T
	dir   string // "" for a store in memory, which cannot be reopened
	keys  map[secrets.Role]keyprovider.KeyProvider
	db    *badgerkv.Store
	fault *kvtest.FaultKV
	st    *kvstore.Store
	cp    kvstore.ControlPlane
	p     *secrets.Provider
	srv   *identity.Server
	users []ntUser
}

// ntUser is a seeded user and the sealed secrets it was seeded with.
type ntUser struct {
	u        entity.User
	gen      uint64
	password entity.Secret
}

func newNTDrop(t *testing.T, onDisk bool) *ntDrop {
	t.Helper()
	d := &ntDrop{t: t, keys: roleKeys(t)}
	if onDisk {
		d.dir = t.TempDir()
	}
	t.Cleanup(func() {
		if err := d.db.Close(); err != nil {
			t.Error(err)
		}
	})
	d.open(true)
	return d
}

// open opens the store, first closing the one open, and builds the server
// over it; init initialises a fresh store.
func (d *ntDrop) open(init bool) {
	d.t.Helper()
	ctx := context.Background()
	if d.db != nil {
		if err := d.db.Close(); err != nil {
			d.t.Fatal(err)
		}
	}
	db, err := badgerkv.Open(badgerkv.Options{Dir: d.dir})
	if err != nil {
		d.t.Fatal(err)
	}
	d.db = db
	if init {
		if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
			d.t.Fatal(err)
		}
	}
	d.fault = kvtest.NewFaultKV(db)
	if d.st, err = kvstore.Open(ctx, d.fault); err != nil {
		d.t.Fatal(err)
	}
	d.cp = d.st.ControlPlane()
	if d.p, err = secrets.New(d.cp, d.keys); err != nil {
		d.t.Fatal(err)
	}
	deps := identity.Deps{Store: d.st, Clock: sim.NewTestClock(time.Unix(0, 0)), Secrets: d.p, ControlPlane: d.cp}
	if d.srv, err = identity.New(deps); err != nil {
		d.t.Fatalf("New: %v", err)
	}
}

// seal seals value as version expect+1 of u's secret ref of kind, owned by
// u's record under field, in tx.
func (d *ntDrop) seal(tx entity.ControlPlaneTx, role secrets.Role, kind entity.SecretKind, u entity.User, field string, ref entity.SecretRef, value []byte, expect uint64) (entity.Secret, error) {
	owner := entity.UserSecretOwner(u.Principal.ID, field)
	sec, err := d.p.Seal(context.Background(), role, kind, owner, ref, value, secrets.Meta{})
	if err != nil {
		return sec, err
	}
	return sec, tx.PutSecret(sec, expect)
}

// seed stores n file users, each with a password hash and an NT hash sealed
// through the provider, the first user's NT hash at two versions.
func (d *ntDrop) seed(n int) {
	d.t.Helper()
	for i := range n {
		u := entity.User{
			Principal: entity.NewUser(),
			Name:      "user" + strconv.Itoa(i),
			IDs:       fileIDs(strconv.Itoa(20000 + i)),
			Password:  entity.SecretRef(uuid.New()),
			NTHash:    entity.SecretRef(uuid.New()),
		}
		nu := ntUser{u: u}
		err := d.cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) (err error) {
			if nu.password, err = d.seal(tx, secrets.Storage, entity.SecretPassword, u, "password", u.Password, []byte("argon2id of "+u.Name), 0); err != nil {
				return err
			}
			if _, err := d.seal(tx, secrets.Protocol, entity.SecretNTHash, u, "nt_hash", u.NTHash, bytes.Repeat([]byte{1}, 16), 0); err != nil {
				return err
			}
			nu.gen, err = tx.PutUser(u, 0, entity.Alloc{})
			return err
		})
		if err == nil && i == 0 {
			// The provider seals the version after the stored one, so the
			// second version is sealed once the first has committed.
			err = d.cp.Write(context.Background(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
				_, err := d.seal(tx, secrets.Protocol, entity.SecretNTHash, u, "nt_hash", u.NTHash, bytes.Repeat([]byte{2}, 16), 1)
				return err
			})
		}
		if err != nil {
			d.t.Fatalf("seeding %s: %v", u.Name, err)
		}
		d.users = append(d.users, nu)
	}
}

// turn runs one turn of op and feeds its Next back as op.Step, as the runner
// records it; it returns the result, and the progress the new cursor reads.
func (d *ntDrop) turn(op *entity.Operation) (step.StepResult, apiv1.Progress, error) {
	res, err := d.srv.NTHashDeleteStep(context.Background(), *op)
	if err != nil {
		return res, apiv1.Progress{}, err
	}
	op.Step = res.Next
	return res, d.srv.NTHashDeleteProgress(context.Background(), *op), nil
}

// mustTurn is turn, failing the test on an error.
func (d *ntDrop) mustTurn(op *entity.Operation) (step.StepResult, apiv1.Progress) {
	d.t.Helper()
	res, p, err := d.turn(op)
	if err != nil {
		d.t.Fatalf("turn of the deletion: %v", err)
	}
	return res, p
}

// end commits res's End as the runner does in the operation's last write.
func (d *ntDrop) end(res step.StepResult) {
	d.t.Helper()
	if !res.Done || res.End == nil {
		d.t.Fatalf("last turn: Done %v, End set %v; want both", res.Done, res.End != nil)
	}
	if err := d.cp.Write(context.Background(), entity.WriteMeta{}, res.End); err != nil {
		d.t.Fatalf("committing End: %v", err)
	}
}

// deleted is the installation's NTLMOff.Deleted.
func (d *ntDrop) deleted() entity.OperationID {
	d.t.Helper()
	in, err := d.st.Installation(context.Background())
	if err != nil {
		d.t.Fatal(err)
	}
	return in.NTLMOff.Deleted
}

// noneLeft fails unless no version of any seeded NT hash is held, every
// user's NTHash is zero, and every password hash is held as it was sealed.
func (d *ntDrop) noneLeft(when string) {
	d.t.Helper()
	ctx := context.Background()
	for _, nu := range d.users {
		for _, v := range []uint64{0, 1, 2} {
			if _, err := d.cp.Secret(ctx, nu.u.NTHash, v); !merrs.IsNotFoundError(err) {
				d.t.Errorf("%s: %s's NT hash version %d: error %v, want not found", when, nu.u.Name, v, err)
			}
		}
		u, err := d.st.Principals().User(ctx, nu.u.Principal)
		if err != nil {
			d.t.Fatalf("%s: reading %s: %v", when, nu.u.Name, err)
		}
		if u.NTHash != (entity.SecretRef{}) || u.Password != nu.u.Password {
			d.t.Errorf("%s: %s's NTHash %x, Password %x; want zero, %x", when, nu.u.Name, u.NTHash, u.Password, nu.u.Password)
		}
		pw, err := d.cp.Secret(ctx, nu.u.Password, 0)
		if err != nil || pw.Version != 1 || !bytes.Equal(pw.Value, nu.password.Value) {
			d.t.Errorf("%s: %s's password hash: version %d, %v; want version 1 as sealed", when, nu.u.Name, pw.Version, err)
		}
	}
}

func ntOperation() entity.Operation {
	return entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpNTHashDelete)}
}

// The deletion, its turns fed back their cursors, drops every NT hash in
// batches of at most 256, counts a short last batch as what it held, and
// records itself completed only when its End commits.
func TestNTHashDeleteRunsToDone(t *testing.T) {
	for _, onDisk := range []bool{false, true} {
		t.Run(fmt.Sprintf("on disk %v", onDisk), func(t *testing.T) {
			d := newNTDrop(t, onDisk)
			d.seed(600)
			op := ntOperation()
			var last int64
			var res step.StepResult
			for turns := 1; !res.Done; turns++ {
				if turns > 3 {
					t.Fatalf("turn %d of 600 NT hashes in batches of 256: want done by turn 3", turns)
				}
				var p apiv1.Progress
				res, p = d.mustTurn(&op)
				if p.Done < last || p.Done-last > 256 || p.Total != -1 {
					t.Errorf("turn %d: progress %d of %d after %d; want at most 256 more, of -1", turns, p.Done, p.Total, last)
				}
				last = p.Done
			}
			if last != 600 {
				t.Errorf("progress at done: %d, want 600", last)
			}
			if got := d.deleted(); got != (entity.OperationID{}) {
				t.Errorf("NTLMOff.Deleted before End committed: %x, want zero", got)
			}
			d.end(res)
			if got := d.deleted(); got != op.ID {
				t.Errorf("NTLMOff.Deleted: %x, want the operation %x", got, op.ID)
			}
			d.noneLeft("after the deletion")
		})
	}
}

// A batch whose commit fails is an error the runner retries, not a failure
// of the operation, and the turn run again from the same cursor completes.
func TestNTHashDeleteRetriesAFailedBatch(t *testing.T) {
	d := newNTDrop(t, false)
	d.seed(600)
	op := ntOperation()
	d.mustTurn(&op)
	d.mustTurn(&op)
	s, err := sim.NewSchedule(1, sim.NewTestClock(time.Unix(0, 0)),
		sim.Rule{Op: kvtest.OpUpdate, Key: "*", Count: 1, Action: sim.FaultAction{Err: errors.New("injected commit failure")}})
	if err != nil {
		t.Fatal(err)
	}
	d.fault.SetFault(s)
	before := op.Step
	if _, _, err := d.turn(&op); err == nil || errors.Is(err, step.ErrStepFailed) {
		t.Fatalf("third turn under a failing commit: error %v, want one the runner retries", err)
	}
	if !bytes.Equal(op.Step, before) {
		t.Fatalf("a failed turn moved the cursor")
	}
	d.fault.SetFault(nil)
	res, p := d.mustTurn(&op)
	if p.Done != 600 {
		t.Errorf("progress after the retried turn: %d, want 600", p.Done)
	}
	d.end(res)
	d.noneLeft("after the retried turn")
}

// A crash after a batch committed but before the runner recorded its cursor
// runs the turn again over the reopened store: it drops nothing, counts
// nothing twice, and ends the deletion.
func TestNTHashDeleteTurnRunAgainAfterACrash(t *testing.T) {
	d := newNTDrop(t, true)
	d.seed(600)
	op := ntOperation()
	d.mustTurn(&op)
	d.mustTurn(&op)
	recorded := op.Step
	if res, _ := d.mustTurn(&op); !res.Done {
		t.Fatalf("third turn: not done, want done")
	}
	d.open(false)
	op.Step = recorded
	res, p := d.mustTurn(&op)
	if p.Done != 512 {
		t.Errorf("progress after the turn run again: %d, want 512, the crashed batch uncounted", p.Done)
	}
	d.end(res)
	if got := d.deleted(); got != op.ID {
		t.Errorf("NTLMOff.Deleted: %x, want the operation %x", got, op.ID)
	}
	d.noneLeft("after the turn run again")
}

// A user record the store cannot decode fails the operation, and the
// deletion is not recorded.
func TestNTHashDeleteFailsOnAnUndecodableUser(t *testing.T) {
	d := newNTDrop(t, false)
	d.seed(2)
	id := d.users[0].u.Principal.ID
	// The user's record key as the store lays it out: kind U, then the ID.
	err := d.db.Update(context.Background(), func(tx kv.Txn) error {
		return tx.Set(keys.ID([]byte{keys.KindU}, id[:]), []byte{0xff})
	})
	if err != nil {
		t.Fatal(err)
	}
	op := ntOperation()
	if _, _, err := d.turn(&op); !errors.Is(err, step.ErrStepFailed) {
		t.Errorf("turn over an undecodable user: error %v, want one wrapping ErrStepFailed", err)
	}
	if got := d.deleted(); got != (entity.OperationID{}) {
		t.Errorf("NTLMOff.Deleted after a failed deletion: %x, want zero", got)
	}
}

// A put holding the generation read before a batch zeroed the user's NT hash
// is refused, so the dropped reference is not written back.
func TestNTHashDeleteRefusesAStalePut(t *testing.T) {
	d := newNTDrop(t, false)
	d.seed(1)
	op := ntOperation()
	res, _ := d.mustTurn(&op)
	d.end(res)
	nu := d.users[0]
	if _, err := d.cp.PutUser(context.Background(), nu.u, nu.gen, entity.Alloc{}); !errors.Is(err, entity.ErrGenerationMismatch) {
		t.Errorf("PutUser at the generation read before the deletion: error %v, want ErrGenerationMismatch", err)
	}
	d.noneLeft("after the stale put")
}
