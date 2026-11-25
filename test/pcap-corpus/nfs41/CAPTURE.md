# NFSv4.1 and its callback program: knfsd reference

`reference.pcap` is the Linux kernel NFS server (knfsd) answering the Linux
kernel client over loopback, recorded by `../nfs-record.sh`. It holds `EXCHANGE_ID`, `CREATE_SESSION`, `RECLAIM_COMPLETE`, `SECINFO_NO_NAME`, the mount's walk, `OPEN`, `WRITE`, `CLOSE`, `READ`, `REMOVE`, `DESTROY_SESSION`, `DESTROY_CLIENTID`, and a read delegation recalled by a write on the server's side: `CB_COMPOUND` with `CB_SEQUENCE` and `CB_RECALL` on the session's back channel, then `DELEGRETURN`.
Only the frames `tshark` reads as RPC are kept. `reference.skel` is its
skeleton.

**Gap:** knfsd granted the first `OPEN` a delegation, so the client settled
the recipe's lock under it and sent no `LOCK` or `LOCKU`; `nfs40/` holds both,
and an NFSv4.1 lock's only difference on the wire is the `SEQUENCE` before it.

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
- Client mount options: `vers=4.1`; `sec=sys`, so every credential is `AUTH_SYS`
  or `AUTH_NONE`.

## How it was produced

```bash
sudo NFS_RECORD_THROWAWAY_HOST=1 test/pcap-corpus/nfs-record.sh <outdir> nfs41
cp <outdir>/nfs41/reference.pcap test/pcap-corpus/nfs41/
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
test/pcap-corpus/cut-golden.sh test/pcap-corpus/nfs41/reference.pcap <frame> <framing> <file>
```

which runs `tshark -r <capture> -Y frame.number==<frame> -T fields -e tcp.reassembled.data -e tcp.payload -e udp.payload`
and strips the record mark (`rpc`) or nothing (`none`, a UDP datagram). Each
file's header repeats its capture, frame and command.

| File | Frame | Framing |
| --- | --- | --- |
| `knfsd-nfs4-nfs41-null-call.hex` | 1 | `rpc` |
| `knfsd-nfs4-nfs41-null-reply.hex` | 2 | `rpc` |
| `knfsd-nfs4-nfs41-exchangeid-call.hex` | 3 | `rpc` |
| `knfsd-nfs4-nfs41-exchangeid-reply.hex` | 4 | `rpc` |
| `knfsd-nfs4-nfs41-createsession-call.hex` | 7 | `rpc` |
| `knfsd-nfs4-nfs41-createsession-reply.hex` | 8 | `rpc` |
| `knfsd-nfs4-nfs41-reclaimcomplete-call.hex` | 9 | `rpc` |
| `knfsd-nfs4-nfs41-reclaimcomplete-reply.hex` | 10 | `rpc` |
| `knfsd-nfs4-nfs41-secinfononame-call.hex` | 11 | `rpc` |
| `knfsd-nfs4-nfs41-secinfononame-reply.hex` | 12 | `rpc` |
| `knfsd-nfs4-nfs41-putrootfh-getattr-call.hex` | 13 | `rpc` |
| `knfsd-nfs4-nfs41-putrootfh-getattr-reply.hex` | 14 | `rpc` |
| `knfsd-nfs4-nfs41-getattr-call.hex` | 15 | `rpc` |
| `knfsd-nfs4-nfs41-getattr-reply.hex` | 16 | `rpc` |
| `knfsd-nfs4-nfs41-access-call.hex` | 25 | `rpc` |
| `knfsd-nfs4-nfs41-access-reply.hex` | 26 | `rpc` |
| `knfsd-nfs4-nfs41-lookup-call.hex` | 27 | `rpc` |
| `knfsd-nfs4-nfs41-lookup-reply.hex` | 28 | `rpc` |
| `knfsd-nfs4-nfs41-open-call.hex` | 75 | `rpc` |
| `knfsd-nfs4-nfs41-open-reply.hex` | 76 | `rpc` |
| `knfsd-nfs4-nfs41-write-call.hex` | 77 | `rpc` |
| `knfsd-nfs4-nfs41-write-reply.hex` | 78 | `rpc` |
| `knfsd-nfs4-nfs41-close-call.hex` | 79 | `rpc` |
| `knfsd-nfs4-nfs41-close-reply.hex` | 80 | `rpc` |
| `knfsd-nfs4-nfs41-delegreturn-call.hex` | 81 | `rpc` |
| `knfsd-nfs4-nfs41-delegreturn-reply.hex` | 82 | `rpc` |
| `knfsd-nfs4-nfs41-destroysession-call.hex` | 83 | `rpc` |
| `knfsd-nfs4-nfs41-destroysession-reply.hex` | 84 | `rpc` |
| `knfsd-nfs4-nfs41-destroyclientid-call.hex` | 85 | `rpc` |
| `knfsd-nfs4-nfs41-destroyclientid-reply.hex` | 86 | `rpc` |
| `knfsd-nfs4-nfs41-read-call.hex` | 163 | `rpc` |
| `knfsd-nfs4-nfs41-read-reply.hex` | 164 | `rpc` |
| `knfsd-nfs4-nfs41-cb-cbsequence-cbrecall-call.hex` | 165 | `rpc` |
| `knfsd-nfs4-nfs41-cb-cbsequence-cbrecall-reply.hex` | 166 | `rpc` |
| `knfsd-nfs4-nfs41-remove-call.hex` | 173 | `rpc` |
| `knfsd-nfs4-nfs41-remove-reply.hex` | 174 | `rpc` |
