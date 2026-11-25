# NLM v4 and NSM v1: knfsd reference

`reference.pcap` is the Linux kernel NFS server (knfsd) answering the Linux
kernel client over loopback, recorded by `../nfs-record.sh`. It holds over an NFSv3 mount, after a restart of `nfs-server` (a fresh lockd, so statd is asked to monitor again): NSM's `UNMON` and `MON`; NLM's `TEST` (answered `NLM4_DENIED_GRACE_PERIOD`, the restart's grace), a `LOCK` answered `NLM4_BLOCKED` because a process on the server holds the file, the `GRANTED_MSG` callback once it lets go and the client's `GRANTED_RES`, and `UNLOCK`; NSM's `NOTIFY` from `sm-notify -f`, which is what a server restart runs. NSM's `NULL` and `NOTIFY` go over UDP and are cut whole.
Only the frames `tshark` reads as RPC are kept. `reference.skel` is its
skeleton.

The client settles a lock conflict between its own processes before asking the server, so the blocked lock's holder is a process on the server's side of the export.

## Host

- Kernel `7.0.0-44-generic` (Ubuntu 26.04.1 LTS), server and client on the same
  host, both over `127.0.0.1`.
- `nfs-kernel-server` and `nfs-common` from nfs-utils 2.8.5; `tshark` 4.6.4.
- Exported filesystem: XFS on a 512 MiB loop image, `mkfs.xfs -m reflink=1`
  (xfsprogs 6.18.0), exported with
  `exportfs -o rw,sync,no_subtree_check,no_root_squash,fsid=4242 127.0.0.1:<dir>`.
- Client mount options: `vers=3,proto=tcp,mountproto=tcp,port=2049`; `sec=sys`, so every credential is `AUTH_SYS`
  or `AUTH_NONE`.

## How it was produced

```bash
sudo test/pcap-corpus/nfs-record.sh <outdir> nlm
cp <outdir>/nlm/reference.pcap test/pcap-corpus/nlm/
tshark -r reference.pcap -T pdml | ../normalize.py --proto nlm --proto stat > reference.skel
```

`nfs-record.sh` lists every command the area runs. It makes the scratch
filesystem and export, records under `tcpdump -i lo`, filters the frames with
`tshark -Y rpc`, and removes the export, the mounts and the loop image.

## Golden files

Cut into `internal/adapter/nfs/wire/testdata/golden/` with

```bash
test/pcap-corpus/cut-golden.sh test/pcap-corpus/nlm/reference.pcap <frame> <framing> <file>
```

which runs `tshark -r <capture> -Y frame.number==<frame> -T fields -e tcp.reassembled.data -e tcp.payload -e udp.payload`
and strips the record mark (`rpc`) or nothing (`none`, a UDP datagram). Each
file's header repeats its capture, frame and command.

| File | Frame | Framing |
| --- | --- | --- |
| `knfsd-nfs3-nlm-nsm-unmon-call.hex` | 3 | `rpc` |
| `knfsd-nfs3-nlm-nsm-unmon-reply.hex` | 4 | `rpc` |
| `knfsd-nfs3-nlm-nsm-null-call.hex` | 7 | `none` |
| `knfsd-nfs3-nlm-nsm-null-reply.hex` | 8 | `none` |
| `knfsd-nfs3-nlm-test-call.hex` | 45 | `rpc` |
| `knfsd-nfs3-nlm-test-reply.hex` | 46 | `rpc` |
| `knfsd-nfs3-nlm-nsm-mon-call.hex` | 75 | `rpc` |
| `knfsd-nfs3-nlm-nsm-mon-reply.hex` | 76 | `rpc` |
| `knfsd-nfs3-nlm-lock-call.hex` | 77 | `rpc` |
| `knfsd-nfs3-nlm-lock-reply.hex` | 78 | `rpc` |
| `knfsd-nfs3-nlm-grantedmsg-call.hex` | 81 | `rpc` |
| `knfsd-nfs3-nlm-grantedmsg-reply.hex` | 82 | `rpc` |
| `knfsd-nfs3-nlm-grantedres-call.hex` | 83 | `rpc` |
| `knfsd-nfs3-nlm-grantedres-reply.hex` | 84 | `rpc` |
| `knfsd-nfs3-nlm-unlock-call.hex` | 85 | `rpc` |
| `knfsd-nfs3-nlm-unlock-reply.hex` | 86 | `rpc` |
| `knfsd-nfs3-nlm-nsm-notify-call.hex` | 89 | `none` |
| `knfsd-nfs3-nlm-nsm-notify-reply.hex` | 90 | `none` |
