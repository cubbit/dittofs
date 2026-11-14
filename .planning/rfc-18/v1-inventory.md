# RFC 18 identity: inventory of the v1 code it replaces

- Date: 2026-11-14
- Sources read: `origin/feat/storage-v2` at `a727ee02e`, `origin/develop` at `9a3f2e00c`; RFC 18 and
  its plan from PR #4058's head `f7e0fbd13` (`docs/internals/rfcs/rfc-18-identity.md`,
  `docs/internals/planning/rfc-18-identity-plan.md`).
- Line counts: `cat <non-test *.go> | wc -l` per package directory, non-recursive (raw lines,
  comments and blanks included), excluding `*_test.go`. Test counts are files / raw lines of
  `*_test.go` in the same directory. "unit" means no build tag.
- Epics are the plan's §2: E1 contract and surface, E2 principals and accounts, E3 resolution,
  E4 local NTLMv2 sign-in, E5 Kerberos and the PAC, E6 the directory, E7 NETLOGON and the machine
  account, E8 integration, E9 E2E green. "RFC 23 E1" is RFC 23's plan's epic 1, which deletes v1
  control-plane bodies; "O2" is the plan's deferral of the NFS side.

Verdicts: **reuse** (as is), **reuse+chg** (move or lift, then rewrite against RFC 18),
**ref** (read for prior art, do not carry), **delete**.

## 1. Still on `feat/storage-v2`

### 1.1 Identity core

| Package | What it does | Src | Tests | Epic | Verdict and reason |
| --- | --- | --- | --- | --- | --- |
| `pkg/identity` (identity.go, resolver.go, cache.go, store.go) | Provider chain resolving a `Credential` to a `ResolvedIdentity`; TTL cache (5 min / 30 s neg / 10 s error); `LinkStore` of principal-to-username links | 4 f, 572 | 1 f, 522; unit; uses a `mockProvider` | E1 reuses the path; RFC 23 E1 deletes it | **delete**. §5.2/§5.4: errors are never cached and the directory cache has stale grace; §4.1 has one mapping, not a link table. Same on develop. |
| `pkg/identity/ldap` (provider.go 949, config.go 289, test.go 62) | AD/LDAP provider: user@REALM or SID to uid/gid via RFC2307 attrs or `rid` mode; nested groups; `ResolvePrincipalSID`; reverse uid/gid/SID lookups | 3 f, 1300 | 4 f, 880; unit against a `fakeConn`; real AD only in `ad-dc` | E6 | **reuse+chg** (lifted into `internal/identity/directory`). Must change: ambiguous match refuses (§5.1, today warns and takes the first); verified TLS only, so `allow_plaintext` and `insecure_skip_verify` go (§5.1); `rid` mode's uid = gid = RID goes (§4.3, directory uids minted once); bind password becomes a `Secret`; add paging, range retrieval, `tokenGroups`. Tests must drop `fakeConn` for the in-memory directory held to the same suite. Develop identical (1301 lines). |
| `pkg/auth/sid` sid.go (190), wellknown.go (55) | MS-DTYP SID codec, parse/format, well-known names | 2 f, 245 | shared 1 f, 623; unit | E1/E2 consumers | **reuse**. Plan §1.5 rule 7: codec and well-known SIDs stay where they are. Needs `FuzzSID` (§10) if it has none. |
| `pkg/auth/sid` mapper.go (344) | Machine SID plus Samba formula RID = 2*uid+1000 (groups 2*gid+1001); uid 0 to `S-1-5-32-544` | 1 f, 344 | in the 623 above | E2 (RID counter replaces it) | **delete**. §2.3/§4.4: a RID comes from a counter, never from a uid. Deleted by RFC 23 E1. |
| `internal/auth/netlogon` | MS-NRPC secure channel (go-msrpc), `NetrLogonSamLogon` pass-through, machine-password rotation (`RotationManager`), online join over LDAP (join.go), DC discovery by DNS SRV, offline/mutable credential providers, status controller | 10 f, 2283 | 10 f, 1646; unit; uses `fakeLDAP`, `fakeChannel`, `fakeState` | E7 | **reuse+chg** (`git mv` to `internal/identity/netlogon`). Must change: join sets `msDS-SupportedEncryptionTypes` = `0x1F` (join.go:41; A4 wants `0x18`, AES only); seal-before-create and resumable join, recorded DC, offline join's closing rotation (A3, A7, A8); previous-version fallback with retention; `SecretStore` becomes RFC 13 `Secret` records with `KeyVersion`/`HandedOver`; status becomes the `machine_account_unhealthy` condition; pass-through keys MIC with `UserSessionKey` and checks `LogonDomainID` (A7). Existing unit tests use fakes that CLAUDE.md bans, so they are rewritten on the in-memory DC held to the DC suite. Develop identical. |
| `internal/vfs/admit` (admit.go 103, resolve.go 157) | Admission: export policy check, then `Resolve` (proven or claimed caller to principal, squash, grant, root, anonymous) | 2 f, 260 | 1 f, 141; unit | E3 | **reuse+chg**. Plan: moved into identity and completed as `identity.Resolver` (§6). Already v2-shaped (reads RFC 16 `entity` records); needs groups, flags, proof key and caller cache keyed per §7.1. |
| `internal/vfs/authz` (109) | The filesystem's one caller of the store's authorisation chokepoint; its own `Caller` | 1 f, 109 | 1 f, 101; unit | E3 | **reuse+chg**. Rebuilt on RFC 18 §6.1's `Caller` (plan §1.3). |
| `internal/vfs/shares.go` `SignIn`, `Spell`, `Find` (lines 337-347) | Stubs returning `ErrAccess` | ~12 lines | in shares_test.go | E3, E4 | **delete** the stubs; bodies call `vfs.Deps.Auth` and `Names`. |
| `pkg/vfs` `Identity` (types.go:119), `Principals` (service.go:188), SID fields | v2 value types and the `Principals` interface the adapters see | part of types.go (482) and service.go | unit | E1 | **reuse** (moved into `pkg/identity`, aliased from `pkg/vfs`). |
| `pkg/metadata/entity` `User`, `Group`, `Principal` | RFC 16 records already merged | control.go 389, id.go | unit | E2 | **reuse**. Note: no `Minter` and no NT-hash/`Secret` fields on the branch yet (A5, A11 add them in RFC 16). |

### 1.2 Control plane, API, CLI (v1 GORM models and handlers)

All of these exist identically on develop (handlers within a few dozen lines).

| Area | Files (src lines) | Tests | Epic | Verdict and reason |
| --- | --- | --- | --- | --- |
| Models | `models/user.go` 164 (bcrypt hash plus hex NT hash always stored), `user_grace.go` 41, `group.go` 73, `admin.go` 71 (bootstrap admin uid 0), `identity_mapping.go` 25, `identity_provider.go` 55, `sid_mapping.go` 53, `sid_share_permission.go` 56, `permission.go` 163 (five squash modes, `default_permission`); total 701 | none | RFC 23 E1 | **delete**. §3.5 (no NT hash with NTLM off; A5 two sealed secrets), §2.4/§8 (roles grant nothing on files), §6.2 (squash none/root/all), §6.4 (everyone grant, no default). |
| Store | `store/identity.go` 74, `identity_provider.go` 46, `sid_mapping.go` 96, `users.go` 314, `groups.go` 229, `permissions.go` 334; total 1093 | 2 f, 360; `integration` tag | RFC 23 E1 | **delete**. Replaced by RFC 16 KV records and the `Minter`. |
| API handlers | `identity_mappings.go` 181, `identity_providers.go` 495, `netlogon.go` 114, `sid_mappings.go` 89, `users.go` 560, `groups.go` 305; total 1744 | 5 f, 2006 (users, groups, sid_mappings under `integration`) | RFC 23 E1; E2 builds `APISignIn` | **delete**; `users.go` is **ref** for the API sign-in flow (§8.1 is a new interface). The identity-provider handler is the only branch importer of gokrb5 (plan §1.1), so E5 re-adds the dependency. |
| `pkg/controlplane/runtime/share_grants.go` | 165 | none | E3 | **ref**. §6.4 grants by principal ID; SID-keyed grants dropped. |
| `pkg/apiclient` | `identity_mappings.go` 60, `identity_providers.go` 144, `sid_mappings.go` 22, `netlogon.go` 49 | 1 f, 78 | RFC 23 E1 | **delete**. |
| `cmd/dfs` | `netlogon.go` 196, `netlogon_cmd.go` 105 (`dfs netlogon test`) | 1 f, 351 | RFC 23 E1 (§6 O5) | **delete**. Replaced by RFC 23's directory-test action. |
| `cmd/dfsctl` | `idmap/` 7 f 393, `netlogon/` 146, `identityprovider/` 375, `share/permission/` 5 f 370 | identityprovider 1 f, 201 | RFC 23 E1 | **delete**; not rebuilt (plan §1.1). |
| `pkg/config` `kerberos.*`, `ldap.*`, `identity.machine_sid` | inside config.go | | RFC 13 plan | **delete**; refused by the bootstrap loader, replaced by `identity.*` settings and `Secret`s. |

## 2. Only on `develop` (deleted from storage-v2 by RFC 20 epic 1, #3916)

### 2.1 Kerberos and identity glue

| Package | What it does | Src | Tests | Epic | Verdict and reason |
| --- | --- | --- | --- | --- | --- |
| `internal/auth/kerberos` (service.go 516, replay.go 94, gsswrap.go 76, doc.go 13) | AP-REQ verify, session key and subkey, AP-REP mutual auth, authenticator replay cache, PAC extraction | 4 f, 699 | 4 f, 712; unit; real gokrb5 keytabs and tickets built in the test (no KDC) | E5 | **reuse+chg** (lifted from history, plan rule 7). Must change: a PAC that fails its signature fails the sign-in (§3.4; today proceeds with no SIDs, dropping deny-only SIDs); server checksum over PAC bytes as received, first buffer of a type only (A6); AES only, DES/RC4 refused (A4); client-address and service-name checks (§3.3); replay entry kept until authenticator time plus skew (A10). Its test style (tickets from a test realm's keys) already matches plan rule 1. |
| `pkg/auth/kerberos` (provider.go 217, keytab.go 203, identity.go 70, doc.go 18) | Keytab and krb5.conf loading with env overrides, file-polling hot reload, principal-to-username with `StripRealm` | 4 f, 508 | 2 f, 520; unit | E5 | **ref**. Keytab arrives through `Apply` from sealed `Secret`s (§3.3, A3), not a polled file or `DITTOFS_KERBEROS_*`; stripping service-name prefixes conflicts with machine principals (§2.4). |
| `pkg/identity/kerberos` | Principal to user via `LinkStore` then realm-stripped lookup | 1 f, 120 | 1 f, 256; unit | none | **delete**. §4.2: Kerberos names map through one mapping; no link table. |
| `pkg/auth/auth.go` | `Result` / `Translator` shared auth model | 1 f, 73 | 1 f, 44 | E1 | **ref**. Superseded by RFC 18 §3.1's interface. |
| `pkg/adapter/identity.go` | Builds the resolver from the runtime; defaults a missing uid/gid to 1000 | 1 f, 150 | 1 f, 101 | none | **delete**. §2.3: no default uid; this default is the cache-key bug CLAUDE.md records. |
| `pkg/controlplane/runtime/identity` | Squash modes incl. `root_to_admin`, `all_to_admin`; AUTH_NULL to anonymous | 2 f, 100 | 1 f, 246 | E3 | **delete**. §6.2: `none`/`root`/`all`, admin only from a grant (ID6). |

### 2.2 SMB adapter pieces (RFC 22 owns the wire; RFC 18 owns the decision)

| Code | What it does | Src | Tests | Epic | Verdict and reason |
| --- | --- | --- | --- | --- | --- |
| `internal/adapter/smb/auth/ntlm.go` | NTLM message parse/build, CHALLENGE, NTLMv2 verification, session base key, MIC, key exchange | 1279 | dir: 7 f, 2692; unit; MS-NLMP vectors and first-principles cross-checks | E4 (crypto); parser is RFC 20/22's | **reuse+chg** for the NTLMv2 cryptography only (plan rule 7). Must change: MIC enforced (§3.5), refuse 24-byte `NtChallengeResponse` (A10), session base key `HMAC_MD5(NTOWFv2, NTProofStr)` and MIC keyed by exported session key (A8). |
| `internal/adapter/smb/auth/spnego.go` | SPNEGO parse/build over gokrb5 | 433 | in the 2692 | RFC 22 (`gss`) | **ref** for RFC 18; not identity's. |
| `handlers/session_setup.go` | SESSION_SETUP: mechanism dispatch, NTLM retry over five domain spellings, NETLOGON pass-through only for users not in the store, guest fallback | 2426 | 1 f, 1405 + 6 more session_setup_* files; unit | E4, E7 | **ref**, and a trap: guest on failure (§3.6 "no map to guest"), five domain spellings (§3.5 one route per message), MIC unenforced. |
| `handlers/kerberos_auth.go` | SMB Kerberos path: AP-REQ via the service, AP-REP, signing key | 724 | 1 f, 186 + kerberos_identity_test | E5 | **ref** for key derivation and AP-REP; the decision moves into identity. |
| `handlers/auth_helper.go` 549, `auth_revalidate.go` 356, `identity_resolver.go` 69, part of `session_lifecycle.go` 1133 | Building the auth context, per-request revalidation, SID-to-uid mapping | 974 (+ lifecycle) | many unit files | E3, E4 (§3.8) | **ref**. Revalidation logic is prior art for re-authentication and §7.2's one-second propagation. |
| `internal/adapter/smb/rpc/lsarpc.go` 827, `identity.go` 31 | LSARPC `LookupSids` (no `LookupNames`; `SOME_NOT_MAPPED` for unknown) | 858 | 1 f, 1748 | RFC 22; feeds §4.6 `Spell`/`Find` | **ref**. |

### 2.3 NFS side (deferred by the plan, O2)

| Code | Src | Tests | Verdict |
| --- | --- | --- | --- |
| `internal/adapter/nfs/rpc/gss` (RPCSEC_GSS framework, integrity, privacy, sequence) | 7 f, 2669 | 8 f, 4174; unit | **ref** for v1.1. Unmapped principal acts as `nobody` (§3.3 refuses). |
| `internal/adapter/nfs/auth` (share_permission.go, export_access.go, auth_context.go, kerberos_level.go, unix.go) | 6 f, 548 | 8 f, 969; unit | **ref** for v1.1; root without a record, unknown uid as guest with `default_permission` (Appendix B). |
| `pkg/adapter/nfs/identity` (static mapper, `ParsePrincipal`) | 2 f, 197 | 2 f, 202 | **delete**. |
| NFSv4 owner strings in `v4/attrs/encode.go` / `decode.go` | part of 896 | | **ref** for v1.1 (§4.5). |

## 3. Test infrastructure for AD and Kerberos

| Item | Where | What it is | Needs | State on storage-v2 |
| --- | --- | --- | --- | --- |
| `integration` option `ad-dc` | `test/suites.yaml` | `go test -tags=ad_dc -timeout=20m ./test/integration/ad-dc/`, tier `merge` (blocking after merge and nightly) | `linux`, `docker`, `smbclient` | present |
| `test/integration/ad-dc` | 8 test files, 1577 lines (develop: 9 files, 2099, plus `ad_dc_keytab_test.go`) | Samba AD DC fixture: `Dockerfile` (debian bookworm, `samba-ad-dc`, `krb5-user`, `ldb-tools`), `entrypoint.sh` provisions a domain with `--use-rfc2307`, users with and without uidNumber, exports a service keytab to `/keytabs`. Tests `docker build`/`docker run` it themselves, started once per package. Covers LDAP resolution, NETLOGON pass-through and reload, online join, an `smbclient` logon against a built `dfs` | docker on the host; no env var; skips in `-short` or without docker/smbclient | imports `pkg/identity`, `pkg/identity/ldap`, `internal/auth/netlogon`, `pkg/apiclient`, so it breaks when RFC 23 E1 deletes them; plan rewrites it in E6/E7. LDAP test sets `InsecureSkipVerify: true` (conflicts with §5.1, the fixture needs a CA it can verify). The smbclient test needs an SMB adapter serving through the node (not on the branch; not run here). |
| `integration` option `kerberos` | `test/suites.yaml` | `go test -tags=kerberos ./test/integration/kerberos/` | `docker` | **dangling**: the entry remains but `test/integration/kerberos/` was deleted. On develop: MIT KDC image (`krb5-kdc`, `krb5-admin-server`) plus a 28 KB test file. |
| `e2e` option `kerberos` | `test/suites.yaml`; `test/e2e/*kerberos*_test.go` (5 files), `framework/kerberos.go` | `-tags=e2e -run Kerberos`, tier `pr`, path-filtered; `DITTOFS_E2E_REQUIRE_KERBEROS=1` turns a skip into a failure | `linux`, `root`, `nfs-client`, `smb-client`, `nfs4-acl-tools`, `service:localstack` (`dt services up localstack`) | **broken**: `KDCHelper` builds its testcontainer from `test/integration/kerberos`, which is gone, and the adapters are gone. Plan E9 adds its Kerberos rows here. |
| `smb-conformance` option `kerberos` | `test/suites.yaml`; `test/smb-conformance/kdc/` (MIT KDC Dockerfile, entrypoint); `known-kerberos.yaml` | smbtorture `smb2.session`, `smb2.read`, `smb2.lock` with `--use-kerberos=required`, tier `merge` | `linux`, `docker`; fixture `kerberos: true` | present; fixture needs an SMB adapter |
| Harness KDC | `test/harness/fixture/kerberos.go` | `startKDC` runs image `dittofs-dt-kdc` (built by `dt` from `test/smb-conformance/kdc`), writes keytab and krb5.conf for `cifs/<host>` | docker | `fixture/api.go:63` calls `CreateIdentityMapping("kerberos", user@realm, user)`, a v1 API RFC 23 E1 deletes; must move to RFC 18's mapping |
| `dt services` | `test/harness/env.sh`, `cmd/dt/env.go` | `localstack`, `postgres`, `kmip` only | docker | no AD or KDC service; each suite starts its own container |
| `needs` | `test/harness/needs/needs.go`, `registry/check.go` | `kerberos` need = `kinit` (`krb5-user`), satisfied inside `dtc` | | `dtc`'s dev image installs `krb5-user`, `smbclient`, `cifs-utils` |
| lint-go | `test/suites.yaml` | `go vet -tags=...ad_dc,...,kerberos,...` | | keeps the tagged files compiling; drop `kerberos` when the dangling option goes |

## 4. Totals (non-test lines)

- Carried forward with changes: netlogon 2283, LDAP 1300, develop Kerberos service 699, NTLMv2 crypto
  within ntlm.go (part of 1279), admit 260, authz 109; about 5,900 lines of starting material.
- Kept as is: SID codec and well-known 245; `pkg/vfs` value types (moved).
- Deleted on storage-v2: `pkg/identity` 572, `sid/mapper.go` 344, control-plane models/store/handlers/
  apiclient/CLI about 4,700.
- Reference only: SMB session setup and Kerberos handler about 3,150, `pkg/auth/kerberos` 508, NFS
  side about 3,400.

## What this means for the epics

**Shrink.** E7 (25 md, the largest) starts from 2,283 lines that already do the secure channel,
`SamLogon` pass-through, rotation, DNS discovery and an online join, plus a working Samba AD DC
fixture that already exercises join and pass-through; the work left is the RFC's join semantics
(seal first, resume, offline join, retention, `KeyVersion`/`HandedOver`) and replacing fake-based
tests. E6 (18 md) starts from a 1,300-line LDAP provider with nested groups and SID lookups, and
the same fixture already has RFC2307 and non-RFC2307 users. E5 gets AP-REQ, replay cache, AP-REP and
PAC parsing from develop with tests already built the way plan rule 1 wants (test-realm keys, no KDC).
E4 gets MS-NLMP-vector-tested NTLMv2 crypto. E3 starts from `admit`, which is already v2-shaped.
E2 and E1 get little: the `Minter`, two-secret `User` and RID counter do not exist anywhere yet.

**Traps** (code that looks reusable but contradicts the RFC): `sid/mapper.go`'s uid-derived RIDs;
`pkg/adapter/identity.go`'s default uid 1000; LDAP `rid` idmap mode, first-match-on-ambiguity,
`allow_plaintext` and `insecure_skip_verify`; `join.go`'s `0x1F` enctypes (RC4/DES); the PAC
fail-open in develop's Kerberos service; `session_setup.go`'s guest fallback, five-domain retry,
unenforced MIC and "pass-through only for users not in the store"; `pkg/identity`'s error-TTL cache;
`runtime/identity`'s `*_to_admin` squash modes and the `RootKeepsAdmin` bypass; the always-stored
NT hash on the v1 user model; `pkg/auth/kerberos`'s polled keytab file and `StripRealm`.

**Risks.**
- Most existing unit tests for netlogon and LDAP rest on fakes (`fakeLDAP`, `fakeChannel`,
  `fakeState`, `fakeConn`, `mockProvider`) that CLAUDE.md now bans; the in-memory DC and directory
  plus their conformance suites must be built before those tests can be ported, so test effort in
  E6/E7 is close to new.
- The only real-AD coverage (`ad-dc`) imports packages RFC 23 E1 deletes; if E1 lands before E6/E7
  move the code, the merge-tier cell goes red or must be parked. Sequence the `git mv` (or a temporary
  skip with an issue) with RFC 23 E1.
- Kerberos test infra on storage-v2 is already half dead (dangling `integration/kerberos` option,
  e2e `KDCHelper` pointing at a deleted directory); E9 and the smbtorture Kerberos cell need an SMB
  adapter through the node and a KDC fixture that is restored or rebuilt.
- AES only (A4) means the Samba fixture and any KDC fixture must issue AES-only tickets and keytabs;
  the current fixture exports a keytab with whatever enctypes Samba defaults to (not checked).
- No AD/KDC service in `dt services`; every AD test pays a Samba provision per package run (minutes),
  which matters if E6/E7 add many rows to `ad-dc`.
