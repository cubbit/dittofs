package identity

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// rotationPeriod is how long after its last confirm the machine password is
// rotated (RFC 18 §3.5).
//
// ponytail: the period is a constant 7 days, Samba's machine password
// timeout default, inside Windows' 30-day maximum for a member. Make it a
// setting when a domain whose maximum machine-password age is shorter than
// 7 days is shown to need it.
const rotationPeriod = 7 * 24 * time.Hour

// rotationRetry is how long after a failed rotation the next one starts.
const rotationRetry = time.Hour

// scheduleTick is how often the schedule checks whether a rotation is due.
//
// decision: the schedule checks on a fixed one-minute tick rather than
// waking at the due time, so a rotation starts up to a minute late and a
// join ending, or an API-started rotation moving the confirm, needs no wake.
// Overturned if a minute's lateness is shown to matter, or the check's one
// read-only write a minute is shown to load the store.
const scheduleTick = time.Minute

// errNotDue ends a schedule check's write with nothing written.
var errNotDue = errors.New("identity: no rotation is due")

// startSchedule starts the rotation's schedule unless it runs or the server
// has no control plane to write to. The caller holds s.mu.
func (s *Server) startSchedule() {
	if s.schedStop != nil || s.deps.ControlPlane == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go s.schedule(ctx, done)
	s.schedStop = func() { cancel(); <-done }
}

// stopSchedule stops the rotation's schedule and waits for its loop to end.
// The caller holds s.mu.
func (s *Server) stopSchedule() {
	if s.schedStop != nil {
		s.schedStop()
		s.schedStop = nil
	}
}

// schedule checks whether a rotation is due at once and then every
// scheduleTick on the server's clock, until ctx ends.
func (s *Server) schedule(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		if err := s.scheduleRotation(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("identity: the machine password rotation's schedule", "err", err)
		}
		tick := s.deps.Clock.After(scheduleTick)
		if s.scheduled != nil {
			s.scheduled()
		}
		select {
		case <-tick:
		case <-ctx.Done():
			return
		}
	}
}

// scheduleRotation creates a rotate-machine-password operation, started by
// the schedule, when the installation is joined, no join or rotation runs on
// it, and either the last rotation failed rotationRetry ago or, when it did
// not fail, it was confirmed rotationPeriod ago. The operation and its audit
// record are one write, and a check that finds none due writes nothing.
//
// decision: a failed rotation is retried an hour after its At rather than a
// period after its Confirmed, so failures in a row cannot push the rotation
// past the domain's maximum machine-password age; At is in the store, so a
// restart within the hour still waits it out. Overturned if a failure that
// the hourly retry cannot clear is shown to need a longer back-off.
//
// decision: the period reads Confirmed, but no state the store reaches today
// tells it apart from At: a confirm sets both to its time, and only a
// failure moves At alone, setting Failed. So no test can catch the period
// read from At. A writer that moves At without setting Failed is what makes
// the choice matter, and it brings the test that tells the two apart.
func (s *Server) scheduleRotation(ctx context.Context) error {
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpRotateMachinePassword), State: entity.OpPending}
	// The zero Actor, ActorName and Credential name the schedule.
	rec := entity.AuditRecord{Outcome: []byte(uuid.UUID(op.ID).String())}
	err := s.deps.ControlPlane.Write(ctx, entity.WriteMeta{Audit: &rec}, func(tx entity.ControlPlaneTx) error {
		in, err := tx.Installation()
		if err != nil {
			return err
		}
		if !in.Join.Joined {
			return errNotDue
		}
		lr := in.Join.LastRotation
		due := lr.Confirmed.Add(rotationPeriod)
		// decision: only a rotation's failure path writes Failed, so a
		// rotation that ends another way (a store error out of its step,
		// the runner giving up on it, a cancel from the API) leaves Failed
		// false and Confirmed as old as it was, and the next tick starts
		// another, once a minute with no back-off. The store has no tracked
		// read of the last ended operation of a kind to back off from, so
		// the operation runner's failure path must write Failed and At for
		// every rotation it ends. Holds while no runner drives a rotation;
		// overturned when one does without that write.
		if lr.Failed {
			due = lr.At.Add(rotationRetry)
		}
		if tx.Now().Before(due) {
			return errNotDue
		}
		op.Target = entity.ResourceRef{Kind: entity.ResourceInstallation, ID: in.ID[:]}
		if _, ok, err := tx.RunningOperation(string(apiv1.OpJoinDomain), op.Target); err != nil {
			return err
		} else if ok {
			return errNotDue
		}
		rec.Target = op.Target
		// A rotation already running holds the running index, so PutOperation
		// refuses a second one with ErrOperationRunning.
		_, err = tx.PutOperation(op, 0)
		return err
	})
	if errors.Is(err, errNotDue) || errors.Is(err, entity.ErrOperationRunning) {
		return nil
	}
	return err
}
