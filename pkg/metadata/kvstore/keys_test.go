package kvstore

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

func TestKeyGolden(t *testing.T) {
	p, g, s := "aa000000000000000000000000000001", "bb000000000000000000000000000002", "cc000000000000000000000000000003"
	h := "0000000000000000000000000000000000000000000000000000000000000001"
	hash, alice := [32]byte{31: 1}, entity.Principal{Kind: entity.PrincipalUser, ID: pid}
	// A source is keyed masked: the host bits of each prefix below are set.
	v4, v6 := netip.MustParsePrefix("10.1.2.3/24"), netip.MustParsePrefix("2001:db8:0:1:ffff::1/64")
	epoch := entity.SignInFailures{Last: time.Unix(0, 0), Failed: time.Unix(0, 0)}
	// sf is k's count key, or with an f, the XP row of a count holding it.
	sf := func(k entity.SignInKey, f ...entity.SignInFailures) []byte {
		tag, tail, err := signInTail(k)
		if err != nil {
			t.Fatal(err)
		}
		if len(f) > 0 {
			return signInExpiryKey(tag, tail, f[0])
		}
		return signInKey(tag, tail)
	}
	for _, c := range []struct {
		name string
		key  []byte
		want string
	}{
		{"user", userKey(pid), "20" + p},
		{"group", principalKey(group), "21" + g},
		{"member", memberKey(gid, pid), "22" + g + p},
		{"member rev", memberRevKey(pid, gid), "23" + p + g},
		{"user name", nameKey(keys.NXUser, "alice"), "2501" + "616c696365" + "0001"},
		{"share name folds", nameKey(keys.NXShare, "Photos"), "2503" + "50484f544f53" + "0001"},
		{"path", pathKey("/a"), "26" + "2f61" + "0001"},
		{"paths below", pathsBelow("/a"), "26" + "2f61" + "2f"},
		{"protocol id", protocolKey(entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: "a@R"}), "2b04" + "614052" + "0001"},
		{"netgroup", netgroupKey("lab"), "27" + "6c6162" + "0001"},
		{"share info", shareKey(sid, keys.SInfo), "02" + s + "01"},
		{"grant", grantKey(sid, gid), "02" + s + "04" + g},
		{"grant rev", grantRevKey(gid, sid), "24" + g + s},
		{"grant-list version", shareKey(sid, keys.SGv), "02" + s + "1e"},
		{"setting, installation", settingKey(entity.SettingScope{Kind: entity.ScopeInstallation}, "k"), "3201" + "6b0001"},
		{"setting, share", settingKey(entity.SettingScope{Kind: entity.ScopeShare, ID: sid}, "k"), "3205" + s + "6b0001"},
		{"share list", shareListKey, "28"},
		{"installation", installationKey, "29"},
		{"settings generation", cfgGenKey, "33"},
		{"principals generation", pgenKey, "3b"},
		{"journal", journalKey(gid), "2e" + g},
		{"journal index", journalIndexKey(entity.NodeID(pid), "/j"), "48" + p + "2f6a0001"},
		{"removal step", shardRemovalKey(7), "2d" + "0000000000000007" + "04"},
		{"enabled admin", adminKey(pid), "4a" + p},
		{"audit", auditKey(time.Unix(0, 1), [16]byte(gid)), "45" + "0000000000000001" + g},
		{"audit by actor", auditActorPrefix(group), "4601" + g},
		{"audit by target", auditTargetPrefix(entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{0}}), "4602" + "05" + "00ff0001"},
		{"audit by node", auditNodePrefix(entity.NodeID(sid)), "4603" + s},
		{"operation", operationKey(entity.OperationID(gid)), "3c" + g},
		{"running operation", runningKey("k", entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{0}}), "3d" + "6b0001" + "05" + "00ff0001"},
		{"operation expiry", operationExpiryKey(entity.Operation{ID: entity.OperationID(gid), Updated: time.Unix(0, 2)}),
			"3f" + "0002260ff9290002" + "02" + g}, // 7 days after its end
		{"idempotency", idempotencyKey(group, "k"), "3e" + g + "6b0001"},
		{"idempotency expiry", idempotencyExpiryKey(entity.IdempotencyRecord{Account: group, Key: "k", Expires: time.Unix(0, 2)}),
			"3f" + "0000000000000002" + "01" + g + "6b0001"},
		{"namespace", namespaceInfoKey(entity.NamespaceID(gid)), "17" + g + "0a"},
		{"namespace share", namespaceShareKey(entity.NamespaceID(gid), sid), "17" + g + "0b" + s},
		{"store index", storeIndexKey([16]byte(pid), entity.NamespaceID(gid)), "4b" + p + g},
		{"session", sessionKey(pid), "40" + p},
		{"session's refresh hash", sessionHashKey(pid, hash), "40" + p + "01" + h},
		{"refresh hash", refreshKey(hash), "41" + h},
		{"account's session", accountSessionKey(alice, sid), "4c" + p + s},
		{"session expiry", sessionExpiryKey(entity.Session{ID: sid, Expires: time.Unix(0, 2)}), "3f" + "0000000000000002" + "03" + s},
		{"token", tokenKey(pid), "42" + p},
		{"token hash", tokenHashKey(hash), "43" + h},
		{"account's token", accountTokenKey(alice, sid), "44" + p + s},
		{"token expiry", tokenExpiryKey(entity.APIToken{ID: sid, Expires: time.Unix(0, 2)}), "3f" + "0000000000000002" + "05" + s},
		{"account's sign-in count", sf(entity.SignInKey{Account: alice}), "4901" + p},
		{"source's sign-in count", sf(entity.SignInKey{Source: v4}), "4902" + "0a010200" + "18"},
		{"IPv4-mapped source's sign-in count", sf(entity.SignInKey{Source: netip.MustParsePrefix("::ffff:10.1.2.3/120")}), "4902" + "0a010200" + "18"},
		{"IPv6 source's sign-in count", sf(entity.SignInKey{Source: v6}), "4902" + "20010db8000000010000000000000000" + "40"},
		{"pair's sign-in count", sf(entity.SignInKey{Account: alice, Source: v4}), "4903" + p + "0a010200" + "18"},
		{"source's count expiry", sf(entity.SignInKey{Source: v4}, epoch), "3f" + "000000d18c2e2800" + "04" + "0a010200" + "18"},
		{"pair's count expiry", sf(entity.SignInKey{Account: alice, Source: v4}, epoch), "3f" + "00004e94914f0000" + "06" + p + "0a010200" + "18"},
	} {
		if got := hex.EncodeToString(c.key); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// pathsBelow covers exactly the paths strictly below its argument.
func TestPathsBelow(t *testing.T) {
	below := pathsBelow("/home")
	for path, want := range map[string]bool{
		"/home/alice": true, "/home/a/b": true,
		"/home": false, "/homes": false, "/hom": false, "/home-x": false,
	} {
		if got := bytes.HasPrefix(pathKey(path), below); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

func TestFileKeyGolden(t *testing.T) {
	s, f := "cc000000000000000000000000000003", "f1000000000000000000000000000001"
	for _, c := range []struct {
		name string
		key  []byte
		want string
	}{
		{"file", fileKey(sid, fid), "01" + s + f},
		{"acl", aclKey(sid, fid), "01" + s + f + "01"},
		{"xattr", xattrKey(sid, fid, []byte("user.k")), "01" + s + f + "02" + "757365722e6b" + "0001"},
		{"entry", entryKey(sid, fid, 7, []byte("A")), "01" + s + f + "04" + "0000000000000007" + "41" + "0001"},
		{"delta", deltaKey(sid, fid, [32]byte{31: 1}), "01" + s + f + "05" + "0000000000000000000000000000000000000000000000000000000000000001"},
		{"release", releaseKey(sid, fid), "01" + s + f + "0d"},
		{"numbers", numberKey(sid), "02" + s + "16"},
	} {
		if got := hex.EncodeToString(c.key); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// The cursor after a digest sorts after every entry at that digest, whatever
// its name, and before every entry at the next.
func TestEntriesAfter(t *testing.T) {
	after := entriesAfter(sid, fid, 7)
	for _, name := range [][]byte{{}, {0}, {0xFF}, {0xFF, 0xFF, 0xFF}, []byte("zzz")} {
		if k := entryKey(sid, fid, 7, name); bytes.Compare(k, after) >= 0 {
			t.Errorf("digest 7, name %x sorts at or after the cursor", name)
		}
		if k := entryKey(sid, fid, 8, name); bytes.Compare(k, after) <= 0 {
			t.Errorf("digest 8, name %x sorts at or before the cursor", name)
		}
	}
}

// Describe names a key's parts, from the builders the store writes with.
func TestKeyDescribe(t *testing.T) {
	sh, f := entity.ShareID{0: 0x3f, 1: 0xa2, 2: 0xc1, 3: 0xd0}, entity.FileID{0: 0x9b, 1: 0x1e, 2: 0x44, 3: 0xa7}
	for _, c := range []struct {
		key  []byte
		want string
	}{
		{fileKey(sh, f), "F‖share=3fa2c1d0‖file=9b1e44a7"},
		{entryKey(sh, f, 0xabcdef01, []byte("README.md")), `F‖share=3fa2c1d0‖file=9b1e44a7‖e‖2882400001‖"README.md"`},
		{removalKey(sh, f, entity.VersionOf(7)), "F‖share=3fa2c1d0‖file=9b1e44a7‖rm‖v7"},
		{lockKey(sh, f, 9, 100, 4), "F‖share=3fa2c1d0‖file=9b1e44a7‖op‖9‖l‖100‖4"},
		{removalIndexKey(sh, 2, f, entity.VersionOf(7)), "S‖share=3fa2c1d0‖rr‖2‖file=9b1e44a7‖v7"},
		{clientShardKey(7, 2), "CL‖7‖sh‖2"},
		{shardKey(3), "SH‖3"},
		{clientNameKey(entity.ProtocolSMB, []byte("host")), `CLO‖4‖"host"`},
		{[]byte{0xFE, 1}, "?fe01"},
	} {
		if got := keys.Describe(c.key); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}
