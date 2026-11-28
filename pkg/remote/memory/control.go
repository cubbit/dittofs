package memory

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/remote"
)

// maxControl is the largest control object body the store holds: the S3
// store's bound, so a body refused there is refused here (RFC 4 §4.13).
const maxControl = 256 << 10

// controlKey is a role's key, control/ and its name (RFC 4 Appendix C.2),
// outside blocks/, so a listing never yields it. A role outside the closed
// set has no key.
func controlKey(role remote.Role) (string, error) {
	switch role {
	case remote.RoleClaim, remote.RoleHealth, remote.RoleKeys:
		return "control/" + role.String(), nil
	}

	return "", fmt.Errorf("memory: control object of %v, not a role: %w", role, remote.ErrInvalid)
}

// PutControl stores body as role's object, replacing the whole object there.
// A body over 256 KiB, or a role outside the set, is remote.ErrInvalid.
func (s *Store) PutControl(ctx context.Context, role remote.Role, body []byte) error {
	defer s.enter("put_control", remote.Name{})()

	if err := s.open("PutControl"); err != nil {
		return err
	}

	k, err := controlKey(role)
	if err != nil {
		return err
	}

	if len(body) > maxControl {
		return fmt.Errorf("memory: control object %v of %d bytes, over %d: %w", role, len(body), maxControl, remote.ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	s.ns.mu.Lock()
	s.ns.objects[k] = object{body: bytes.Clone(body), modTime: s.now()}
	s.ns.mu.Unlock()

	return nil
}

// GetControl returns a copy of role's object, and the empty Version, since the
// store keeps no versions. An absent object is remote.ErrNotFound; a role
// outside the set is remote.ErrInvalid.
func (s *Store) GetControl(ctx context.Context, role remote.Role) ([]byte, remote.Version, error) {
	defer s.enter("get_control", remote.Name{})()

	if err := s.open("GetControl"); err != nil {
		return nil, "", err
	}

	k, err := controlKey(role)
	if err != nil {
		return nil, "", err
	}

	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	s.ns.mu.Lock()
	o, ok := s.ns.objects[k]
	s.ns.mu.Unlock()

	if !ok {
		return nil, "", fmt.Errorf("memory: control object %v: %w", role, remote.ErrNotFound)
	}

	return bytes.Clone(o.body), "", nil
}

// rawKeys returns every key the store's namespace holds, relative to the
// prefix, in ascending order, whether the store is closed or not: what a test
// lists beneath the contract.
func (s *Store) rawKeys() []string {
	s.ns.mu.Lock()
	defer s.ns.mu.Unlock()

	keys := make([]string, 0, len(s.ns.objects))
	for k := range s.ns.objects {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// deleteRaw removes the object under key, relative to the prefix, beneath the
// contract: how a test removes an object the store owns.
func (s *Store) deleteRaw(key string) {
	s.ns.mu.Lock()
	delete(s.ns.objects, key)
	s.ns.mu.Unlock()
}
