# Captured PACs

PACs the Samba fixture's KDC issued, captured by
`test/integration/ad-dc/capture-pacs.sh` from a throwaway test domain, with the
keys that verify them. Nothing here is a secret of any real domain.

| File | What it is |
| --- | --- |
| `tgt.pac` | the PAC of alice's TGT, holding `PAC_ATTRIBUTES_INFO` and `PAC_REQUESTOR` |
| `krbtgt.keytab` | `krbtgt/DITTOFS.AD@DITTOFS.AD`, kvno 1, AES256 |
| `service.pac` | the PAC of alice's `cifs/dittofs.dittofs.ad` service ticket |
| `service.keytab` | `cifs/` and `nfs/dittofs.dittofs.ad@DITTOFS.AD`, kvno 2, AES256 and AES128 |
| `alice.apreq` | alice's AP-REQ for `cifs/dittofs.dittofs.ad`, built by gokrb5's client from the service ticket the KDC issued her |
| `alice.json` | the time `alice.apreq`'s authenticator carries, and the SIDs of alice and of `Domain Users`, `devs` and `engineering` as `samba-tool` read them |

What the capture recorded, which `pac_test.go`'s `captures` table checks:

- realm `DITTOFS.AD`, client `alice`, authentication time 2026-11-21T08:55:34Z
  (both tickets);
- alice's SID `S-1-5-21-3296391427-225151214-3620391263-1105`;
- both server signatures of type 16 (`HMAC-SHA1-96-AES256`).

Every capture provisions a new domain, so its keys, SIDs and times all change:
a recapture updates this file and the `captures` table with it.
`kerberos_samba_test.go` reads `alice.json`, so it follows a recapture unchanged.

## The MIT KDC's ticket

A ticket an MIT KDC issued, captured by `test/integration/kerberos`'s
`TestMITPACCapture` from a throwaway `DITTOFS.TEST` realm in Debian bookworm's
`krb5-kdc` (krb5 1.20), which puts a PAC in every ticket, holding no logon
information.

| File | What it is |
| --- | --- |
| `mit-service.pac` | the PAC of alice's `cifs/nas.dittofs.test` service ticket: client information, ticket, server and KDC signatures, and no logon information |
| `mit-service.keytab` | `cifs/nas.dittofs.test@DITTOFS.TEST`, kvno 2, AES256 and AES128 |
| `mit-alice.apreq` | alice's AP-REQ for `cifs/nas.dittofs.test`, built by gokrb5's client from the service ticket the KDC issued her |
| `mit-alice.json` | the time `mit-alice.apreq`'s authenticator carries |

To recapture, with Docker, from the repository root:

    DITTOFS_KERBEROS_PAC_OUT=$PWD/internal/identity/testdata/pac \
      go test -tags=kerberos -count=1 -v -run '^TestMITPACCapture$' ./test/integration/kerberos/

Every run creates a new realm, so the keys and times change.
`kerberos_samba_test.go` reads `mit-alice.json`, so it follows a recapture
unchanged.
