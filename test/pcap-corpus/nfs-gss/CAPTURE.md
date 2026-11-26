# RPCSEC_GSS with Kerberos: knfsd reference

`reference.pcap` is the Linux kernel NFS server (knfsd) answering the Linux
kernel client over loopback under `RPCSEC_GSS` with Kerberos, recorded by the
`gss` area of `../nfs-record.sh` against a throwaway MIT KDC. For NFSv3 with
MOUNT, then NFSv4.0, then NFSv4.1, it mounts at `sec=krb5`, `krb5i` and
`krb5p` in turn, and at each a user principal runs `stat`, a small write and a
read of a file, then removes it before the unmount. It holds:

- the `RPCSEC_GSS_INIT` calls (an RPC `NULL` with `gss_proc` 1) and knfsd's
  replies: handle, major and minor status, a window of 128, an empty token
  and a reply verifier that is a MIC token over the window;
- data calls (`gss_proc` 0) under each context at `rpcsec_gss_svc_none`,
  `_integrity` and `_privacy`, with their replies;
- `RPCSEC_GSS_DESTROY` calls (`gss_proc` 3) for NFSv4 contexts at unmount;
- MOUNT's `NULL` and `MNT` under `AUTH_NONE` and `AUTH_SYS`, as the Linux
  client sends them whatever `sec=` says;
- knfsd's own `INIT`s to the NFSv4.0 callback program, which fail (below).

Only the frames `tshark` reads as RPC are kept, so the KDC's AS and TGS
exchanges are not in it. `reference.skel` is its skeleton.

## The realm and its keys

The golden bytes depend on all of these: another realm, principal, key
version or enctype gives other tickets, tokens and checksums.

- Realm `DITTOFS.TEST`, a KDC in the harness's image `dittofs-dt-kdc`
  (`test/smb-conformance/kdc`: Debian bookworm, `krb5-kdc`
  1.20.1-2+deb12u5), `supported_enctypes` `aes256-cts-hmac-sha1-96` and
  `aes128-cts-hmac-sha1-96` only, listening on `127.0.0.1:18888`.
- Service principal `nfs/localhost.localdomain@DITTOFS.TEST`, key version
  2: `rpc.gssd` names the server by a reverse lookup of `127.0.0.1`, which
  this host's `/etc/hosts` answers `localhost.localdomain`.
- The client's machine principal `host/ditto0926@DITTOFS.TEST`, which
  `rpc.gssd` takes from its own keytab for root's calls: the mount's
  `FSINFO`, `PATHCONF` and `GETATTR`, and NFSv4's lease and session calls,
  which the client sends at `krb5i` whatever the mount's level.
- User principal `nobody@DITTOFS.TEST`, password `nfs-record`, the uid
  65534 the file operations run as. It is `nobody` so that knfsd's idmapping
  gives it the host's `nobody` user: a principal that maps to no user is
  given no uid, and knfsd refuses to create files for it.
- Every ticket and session key is `aes256-cts-hmac-sha1-96` (18). The kernel
  offered `rpc.gssd` enctypes 20, 19, 26, 25, 18 and 17; the KDC holds only
  17 and 18.
- The Linux client asks for no mutual authentication (`ap-options` 0, and
  the GSS checksum's flags give integrity and confidentiality only), and
  sends an `aes256-cts-hmac-sha1-96` authenticator subkey. So knfsd sends no
  AP-REP: every `INIT` reply's token is empty, and the context key is the
  authenticator's subkey. The reply verifiers' MIC tokens carry
  `SendByAcceptor` and not `AcceptorSubkey`.

**`nfs.keytab` is committed, and protects nothing but this capture.** It
holds `nfs/localhost.localdomain@DITTOFS.TEST`'s two keys (aes256 and
aes128, key version 2) from a realm made for this recording and gone after
it, so a test can decrypt the ticket in each captured `INIT`, take its
session key and the authenticator's subkey, and check the client's and
knfsd's MIC and Wrap tokens. With it, `tshark -o kerberos.decrypt:TRUE -o
kerberos.file:nfs.keytab` decrypts every ticket and authenticator here. The
machine principal's keytab is not committed: no message here is encrypted
under its key. No other key of any realm is in the corpus.

## Host

- Kernel `7.0.0-44-generic` (Ubuntu 26.04.1 LTS), server and client on the
  same host, both over `127.0.0.1`.
- Host name `ditto0926`, the recording host's: it is in the machine
  principal and the NFSv4 client's owner names, so it shapes the golden
  bytes.
- `nfs-kernel-server` and `nfs-common` from nfs-utils 2.8.5, the host's MIT
  krb5 1.22.1 for `rpc.gssd`, `rpc.svcgssd` and `kinit`, `tshark` 4.6.4.
- Server side: `rpc.svcgssd -p nfs/localhost.localdomain@DITTOFS.TEST`
  with that keytab; client side: `rpc.gssd -k <machine keytab> -d <ccache
  dir>`. Both run in the foreground for the area with `KRB5_CONFIG` naming
  the KDC's `krb5.conf`, so the host's `/etc/krb5.conf` and
  `/etc/krb5.keytab` are not read.
- Exported filesystem: XFS on a 512 MiB loop image, `mkfs.xfs -m reflink=1`,
  exported with
  `exportfs -o rw,sync,no_subtree_check,no_root_squash,fsid=0,sec=krb5:krb5i:krb5p 127.0.0.1:<dir>`.
  `fsid=0` makes it NFSv4's root, mounted as `127.0.0.1:/`: knfsd's
  generated pseudo-root allows only `AUTH_SYS`, so a `sec=krb5` `PUTROOTFH`
  there is `NFS4ERR_WRONGSEC`. NFSv3 mounts it by its path.
- Client mount options: `vers=3,proto=tcp,mountproto=tcp,port=2049`,
  `vers=4.0` and `vers=4.1`, each with `sec=krb5`, `sec=krb5i` and
  `sec=krb5p`.

## What it shows, and what it lacks

- `rpc.gssd` sets a context up in user space over a TCP connection of its
  own, so each `INIT` and its reply sit on a connection that carries nothing
  else; the kernel then sends the context's calls on the mount's connection.
- Every `INIT` names `rpcsec_gss_svc_none`, whatever the mount's level; the
  level is in the data calls' credentials.
- There is no `CONTINUE_INIT`: Kerberos completes in one leg.
- There is no `DESTROY` of an NFSv3 context: the client destroyed only its
  NFSv4 contexts at unmount. The `DESTROY`s of the NFSv4.1 `krb5p` mount
  share one TCP segment with another call, frame 534, so none is cut from it.
- knfsd's `INIT`s to the NFSv4.0 callback program fail: the server's
  `rpc.gssd` holds no key for `nfs/ditto0926` and the client's acceptor none
  for `host/ditto0926`, so the callback channel carries no Kerberos context,
  as RFC 21 §3.4 has it for DittoFS.

## How it was produced

```bash
docker build -t dittofs-dt-kdc test/smb-conformance/kdc
sudo apt-get install nfs-kernel-server
sudo NFS_RECORD_THROWAWAY_HOST=1 test/pcap-corpus/nfs-record.sh <outdir> gss
cp <outdir>/gss/reference.pcap <outdir>/gss/nfs.keytab test/pcap-corpus/nfs-gss/
tshark -r reference.pcap -T pdml | ../normalize.py --proto rpc --proto nfs > reference.skel
```

`nfs-record.sh` lists every command the area runs. Besides the scratch
filesystem and export, it starts the KDC container on the host's network,
adds the machine principal with `kadmin.local`, gets the user's ticket with
`kinit`, restarts `nfs-server` (which forgets earlier areas' NFSv4 clients,
whose `AUTH_SYS` principal would make the client's Kerberos `SETCLIENTID`
`NFS4ERR_CLID_INUSE`), and starts `rpc.svcgssd` and `rpc.gssd`. On exit it
stops both daemons, removes the container and deletes the scratch tree with
its keytabs and credential caches. Installing `nfs-kernel-server`, building
the image and removing both afterwards are the operator's.

On Ubuntu, AppArmor's `tshark` profile refuses to read a capture under a
home directory or one owned by `tcpdump`'s user; a copy of the `tshark`
binary outside `/usr/bin` runs unconfined.

## Golden files

Cut into `internal/adapter/nfs/wire/testdata/golden/` with

```bash
test/pcap-corpus/cut-golden.sh test/pcap-corpus/nfs-gss/reference.pcap <frame> rpc <file>
```

Each file is named `knfsd-<nfs3|nfs4>-<gss3|gss40|gss41>-<sec>-<what>-<call|reply>`:
`<sec>` is the mount's level and `<what>` is `init`, `destroy` or the
operation. For each mount the `init` is the user principal's context and the
`write` a call under it (NFSv3 `WRITE`; NFSv4.0 `PUTFH`, `WRITE`, `GETATTR`;
NFSv4.1 the same after `SEQUENCE`), so a test can take a data call's key from the `init` beside it.
The context handle in each data call's credential is the one its `init`
reply returned.

| File | Frame | Context handle |
| --- | --- | --- |
| `knfsd-nfs3-gss3-krb5-init-call.hex` | 29 | |
| `knfsd-nfs3-gss3-krb5-init-reply.hex` | 30 | `02000000` |
| `knfsd-nfs3-gss3-krb5-write-call.hex` | 37 | `02000000` |
| `knfsd-nfs3-gss3-krb5-write-reply.hex` | 38 | |
| `knfsd-nfs3-gss3-krb5i-init-call.hex` | 87 | |
| `knfsd-nfs3-gss3-krb5i-init-reply.hex` | 88 | `04000000` |
| `knfsd-nfs3-gss3-krb5i-write-call.hex` | 95 | `04000000` |
| `knfsd-nfs3-gss3-krb5i-write-reply.hex` | 96 | |
| `knfsd-nfs3-gss3-krb5p-init-call.hex` | 145 | |
| `knfsd-nfs3-gss3-krb5p-init-reply.hex` | 146 | `06000000` |
| `knfsd-nfs3-gss3-krb5p-write-call.hex` | 153 | `06000000` |
| `knfsd-nfs3-gss3-krb5p-write-reply.hex` | 154 | |
| `knfsd-nfs4-gss40-krb5-init-call.hex` | 214 | |
| `knfsd-nfs4-gss40-krb5-init-reply.hex` | 215 | `0a000000` |
| `knfsd-nfs4-gss40-krb5-write-call.hex` | 222 | `0a000000` |
| `knfsd-nfs4-gss40-krb5-write-reply.hex` | 223 | |
| `knfsd-nfs4-gss40-krb5i-init-call.hex` | 279 | |
| `knfsd-nfs4-gss40-krb5i-init-reply.hex` | 280 | `0d000000` |
| `knfsd-nfs4-gss40-krb5i-write-call.hex` | 287 | `0d000000` |
| `knfsd-nfs4-gss40-krb5i-write-reply.hex` | 288 | |
| `knfsd-nfs4-gss40-krb5p-init-call.hex` | 343 | |
| `knfsd-nfs4-gss40-krb5p-init-reply.hex` | 344 | `11000000` |
| `knfsd-nfs4-gss40-krb5p-write-call.hex` | 351 | `11000000` |
| `knfsd-nfs4-gss40-krb5p-write-reply.hex` | 352 | |
| `knfsd-nfs4-gss40-krb5p-destroy-call.hex` | 370 | `11000000` |
| `knfsd-nfs4-gss40-krb5p-destroy-reply.hex` | 372 | |
| `knfsd-nfs4-gss41-krb5-init-call.hex` | 399 | |
| `knfsd-nfs4-gss41-krb5-init-reply.hex` | 400 | `14000000` |
| `knfsd-nfs4-gss41-krb5-write-call.hex` | 405 | `14000000` |
| `knfsd-nfs4-gss41-krb5-write-reply.hex` | 406 | |
| `knfsd-nfs4-gss41-krb5-destroy-call.hex` | 424 | `14000000` |
| `knfsd-nfs4-gss41-krb5-destroy-reply.hex` | 427 | |
| `knfsd-nfs4-gss41-krb5i-init-call.hex` | 456 | |
| `knfsd-nfs4-gss41-krb5i-init-reply.hex` | 457 | `17000000` |
| `knfsd-nfs4-gss41-krb5i-write-call.hex` | 462 | `17000000` |
| `knfsd-nfs4-gss41-krb5i-write-reply.hex` | 463 | |
| `knfsd-nfs4-gss41-krb5p-init-call.hex` | 510 | |
| `knfsd-nfs4-gss41-krb5p-init-reply.hex` | 511 | `1a000000` |
| `knfsd-nfs4-gss41-krb5p-write-call.hex` | 516 | `1a000000` |
| `knfsd-nfs4-gss41-krb5p-write-reply.hex` | 517 | |

`TestParseCaptured/knfsd` in `wire_test.go` checks each call's credential
against its name: `RPCSEC_GSS`, `gss_proc` `INIT`, `DESTROY` or `DATA`, the
service of its mount's level (`none` for every `INIT`), and a context handle
on every call but an `INIT`; and each reply's `RPCSEC_GSS` verifier.
`ParseCall` reads a COMPOUND's tag and minor version from a `krb5i` or
`krb5p` call's protected body, and refuses the two `krb5p` NFSv4 `write`
calls there, so the test reads those calls' credentials by hand.
