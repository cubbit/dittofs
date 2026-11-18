package identity_test

import (
	"bytes"
	"context"
	"testing"

	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// A hold is readable under its operation's ID until released; the release
// drops it and zeroes the caller's password slice, so the password is left
// in no memory the hold reached.
func TestHoldJoinCredentialRelease(t *testing.T) {
	s := newServer(t)
	id := entity.OperationID{1}
	pw := []byte("Admin-Passw0rd")
	release := s.HoldJoinCredential(id, pid.JoinCredential{User: "Administrator@AD.TEST", Password: pw})

	got, ok := s.HeldCredential(id)
	if !ok || got.User != "Administrator@AD.TEST" || !bytes.Equal(got.Password, []byte("Admin-Passw0rd")) {
		t.Fatalf("HeldCredential before release = %q, %q, %v; want the held user and password", got.User, got.Password, ok)
	}
	if _, ok := s.HeldCredential(entity.OperationID{2}); ok {
		t.Errorf("HeldCredential of another operation = held; want absent")
	}

	release()
	if _, ok := s.HeldCredential(id); ok {
		t.Errorf("HeldCredential after release = held; want absent")
	}
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Errorf("caller's password after release = %q; want every byte zero", pw)
	}
	release() // a second call is harmless
}

// A second hold under the same ID replaces the first, and the first's release
// leaves the second in place.
func TestHoldJoinCredentialReplaced(t *testing.T) {
	s := newServer(t)
	id := entity.OperationID{1}
	first := s.HoldJoinCredential(id, pid.JoinCredential{User: "a", Password: []byte("one")})
	second := s.HoldJoinCredential(id, pid.JoinCredential{User: "b", Password: []byte("two")})
	first()
	if got, ok := s.HeldCredential(id); !ok || got.User != "b" || string(got.Password) != "two" {
		t.Errorf("HeldCredential after the first release = %q, %q, %v; want the second hold", got.User, got.Password, ok)
	}
	second()
}

// Stop drops every hold and zeroes its password, so none outlives the server.
func TestStopDropsHolds(t *testing.T) {
	s := newServer(t)
	if err := s.Apply(state(1)); err != nil {
		t.Fatal(err)
	}
	id := entity.OperationID{1}
	pw := []byte("Admin-Passw0rd")
	release := s.HoldJoinCredential(id, pid.JoinCredential{User: "Administrator", Password: pw})
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := s.HeldCredential(id); ok {
		t.Errorf("HeldCredential after Stop = held; want absent")
	}
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Errorf("caller's password after Stop = %q; want every byte zero", pw)
	}
	release()
}
