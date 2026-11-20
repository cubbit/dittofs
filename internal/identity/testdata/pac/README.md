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

What the capture recorded, which `pac_test.go`'s `captures` table checks:

- realm `DITTOFS.AD`, client `alice`, authentication time 2026-11-20T07:40:42Z
  (both tickets);
- alice's SID `S-1-5-21-978171696-1174491544-1106729380-1105`;
- both server signatures of type 16 (`HMAC-SHA1-96-AES256`).

Every capture provisions a new domain, so its keys, SIDs and times all change:
a recapture updates this file and the `captures` table with it.
