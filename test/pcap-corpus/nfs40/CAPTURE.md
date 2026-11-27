# NFSv4.0 and its callback program: knfsd reference

`reference.pcap` is the Linux kernel NFS server (knfsd) answering the Linux
kernel client over loopback, recorded by `../nfs-record.sh`. It holds `SETCLIENTID` and `SETCLIENTID_CONFIRM`, the callback program's `CB_NULL` (program 0x40000000, the number the Linux client names), the mount's `PUTROOTFH`/`LOOKUP` walk, `OPEN`, `OPEN_CONFIRM`, `WRITE`, `CLOSE`, `LOCK`, `LOCKU`, `RELEASE_LOCKOWNER`, `READ` and `REMOVE`.
Only the frames `tshark` reads as RPC are kept. `reference.skel` is its
skeleton.

**Gap:** knfsd answered every `OPEN` of this recipe `OPEN_DELEGATE_NONE`, so there is no `CB_RECALL` of minor version 0; the callback program is reached through `CB_NULL` only. `nfs41/` holds a recall, and a 4.0 `CB_RECALL` vector, if one is needed, is written from RFC 7530 by hand.

## Host

- Kernel `7.0.0-44-generic` (Ubuntu 26.04.1 LTS), server and client on the same
  host, both over `127.0.0.1`.
- Host name `ditto0926`, the recording host's: it is the `AUTH_SYS` machine
  name in every credential and part of the client's owner and caller names,
  so it shapes the golden bytes; a recording on another host differs there.
- `nfs-kernel-server` and `nfs-common` from nfs-utils 2.8.5; `tshark` 4.6.4.
- Exported filesystem: XFS on a 512 MiB loop image, `mkfs.xfs -m reflink=1`
  (xfsprogs 6.18.0), exported with
  `exportfs -o rw,sync,no_subtree_check,no_root_squash,fsid=4242 127.0.0.1:<dir>`.
- Client mount options: `vers=4.0`; `sec=sys`, so every credential is `AUTH_SYS`
  or `AUTH_NONE`.

## How it was produced

```bash
sudo NFS_RECORD_THROWAWAY_HOST=1 test/pcap-corpus/nfs-record.sh <outdir> nfs40
cp <outdir>/nfs40/reference.pcap test/pcap-corpus/nfs40/
tshark -r reference.pcap -T pdml | ../normalize.py --proto nfs > reference.skel
```

`nfs-record.sh` lists every command the area runs. It makes the scratch
filesystem and export, records under `tcpdump -i lo`, filters the frames with
`tshark -Y rpc`, and removes the export, the mounts and the loop image.
The area was recorded on its own, before the script gained its
`NFS_RECORD_THROWAWAY_HOST` opt-in, its default order with `nlm` last and its
cleanup of only what it holds; the script has not been re-run since. None of
those changes alters what a single area sends.

## Golden files

Cut into `internal/adapter/nfs/wire/testdata/golden/` with

```bash
test/pcap-corpus/cut-golden.sh test/pcap-corpus/nfs40/reference.pcap <frame> <framing> <file>
```

which runs `tshark -r <capture> -Y frame.number==<frame> -T fields -e tcp.reassembled.data -e tcp.payload -e udp.payload`
and strips the record mark (`rpc`) or nothing (`none`, a UDP datagram). Each
file's header repeats its capture, frame and command.

| File | Frame | Framing |
| --- | --- | --- |
| `knfsd-nfs4-nfs40-null-call.hex` | 1 | `rpc` |
| `knfsd-nfs4-nfs40-null-reply.hex` | 2 | `rpc` |
| `knfsd-nfs4-nfs40-setclientid-call.hex` | 3 | `rpc` |
| `knfsd-nfs4-nfs40-setclientid-reply.hex` | 4 | `rpc` |
| `knfsd-nfs4-nfs40-setclientidconfirm-call.hex` | 5 | `rpc` |
| `knfsd-nfs4-nfs40-setclientidconfirm-reply.hex` | 6 | `rpc` |
| `knfsd-nfs4-nfs40-cb-cbnull-call.hex` | 7 | `rpc` |
| `knfsd-nfs4-nfs40-cb-cbnull-reply.hex` | 9 | `rpc` |
| `knfsd-nfs4-nfs40-putrootfh-getattr-call.hex` | 13 | `rpc` |
| `knfsd-nfs4-nfs40-putrootfh-getattr-reply.hex` | 16 | `rpc` |
| `knfsd-nfs4-nfs40-getattr-call.hex` | 19 | `rpc` |
| `knfsd-nfs4-nfs40-getattr-reply.hex` | 20 | `rpc` |
| `knfsd-nfs4-nfs40-access-call.hex` | 29 | `rpc` |
| `knfsd-nfs4-nfs40-access-reply.hex` | 30 | `rpc` |
| `knfsd-nfs4-nfs40-lookup-call.hex` | 31 | `rpc` |
| `knfsd-nfs4-nfs40-lookup-reply.hex` | 32 | `rpc` |
| `knfsd-nfs4-nfs40-open-call.hex` | 79 | `rpc` |
| `knfsd-nfs4-nfs40-open-reply.hex` | 80 | `rpc` |
| `knfsd-nfs4-nfs40-openconfirm-call.hex` | 81 | `rpc` |
| `knfsd-nfs4-nfs40-openconfirm-reply.hex` | 82 | `rpc` |
| `knfsd-nfs4-nfs40-write-call.hex` | 83 | `rpc` |
| `knfsd-nfs4-nfs40-write-reply.hex` | 84 | `rpc` |
| `knfsd-nfs4-nfs40-close-call.hex` | 85 | `rpc` |
| `knfsd-nfs4-nfs40-close-reply.hex` | 86 | `rpc` |
| `knfsd-nfs4-nfs40-lock-call.hex` | 89 | `rpc` |
| `knfsd-nfs4-nfs40-lock-reply.hex` | 90 | `rpc` |
| `knfsd-nfs4-nfs40-locku-call.hex` | 91 | `rpc` |
| `knfsd-nfs4-nfs40-locku-reply.hex` | 92 | `rpc` |
| `knfsd-nfs4-nfs40-releaselockowner-call.hex` | 93 | `rpc` |
| `knfsd-nfs4-nfs40-releaselockowner-reply.hex` | 94 | `rpc` |
| `knfsd-nfs4-nfs40-read-call.hex` | 179 | `rpc` |
| `knfsd-nfs4-nfs40-read-reply.hex` | 180 | `rpc` |
| `knfsd-nfs4-nfs40-remove-call.hex` | 185 | `rpc` |
| `knfsd-nfs4-nfs40-remove-reply.hex` | 186 | `rpc` |
