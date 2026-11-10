package fixture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cubbit/dittofs/pkg/apiclient"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// configure sets the server up through its API: its stores on the fixture's
// profile, with the blocks of an S3 profile in b, the users, and the shares
// with their settings and grants. Users get password.
//
// decision: a fixture that sets NFS or SMB adapter settings (lease,
// delegations, rpcbind, SMB encryption) is refused before anything is
// created, because this dfs serves no file protocol and answers no adapter
// settings route. Send those settings again once dfs serves NFS and SMB.
func (s *Server) configure(c *apiclient.Client, b *bucket, password string) error {
	f := s.f
	if f.Delegations != nil || f.Lease != "" || f.RPCBind != "" || f.SMB.Encryption != "" {
		return errors.New("the fixture sets NFS or SMB adapter settings (lease, delegations, rpcbind or SMB encryption), which this dfs cannot apply: it serves no file protocol")
	}
	meta := map[string]any{"in_memory": true}
	if f.Profile != "memory" { // badger and badger-s3 keep it on disk
		meta = map[string]any{"path": filepath.Join(s.data, "metadata")}
	}
	if _, err := c.CreateMetadataStore(&apiclient.CreateStoreRequest{Name: "default", Type: "badger", Config: meta}); err != nil {
		return fmt.Errorf("the metadata store: %w", err)
	}
	blocks := &apiclient.CreateStoreRequest{Name: "default", Type: "memory"}
	if b != nil {
		blocks.Type, blocks.Config = "s3", b.storeConfig()
	}
	if _, err := c.CreateBlockStore(blocks); err != nil {
		return fmt.Errorf("the block store: %w", err)
	}
	// The users come before the shares, since {user} owns a shares file's
	// shares.
	for _, u := range f.Users {
		uid, gid := ids(u)
		if _, err := c.CreateUser(&apiclient.CreateUserRequest{Username: userName(u), Password: password, UID: &uid, GID: &gid}); err != nil {
			return fmt.Errorf("user %s: %w", u, err)
		}
	}
	// decision: {user} and {user2} have UIDs and GIDs of their own, the first
	// two above the range users may name, since a user without one maps to
	// 1000, which a users entry or the developer may hold.
	for _, u := range []struct {
		name string
		id   uint32
	}{{User, UserUID}, {User2, User2UID}} {
		if _, err := c.CreateUser(&apiclient.CreateUserRequest{Username: u.name, Password: password, UID: &u.id, GID: &u.id}); err != nil {
			return fmt.Errorf("the test user %s: %w", u.name, err)
		}
	}
	// {user}'s principal maps to {user}, as the scripts' bootstrap maps it: a
	// client given the principal sends it as its NTLM user name too, which
	// the server looks up through the mapping.
	if f.Kerberos {
		if _, err := c.CreateIdentityMapping("kerberos", User+"@"+Realm, User); err != nil {
			return fmt.Errorf("the identity mapping of %s@%s: %w", User, Realm, err)
		}
	}
	for _, share := range s.shares {
		if err := addShare(c, f, share); err != nil {
			return err
		}
	}
	return nil
}

// addShare creates share on the default stores, with fixture f's squash
// and each of its users' read-write grant. A shares file's share has its root
// owned by {user}, who holds the admin grant on it: ownership lets {user}
// write at the root, and admin lets it take ownership of a file. On the
// default share {user} has no grant, unless the fixture mounts it over SMB,
// as {user}: then {user} holds the read-write grant the mount needs.
func addShare(c *apiclient.Client, f registry.Fixture, share apiclient.CreateShareRequest) error {
	share.MetadataStoreID, share.BlockStore = "default", "default"
	grant := ""
	switch {
	case f.Shares != "":
		grant, share.Owner = "admin", User
	case f.SMB.Mount:
		grant = "read-write"
	}
	if _, err := c.CreateShare(&share); err != nil {
		return fmt.Errorf("the share %s: %w", share.Name, err)
	}
	if grant != "" {
		if err := c.SetUserSharePermission(share.Name, User, grant); err != nil {
			return fmt.Errorf("%s's grant on %s: %w", User, share.Name, err)
		}
	}
	if f.Squash != "" {
		if _, err := c.PatchShareNFSConfig(share.Name, &apiclient.PatchShareNFSConfigRequest{Squash: &f.Squash}); err != nil {
			return fmt.Errorf("squash %s on %s: %w", f.Squash, share.Name, err)
		}
	}
	for _, u := range f.Users {
		if err := c.SetUserSharePermission(share.Name, userName(u), "read-write"); err != nil {
			return fmt.Errorf("user %s's grant on %s: %w", u, share.Name, err)
		}
	}
	return nil
}

// ids gives a fixture user's UID and GID: self is the user dt runs as, in its
// own group, and a UID's GID is the UID, as the suites' scripts set them up
// today.
func ids(user string) (uid, gid uint32) {
	if user == "self" {
		return uint32(os.Getuid()), uint32(os.Getgid())
	}
	n, _ := strconv.ParseUint(user, 10, 32) // the registry checks it's a UID
	return uint32(n), uint32(n)
}

// userName is a fixture user's name on the server.
func userName(user string) string {
	uid, _ := ids(user)
	return fmt.Sprintf("dt-%d", uid)
}

// check is the fixture's check before the first test that the server
// answers: an authenticated call answers. With an S3 profile's bucket b, the
// server's own check of its block store must find it healthy too.
func (s *Server) check(c *apiclient.Client, b *bucket) error {
	if _, err := c.GetCurrentUser(); err != nil {
		return fmt.Errorf("an authenticated call: %w", err)
	}
	if b == nil {
		return nil
	}
	h, err := c.BlockStoreHealth("default")
	if err != nil {
		return fmt.Errorf("the block store's health: %w", err)
	}
	if !h.Healthy {
		return fmt.Errorf("the server can't reach the bucket %s at %s: %s", bucketName, b.endpoint, h.Details)
	}
	return nil
}
