package kvstore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
)

var (
	jid  = [16]byte{0: 0xDD, 15: 0x0E}
	jrec = journalRecord{
		Node:   entity.NodeID{0: 0xEE, 15: 0x0F},
		Device: "/j1",
	}
	pid   = entity.PrincipalID{0: 0xAA, 15: 0x01}
	gid   = entity.PrincipalID{0: 0xBB, 15: 0x02}
	sid   = entity.ShareID{0: 0xCC, 15: 0x03}
	group = entity.Principal{Kind: entity.PrincipalGroup, ID: gid}
)

// codecCase is one entity, its encoding pinned, and a decoder of it.
type codecCase struct {
	name   string
	value  any
	enc    []byte
	golden string
	dec    func([]byte) (any, error)
}

func codecCases() []codecCase {
	user := entity.User{
		Principal:  entity.Principal{Kind: entity.PrincipalUser, ID: pid},
		Name:       "alice",
		IDs:        entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "1000"}},
		Primary:    group,
		Disabled:   true,
		Secret:     entity.SecretRef{15: 9},
		Email:      "a@x",
		Role:       entity.RoleAdmin,
		NotBefore:  time.Unix(1_800_000_000, 0).UTC(),
		MustChange: true,
		Generation: 5,
		Position:   11,
	}
	grp := entity.Group{Principal: group, Name: "staff", Generation: 2, Position: 12}
	share := entity.Share{ID: sid, Name: "Photos", Path: "/photos", Root: entity.FileID{15: 7}, Tag: 9,
		Fold: entity.FoldUpcase, Names: entity.NamesWindows, DigestKey: [32]byte{31: 0x5A}, Generation: 4, Position: 13}
	grant := entity.ShareGrant{Share: sid, Principal: group, Access: entity.AccessReadWrite, Version: 3}
	policy := entity.ExportPolicy{
		Share:         sid,
		Flavors:       []entity.AuthFlavor{entity.FlavorKerberos, entity.FlavorSys},
		MinProof:      entity.ProofTicket,
		MinProtection: entity.ProtectionIntegrity,
		ReadOnly:      true,
		Clients: []entity.ClientRule{
			{Match: entity.ClientMatch{CIDR: netip.MustParsePrefix("10.0.0.0/8")}, Access: entity.AccessRead},
			{Match: entity.ClientMatch{Netgroup: "lab"}, Access: entity.AccessReadWrite},
		},
		Squash:    entity.SquashRoot,
		Anonymous: entity.Everyone,
		SMB:       entity.SMBEncrypt | entity.SMBHidden,
		Version:   4,
	}
	ng := entity.Netgroup{Name: "lab", Members: []string{"a.example", "10.1.0.0/16"}, Version: 2}
	in := entity.Installation{ID: [16]byte{15: 1}, Instance: [16]byte{15: 2}, Generation: []byte{5}, SigningKey: entity.SecretRef{15: 8}}
	in.NTLMOff.Off, in.NTLMOff.Deleted = 6, entity.OperationID{15: 7}
	in.Join = entity.MachineJoin{Joined: true, Account: "H$", Owner: []byte{1}, Delegation: []byte{2}, Names: []string{"a"}}
	in.Join.LastRotation.At, in.Join.LastRotation.Failed = time.Unix(2, 3).UTC(), true
	in.Join.LastRotation.Tag, in.Join.LastRotation.Read, in.Join.LastRotation.Version = 4, 5, 6
	in.Join.LastRotation.Confirmed = time.Unix(7, 0).UTC()
	audit := entity.AuditRecord{ID: [16]byte{7: 0xAD}, Time: time.Unix(0, 1_000).UTC(), Actor: group, ActorName: "n", Credential: "k",
		Node: entity.NodeID{15: 1}, Source: "s", Request: []byte("r"),
		Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{9}}, Before: 1, After: 2, Change: []byte("c"), Outcome: []byte("o")}
	apos := auditPos(audit.Time, audit.ID)
	idem := entity.IdempotencyRecord{Account: group, Key: "k", Digest: [32]byte{31: 1}, Answer: []byte("a"), Expires: time.Unix(8, 9).UTC()}
	nsr := entity.Namespace{ID: entity.NamespaceID{15: 4}, Store: [16]byte{0: 0x5E}, Bound: []byte("b"), Generation: 3}
	alice := entity.Principal{Kind: entity.PrincipalUser, ID: pid}
	sess := entity.Session{ID: [16]byte{15: 0x51}, Account: alice, Refresh: [32]byte{31: 2},
		Created: time.Unix(3, 4).UTC(), Expires: time.Unix(5, 0).UTC(), Revoked: true}
	tok := entity.APIToken{ID: [16]byte{15: 0x52}, Account: alice, Hash: [32]byte{31: 3},
		Created: time.Unix(6, 0).UTC(), Expires: time.Unix(7, 0).UTC()}
	fails := entity.SignInFailures{Count: 3, Last: time.Unix(8, 0).UTC(), Failed: time.Unix(9, 1).UTC(), Nonce: 10}
	op := entity.Operation{ID: entity.OperationID{15: 4}, Kind: "k", Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: []byte{9}},
		Account: group, State: entity.OpFailed, Claim: entity.OpClaim{Node: entity.NodeID{15: 1}, Epoch: 2, Expires: time.Unix(3, 0).UTC()},
		Step: []byte("s"), Params: []byte("p"), Started: time.Unix(4, 0).UTC(), Updated: time.Unix(5, 6).UTC(),
		Error: []byte("e"), Result: []byte("r"), Generation: 7}
	node := entity.Node{ID: entity.NodeID{15: 5}, Address: "a", Roles: entity.RoleProtocol | entity.RoleStorage, Binary: "b",
		Domain: "d", Epoch: 2, Formats: map[entity.Format]entity.VersionRange{2: {Min: 3, Max: 4}, 1: {Min: 1, Max: 1}}, Lapsed: true}
	status := entity.NodeStatus{Node: node.ID, Settings: 1, ShareList: 2, Principals: 3, Claims: 4,
		Refused:        []entity.RefusedRecord{{Record: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{9}}, Generation: 5, Error: []byte("e")}},
		RestartPending: []entity.ResourceRef{{Kind: entity.ResourceSetting, ID: []byte{8}}},
		Secrets:        []entity.SecretVersion{{Secret: entity.SecretRef{15: 7}, Version: 6}}, Conditions: []byte("c")}
	set := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "k", Value: []byte("v"), Generation: 6, Schema: 1, Position: 14}
	return []codecCase{
		{"user", user, encodeUser(user),
			"01" + "05616c696365" + "01" + "0104" + "31303030" + "02" + "bb000000000000000000000000000002" + "01" + "00000000000000000000000000000009" +
				"03614078" + "03" + "000000006b49d200" + "01" + "0000000000000005" + "000000000000000b",
			func(b []byte) (any, error) { return decodeUser(pid, b) }},
		{"group", grp, encodeGroup(grp), "01" + "057374616666" + "00" + "0000000000000002" + "000000000000000c",
			func(b []byte) (any, error) { return decodeGroup(gid, b) }},
		{"kind", entity.PrincipalGroup, encodeKind(entity.PrincipalGroup), "0102",
			func(b []byte) (any, error) { return decodeKind(b) }},
		{"id", [16]byte(sid), encodeID(sid), "01" + "cc000000000000000000000000000003",
			func(b []byte) (any, error) { return decodeID(b) }},
		{"principal", group, encodePrincipal(group), "01" + "02" + "bb000000000000000000000000000002",
			func(b []byte) (any, error) { return decodePrincipal(b) }},
		{"share", share, encodeShareInfo(share),
			"01" + "0650686f746f73" + "072f70686f746f73" + "00000000000000000000000000000007" +
				"00000002" + "02" + "000000000000000000000000000000000000000000000000000000000000005a" + "00000009" + "0000000000000004" + "000000000000000d",
			func(b []byte) (any, error) { return decodeShareInfo(sid, b) }},
		{"state", shareState{State: entity.ShareRemoving, Step: 1, Snapshots: entity.SnapshotsDetach},
			encodeState(shareState{State: entity.ShareRemoving, Step: 1, Snapshots: entity.SnapshotsDetach}), "01" + "03" + "01" + "02",
			func(b []byte) (any, error) { return decodeState(b) }},
		{"grant", grant, encodeGrant(grant), "01" + "02" + "02" + "0000000000000003",
			func(b []byte) (any, error) { return decodeGrant(sid, gid, b) }},
		{"policy", policy, encodePolicy(policy),
			"01" + "02" + "0302" + "03" + "01" + "01" + "02" +
				"01" + "05" + "0a00000008" + "01" +
				"02" + "036c6162" + "02" +
				"01" + "03" + "00000000000000000000000000000003" + "03" + "0000000000000004",
			func(b []byte) (any, error) { return decodePolicy(sid, b) }},
		{"netgroup", ng, encodeNetgroup(ng),
			"01" + "02" + "09612e6578616d706c65" + "0b31302e312e302e302f3136" + "0000000000000002",
			func(b []byte) (any, error) { return decodeNetgroup("lab", b) }},
		{"installation", in, encodeInstallation(in),
			"01" + "00000000000000000000000000000001" + "00000000000000000000000000000002" + "0105" +
				"0000000000000006" + "00000000000000000000000000000007" + "00000000000000000000000000000008" +
				"01" + "024824" + "0101" + "0102" + "01" + "0161" +
				"0000000000000002" + "00000003" + "01" + "00000004" + "00000005" + "0000000000000006" +
				"0000000000000007" + "00000000",
			func(b []byte) (any, error) { return decodeInstallation(b) }},
		{"audit", audit, encodeAudit(audit),
			"01" + "02" + "bb000000000000000000000000000002" + "016e" + "016b" + "00000000000000000000000000000001" + "0173" + "0172" +
				"05" + "0109" + "0000000000000001" + "0000000000000002" + "0163" + "016f",
			func(b []byte) (any, error) { return decodeAudit(apos, b) }},
		{"idempotency", idem, encodeIdempotency(idem),
			"01" + "0000000000000000000000000000000000000000000000000000000000000001" + "0161" +
				"0000000000000008" + "00000009",
			func(b []byte) (any, error) { return decodeIdempotency(group, "k", b) }},
		{"operation", op, encodeOperation(op),
			"01" + "016b" + "08" + "0109" + "02" + "bb000000000000000000000000000002" + "04" +
				"00000000000000000000000000000001" + "0000000000000002" + "0000000000000003" + "00000000" +
				"0173" + "0170" + "0000000000000004" + "00000000" + "0000000000000005" + "00000006" +
				"0165" + "0172" + "0000000000000007",
			func(b []byte) (any, error) { return decodeOperation(op.ID, b) }},
		{"node", node, encodeNode(node),
			"01" + "0161" + "03" + "0162" + "0164" + "0000000000000002" + "02" +
				"01" + "00000001" + "00000001" + "02" + "00000003" + "00000004" + "01",
			func(b []byte) (any, error) { return decodeNode(node.ID, b) }},
		{"node status", status, encodeNodeStatus(status),
			"01" + "0000000000000001" + "0000000000000002" + "0000000000000003" + "0000000000000004" +
				"01" + "05" + "0109" + "0000000000000005" + "0165" + "01" + "0c" + "0108" +
				"01" + "00000000000000000000000000000007" + "0000000000000006" + "0163",
			func(b []byte) (any, error) { return decodeNodeStatus(node.ID, b) }},
		{"counter", uint64(7), encodeCounter(7), "01" + "0000000000000007",
			func(b []byte) (any, error) { return decodeCounter(b) }},
		{"setting", set, encodeSetting(set), "01" + "0176" + "0000000000000006" + "00000001" + "000000000000000e",
			func(b []byte) (any, error) { return decodeSetting(set.Scope, "k", b) }},
		{"journal index", jid, encodeJournalIndex(jid), "01" + "dd00000000000000000000000000000e",
			func(b []byte) (any, error) {
				return decodeJournalIndex(b)
			}},
		{"journal", jrec, encodeJournal(jrec), "01" + "ee00000000000000000000000000000f" + "03" + "2f6a31",
			func(b []byte) (any, error) {
				return decodeJournal(b)
			}},
		{"namespace", nsr, encodeNamespace(nsr),
			"01" + "5e000000000000000000000000000000" + "0162" + "0000000000000003",
			func(b []byte) (any, error) { return decodeNamespace(nsr.ID, b) }},
		{"session", sess, encodeSession(sess),
			"01" + "01" + "aa000000000000000000000000000001" +
				"0000000000000000000000000000000000000000000000000000000000000002" +
				"0000000000000003" + "00000004" + "0000000000000005" + "00000000" + "01",
			func(b []byte) (any, error) { return decodeSession(sess.ID, b) }},
		{"api token", tok, encodeToken(tok),
			"01" + "01" + "aa000000000000000000000000000001" +
				"0000000000000000000000000000000000000000000000000000000000000003" +
				"0000000000000006" + "00000000" + "0000000000000007" + "00000000",
			func(b []byte) (any, error) { return decodeToken(tok.ID, b) }},
		{"sign-in failures", fails, encodeSignIn(fails),
			"01" + "00000003" + "0000000000000008" + "00000000" + "0000000000000009" + "00000001" + "000000000000000a",
			func(b []byte) (any, error) { return decodeSignIn(b) }},
	}
}

func TestCodecs(t *testing.T) {
	for _, c := range codecCases() {
		t.Run(c.name, func(t *testing.T) {
			if got := hex.EncodeToString(c.enc); got != c.golden {
				t.Errorf("encoding\n got %s\nwant %s", got, c.golden)
			}
			got, err := c.dec(c.enc)
			if err != nil || !reflect.DeepEqual(got, c.value) {
				t.Fatalf("round trip: %+v, %v", got, err)
			}
			if _, err := c.dec(append([]byte{2}, c.enc[1:]...)); !errors.Is(err, codec.ErrVersion) {
				t.Errorf("unknown version: %v", err)
			}
			if _, err := c.dec(append(c.enc, 0)); !errors.Is(err, codec.ErrTrailing) {
				t.Errorf("trailing byte: %v", err)
			}
			for n := range len(c.enc) {
				if _, err := c.dec(c.enc[:n]); err == nil {
					t.Errorf("truncated to %d bytes: decoded", n)
				}
			}
		})
	}
}

func TestPolicyUnknownMatchTag(t *testing.T) {
	b := codec.NewWriter(v1).Len(0).Uint8(0).Uint8(0).Bool(false).Len(1).Uint8(9).Encode()
	if _, err := decodePolicy(sid, b); !errors.Is(err, errMalformed) {
		t.Errorf("unknown tag: %v", err)
	}
}

func TestUserUnknownRole(t *testing.T) {
	b := encodeUser(entity.User{Name: "u", Role: entity.APIRole(200)})
	if _, err := decodeUser(entity.PrincipalID{}, b); !errors.Is(err, errMalformed) {
		t.Errorf("user with role byte 200: %v, want %v", err, errMalformed)
	}
}

func TestAuditUnknownKind(t *testing.T) {
	a := entity.AuditRecord{Target: entity.ResourceRef{Kind: entity.ResourceSession + 1}}
	if _, err := decodeAudit(auditPos(a.Time, a.ID), encodeAudit(a)); !errors.Is(err, errMalformed) {
		t.Errorf("audit record targeting kind %d: %v, want %v", a.Target.Kind, err, errMalformed)
	}
}

// Each control-plane decoder, on any bytes, either decodes a value that is
// written again to bytes decoding to the same value, or fails with a named
// error.
func FuzzControlPlaneValues(f *testing.F) {
	decoders := map[string]func([]byte) (any, error){}
	for _, c := range append(codecCases(), fileCodecCases()...) {
		switch c.name {
		case "user", "group", "share", "setting", "quota", "installation", "audit", "idempotency", "namespace",
			"session", "api token", "sign-in failures", "operation", "node", "node status":
			decoders[c.name] = c.dec
			f.Add(c.enc)
			f.Add(c.enc[:len(c.enc)/2])
			f.Add(append(bytes.Clone(c.enc), 0))
		}
	}
	encoders := map[string]func(any) []byte{
		"user":             func(v any) []byte { return encodeUser(v.(entity.User)) },
		"group":            func(v any) []byte { return encodeGroup(v.(entity.Group)) },
		"share":            func(v any) []byte { return encodeShareInfo(v.(entity.Share)) },
		"setting":          func(v any) []byte { return encodeSetting(v.(entity.Setting)) },
		"operation":        func(v any) []byte { return encodeOperation(v.(entity.Operation)) },
		"node":             func(v any) []byte { return encodeNode(v.(entity.Node)) },
		"node status":      func(v any) []byte { return encodeNodeStatus(v.(entity.NodeStatus)) },
		"quota":            func(v any) []byte { return encodeQuota(v.(entity.Quota)) },
		"installation":     func(v any) []byte { return encodeInstallation(v.(entity.Installation)) },
		"audit":            func(v any) []byte { return encodeAudit(v.(entity.AuditRecord)) },
		"idempotency":      func(v any) []byte { return encodeIdempotency(v.(entity.IdempotencyRecord)) },
		"namespace":        func(v any) []byte { return encodeNamespace(v.(entity.Namespace)) },
		"session":          func(v any) []byte { return encodeSession(v.(entity.Session)) },
		"api token":        func(v any) []byte { return encodeToken(v.(entity.APIToken)) },
		"sign-in failures": func(v any) []byte { return encodeSignIn(v.(entity.SignInFailures)) },
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		for name, dec := range decoders {
			v, err := dec(b)
			if err != nil {
				if !errors.Is(err, codec.ErrVersion) && !errors.Is(err, codec.ErrTruncated) &&
					!errors.Is(err, codec.ErrTrailing) && !errors.Is(err, errMalformed) {
					t.Errorf("%s %x: unnamed error %v", name, b, err)
				}
				continue
			}
			if again, err := dec(encoders[name](v)); err != nil || !reflect.DeepEqual(again, v) {
				t.Errorf("%s %x: decoded %+v, written again %+v, %v", name, b, v, again, err)
			}
		}
	})
}
