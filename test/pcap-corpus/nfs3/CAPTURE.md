# NFSv3 and MOUNT v3: knfsd reference

`reference.pcap` is the Linux kernel NFS server (knfsd) answering the Linux
kernel client over loopback, recorded by `../nfs-record.sh`. It holds MOUNT's `NULL` (`rpcinfo`), `EXPORT` (`showmount -e`), `MNT` and `UMNT` (the mount and unmount), and NFSv3's `NULL`, `FSINFO`, `PATHCONF`, `GETATTR`, `ACCESS`, `CREATE`, `WRITE`, `LOOKUP`, `READ`, `READDIRPLUS` and `REMOVE` from a create, write, read, list and remove over the mount.
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
- Client mount options: `vers=3,proto=tcp,mountproto=tcp,port=2049`; `sec=sys`, so every credential is `AUTH_SYS`
  or `AUTH_NONE`.

## How it was produced

```bash
sudo NFS_RECORD_THROWAWAY_HOST=1 test/pcap-corpus/nfs-record.sh <outdir> nfs3
cp <outdir>/nfs3/reference.pcap test/pcap-corpus/nfs3/
tshark -r reference.pcap -T pdml | ../normalize.py --proto nfs --proto mount > reference.skel
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
test/pcap-corpus/cut-golden.sh test/pcap-corpus/nfs3/reference.pcap <frame> <framing> <file>
```

which runs `tshark -r <capture> -Y frame.number==<frame> -T fields -e tcp.reassembled.data -e tcp.payload -e udp.payload`
and strips the record mark (`rpc`) or nothing (`none`, a UDP datagram). Each
file's header repeats its capture, frame and command.

| File | Frame | Framing |
| --- | --- | --- |
| `knfsd-nfs3-nfs3-mount-null-call.hex` | 3 | `rpc` |
| `knfsd-nfs3-nfs3-mount-null-reply.hex` | 4 | `rpc` |
| `knfsd-nfs3-nfs3-mount-export-call.hex` | 7 | `rpc` |
| `knfsd-nfs3-nfs3-mount-export-reply.hex` | 8 | `rpc` |
| `knfsd-nfs3-nfs3-mount-mnt-call.hex` | 19 | `rpc` |
| `knfsd-nfs3-nfs3-mount-mnt-reply.hex` | 20 | `rpc` |
| `knfsd-nfs3-nfs3-v3-null-call.hex` | 21 | `rpc` |
| `knfsd-nfs3-nfs3-v3-null-reply.hex` | 22 | `rpc` |
| `knfsd-nfs3-nfs3-fsinfo-call.hex` | 23 | `rpc` |
| `knfsd-nfs3-nfs3-fsinfo-reply.hex` | 24 | `rpc` |
| `knfsd-nfs3-nfs3-pathconf-call.hex` | 25 | `rpc` |
| `knfsd-nfs3-nfs3-pathconf-reply.hex` | 26 | `rpc` |
| `knfsd-nfs3-nfs3-getattr-call.hex` | 27 | `rpc` |
| `knfsd-nfs3-nfs3-getattr-reply.hex` | 28 | `rpc` |
| `knfsd-nfs3-nfs3-access-call.hex` | 35 | `rpc` |
| `knfsd-nfs3-nfs3-access-reply.hex` | 36 | `rpc` |
| `knfsd-nfs3-nfs3-create-call.hex` | 39 | `rpc` |
| `knfsd-nfs3-nfs3-create-reply.hex` | 40 | `rpc` |
| `knfsd-nfs3-nfs3-write-call.hex` | 41 | `rpc` |
| `knfsd-nfs3-nfs3-write-reply.hex` | 42 | `rpc` |
| `knfsd-nfs3-nfs3-lookup-call.hex` | 45 | `rpc` |
| `knfsd-nfs3-nfs3-lookup-reply.hex` | 46 | `rpc` |
| `knfsd-nfs3-nfs3-read-call.hex` | 51 | `rpc` |
| `knfsd-nfs3-nfs3-read-reply.hex` | 52 | `rpc` |
| `knfsd-nfs3-nfs3-readdirplus-call.hex` | 55 | `rpc` |
| `knfsd-nfs3-nfs3-readdirplus-reply.hex` | 56 | `rpc` |
| `knfsd-nfs3-nfs3-remove-call.hex` | 59 | `rpc` |
| `knfsd-nfs3-nfs3-remove-reply.hex` | 60 | `rpc` |
| `knfsd-nfs3-nfs3-mount-umnt-call.hex` | 65 | `rpc` |
| `knfsd-nfs3-nfs3-mount-umnt-reply.hex` | 66 | `rpc` |
