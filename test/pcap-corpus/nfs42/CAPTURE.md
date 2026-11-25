# NFSv4.2: knfsd reference

`reference.pcap` is the Linux kernel NFS server (knfsd) answering the Linux
kernel client over loopback, recorded by `../nfs-record.sh`. It holds over an export of XFS made with `reflink=1`: `ALLOCATE`, `DEALLOCATE`, `SEEK` (data and hole, one answered `NFS4ERR_NXIO`), `COPY` with `COMMIT`, `CLONE`, `SETXATTR`, `GETXATTR`, `LISTXATTRS` and `REMOVEXATTR`, with the session's setup and teardown around them; knfsd also sent a `CB_RECALL_ANY` on the back channel.
Only the frames `tshark` reads as RPC are kept. `reference.skel` is its
skeleton.

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
- Client mount options: `vers=4.2`; `sec=sys`, so every credential is `AUTH_SYS`
  or `AUTH_NONE`.

## How it was produced

```bash
sudo NFS_RECORD_THROWAWAY_HOST=1 test/pcap-corpus/nfs-record.sh <outdir> nfs42
cp <outdir>/nfs42/reference.pcap test/pcap-corpus/nfs42/
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
test/pcap-corpus/cut-golden.sh test/pcap-corpus/nfs42/reference.pcap <frame> <framing> <file>
```

which runs `tshark -r <capture> -Y frame.number==<frame> -T fields -e tcp.reassembled.data -e tcp.payload -e udp.payload`
and strips the record mark (`rpc`) or nothing (`none`, a UDP datagram). Each
file's header repeats its capture, frame and command.

| File | Frame | Framing |
| --- | --- | --- |
| `knfsd-nfs4-nfs42-null-call.hex` | 1 | `rpc` |
| `knfsd-nfs4-nfs42-null-reply.hex` | 2 | `rpc` |
| `knfsd-nfs4-nfs42-exchangeid-call.hex` | 3 | `rpc` |
| `knfsd-nfs4-nfs42-exchangeid-reply.hex` | 4 | `rpc` |
| `knfsd-nfs4-nfs42-createsession-call.hex` | 7 | `rpc` |
| `knfsd-nfs4-nfs42-createsession-reply.hex` | 8 | `rpc` |
| `knfsd-nfs4-nfs42-reclaimcomplete-call.hex` | 9 | `rpc` |
| `knfsd-nfs4-nfs42-reclaimcomplete-reply.hex` | 10 | `rpc` |
| `knfsd-nfs4-nfs42-secinfononame-call.hex` | 11 | `rpc` |
| `knfsd-nfs4-nfs42-secinfononame-reply.hex` | 12 | `rpc` |
| `knfsd-nfs4-nfs42-putrootfh-getattr-call.hex` | 13 | `rpc` |
| `knfsd-nfs4-nfs42-putrootfh-getattr-reply.hex` | 14 | `rpc` |
| `knfsd-nfs4-nfs42-getattr-call.hex` | 15 | `rpc` |
| `knfsd-nfs4-nfs42-getattr-reply.hex` | 16 | `rpc` |
| `knfsd-nfs4-nfs42-access-call.hex` | 25 | `rpc` |
| `knfsd-nfs4-nfs42-access-reply.hex` | 26 | `rpc` |
| `knfsd-nfs4-nfs42-lookup-call.hex` | 27 | `rpc` |
| `knfsd-nfs4-nfs42-lookup-reply.hex` | 28 | `rpc` |
| `knfsd-nfs4-nfs42-open-call.hex` | 77 | `rpc` |
| `knfsd-nfs4-nfs42-open-reply.hex` | 78 | `rpc` |
| `knfsd-nfs4-nfs42-allocate-call.hex` | 91 | `rpc` |
| `knfsd-nfs4-nfs42-allocate-reply.hex` | 92 | `rpc` |
| `knfsd-nfs4-nfs42-deallocate-call.hex` | 93 | `rpc` |
| `knfsd-nfs4-nfs42-deallocate-reply.hex` | 94 | `rpc` |
| `knfsd-nfs4-nfs42-seek-call.hex` | 95 | `rpc` |
| `knfsd-nfs4-nfs42-seek-reply.hex` | 96 | `rpc` |
| `knfsd-nfs4-nfs42-copy-commit-call.hex` | 111 | `rpc` |
| `knfsd-nfs4-nfs42-copy-commit-reply.hex` | 112 | `rpc` |
| `knfsd-nfs4-nfs42-setattr-call.hex` | 117 | `rpc` |
| `knfsd-nfs4-nfs42-setattr-reply.hex` | 118 | `rpc` |
| `knfsd-nfs4-nfs42-clone-call.hex` | 119 | `rpc` |
| `knfsd-nfs4-nfs42-clone-reply.hex` | 120 | `rpc` |
| `knfsd-nfs4-nfs42-setxattr-call.hex` | 121 | `rpc` |
| `knfsd-nfs4-nfs42-setxattr-reply.hex` | 122 | `rpc` |
| `knfsd-nfs4-nfs42-cb-cbsequence-cbrecallany-call.hex` | 123 | `rpc` |
| `knfsd-nfs4-nfs42-cb-cbsequence-cbrecallany-reply.hex` | 124 | `rpc` |
| `knfsd-nfs4-nfs42-setattr-delegreturn-call.hex` | 125 | `rpc` |
| `knfsd-nfs4-nfs42-setattr-delegreturn-reply.hex` | 126 | `rpc` |
| `knfsd-nfs4-nfs42-getxattr-call.hex` | 137 | `rpc` |
| `knfsd-nfs4-nfs42-getxattr-reply.hex` | 138 | `rpc` |
| `knfsd-nfs4-nfs42-listxattrs-call.hex` | 139 | `rpc` |
| `knfsd-nfs4-nfs42-listxattrs-reply.hex` | 140 | `rpc` |
| `knfsd-nfs4-nfs42-removexattr-call.hex` | 143 | `rpc` |
| `knfsd-nfs4-nfs42-removexattr-reply.hex` | 144 | `rpc` |
| `knfsd-nfs4-nfs42-remove-call.hex` | 147 | `rpc` |
| `knfsd-nfs4-nfs42-remove-reply.hex` | 148 | `rpc` |
| `knfsd-nfs4-nfs42-destroysession-call.hex` | 157 | `rpc` |
| `knfsd-nfs4-nfs42-destroysession-reply.hex` | 158 | `rpc` |
| `knfsd-nfs4-nfs42-destroyclientid-call.hex` | 159 | `rpc` |
| `knfsd-nfs4-nfs42-destroyclientid-reply.hex` | 160 | `rpc` |
