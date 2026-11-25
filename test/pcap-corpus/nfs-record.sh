#!/usr/bin/env bash
#
# nfs-record.sh — record knfsd's reference captures for the NFS areas.
#
# Exports a scratch XFS filesystem (made with reflink=1, so knfsd can CLONE)
# from the kernel NFS server to 127.0.0.1, mounts it with the kernel client
# over loopback, and records each area under tcpdump on lo:
#
#   nfs3   MOUNT NULL, EXPORT, MNT, UMNT, and NFSv3 create/write/read/readdir
#   nfs40  NFSv4.0: SETCLIENTID, open/write/read, a lock, and a read open
#          written on the server's side to recall a delegation (knfsd may
#          grant none, and then no CB_RECALL is recorded)
#   nfs41  the same over NFSv4.1, a recall on the session's back channel
#   nfs42  NFSv4.2: SEEK, ALLOCATE, DEALLOCATE, COPY, CLONE and the xattr
#          operations
#   nlm    NLM lock, test, unlock and a blocked lock granted by callback over
#          an NFSv3 mount; NSM's monitor calls and the SM_NOTIFY that
#          sm-notify -f sends, as a server restart does. It runs last by
#          default: its restart opens a grace period, in which knfsd refuses
#          a non-reclaim NFSv4 OPEN with NFS4ERR_GRACE
#
# Each area's frames that tshark reads as RPC are kept in
# <outdir>/<area>/reference.pcap; everything else on lo is dropped.
#
# Run it on a throwaway host that serves nothing else. The nlm area runs
# `systemctl restart nfs-server`, which drops every export added by hand with
# exportfs and puts every client of the host into a grace period, and
# `sm-notify -f`, which tells every peer in /var/lib/nfs/sm that this host
# rebooted, so those peers drop the NLM locks they hold on it. The script
# refuses to start unless NFS_RECORD_THROWAWAY_HOST=1 says the host is one.
#
# Needs root, the kernel NFS server (nfs-kernel-server), tcpdump, tshark,
# xfsprogs, attr and python3. Run by hand; no suite runs it. On exit it
# releases what it started itself: its background processes, its mount, its
# export and its loop-mounted image.
#
# Usage:
#   sudo NFS_RECORD_THROWAWAY_HOST=1 nfs-record.sh <outdir> [area...]

set -euo pipefail

[[ $# -ge 1 ]] || { grep '^#' "$0" | sed -n '3,$s/^# \{0,1\}//p'; exit 2; }
[[ $EUID -eq 0 ]] || { echo "needs root" >&2; exit 2; }
[[ ${NFS_RECORD_THROWAWAY_HOST:-} == 1 ]] || {
  echo "refusing: restarts nfs-server and runs sm-notify -f; set NFS_RECORD_THROWAWAY_HOST=1 on a throwaway host" >&2
  exit 2
}
out="$(realpath "$1")"; shift
areas=("$@")
[[ ${#areas[@]} -gt 0 ]] || areas=(nfs3 nfs40 nfs41 nfs42 nlm)
for t in exportfs tcpdump tshark mkfs.xfs xfs_io setfattr getfattr python3 sm-notify rpcinfo showmount; do
  command -v "$t" >/dev/null || { echo "missing: $t" >&2; exit 2; }
done

scratch="$(mktemp -d /tmp/nfs-record.XXXXXX)"
exp="$scratch/export" mnt="$scratch/mnt" dump_pid="" holder="" exported=""

# cleanup releases only what this script holds: fd 3, which would keep $mnt
# busy, its own tcpdump and lock holder, then the mount, export and image it
# made under $scratch. The tree is removed only once nothing is mounted in it.
cleanup() {
  set +e
  exec 3<&-
  for p in "$dump_pid" "$holder"; do
    [[ -n "$p" ]] && kill "$p" 2>/dev/null && wait "$p" 2>/dev/null
  done
  mountpoint -q "$mnt" && umount -f "$mnt"
  [[ -n "$exported" ]] && exportfs -u "127.0.0.1:$exp"
  mountpoint -q "$exp" && umount "$exp"
  mountpoint -q "$exp" || mountpoint -q "$mnt" || rm -rf --one-file-system "$scratch"
}
trap cleanup EXIT

mkdir -p "$exp" "$mnt"
truncate -s 512M "$scratch/xfs.img"
mkfs.xfs -q -m reflink=1 "$scratch/xfs.img"
mount -o loop "$scratch/xfs.img" "$exp"
chmod 1777 "$exp"
export_scratch() { exportfs -o rw,sync,no_subtree_check,no_root_squash,fsid=4242 "127.0.0.1:$exp" && exported=1; }
export_scratch

# start_dump <area>: tcpdump on lo, packet-buffered, until stop_dump.
start_dump() {
  mkdir -p "$out/$1"
  tcpdump -Z root -i lo -U -s 0 -w "$out/$1/raw.pcap" 2>"$scratch/$1.log" &
  dump_pid=$!
  until grep -q listening "$scratch/$1.log"; do
    kill -0 "$dump_pid" || { cat "$scratch/$1.log" >&2; exit 1; }
    sleep 0.1
  done
}

# stop_dump <area>: keep the frames tshark reads as RPC.
stop_dump() {
  sleep 1 # bounded: lets the last replies cross lo before tcpdump stops
  kill "$dump_pid"; wait "$dump_pid" || true; dump_pid=""
  tshark -r "$out/$1/raw.pcap" -Y rpc -w "$out/$1/reference.pcap"
  rm "$out/$1/raw.pcap"
  echo "$1: $(tshark -r "$out/$1/reference.pcap" | wc -l) RPC frames"
}

# The python helper takes, tests and releases POSIX locks: lock <file> <secs>
# holds a write lock that long; test <file> asks F_GETLK; wait <file> blocks
# in F_SETLKW until the holder lets go. The client settles a conflict between
# its own processes before asking the server, so a blocked lock that knfsd
# grants by callback needs a holder on the server's side of the export.
lockpy='
import fcntl, os, struct, sys, time
op, path = sys.argv[1], sys.argv[2]
fd = os.open(path, os.O_RDWR)
if op == "lock":
    fcntl.lockf(fd, fcntl.LOCK_EX)
    time.sleep(float(sys.argv[3]))
    fcntl.lockf(fd, fcntl.LOCK_UN)
elif op == "test":
    fcntl.fcntl(fd, fcntl.F_GETLK, struct.pack("hhqqi", fcntl.F_WRLCK, 0, 0, 0, 0))
elif op == "wait":
    fcntl.lockf(fd, fcntl.LOCK_EX)
    fcntl.lockf(fd, fcntl.LOCK_UN)
'

v3opts=vers=3,proto=tcp,mountproto=tcp,port=2049

area_nfs3() {
  rpcinfo -T tcp 127.0.0.1 100005 3   # MOUNT NULL
  showmount -e 127.0.0.1               # MOUNT EXPORT
  mount -t nfs -o "$v3opts" "127.0.0.1:$exp" "$mnt"   # MOUNT MNT
  echo hello >"$mnt/nfs3-file"
  sync
  echo 3 >/proc/sys/vm/drop_caches
  cat "$mnt/nfs3-file" >/dev/null
  ls -l "$mnt" >/dev/null
  rm "$mnt/nfs3-file"
  umount "$mnt"                        # MOUNT UMNT
}

area_nlm() {
  # A fresh lockd forgets the hosts it monitors, so the first lock asks statd
  # to monitor the server again (SM_MON). The restart drops the export too.
  systemctl restart nfs-server
  export_scratch
  mount -t nfs -o "$v3opts" "127.0.0.1:$exp" "$mnt"
  echo lock >"$mnt/nlm-file"
  python3 -c "$lockpy" test "$mnt/nlm-file"  # NLM_TEST: no local holder, so it asks
  python3 -c "$lockpy" lock "$exp/nlm-file" 3 &
  holder=$!
  sleep 1 # bounded: the holder takes its lock before the waiter asks
  python3 -c "$lockpy" wait "$mnt/nlm-file" # blocked, then granted by callback
  wait "$holder"; holder=""
  sm-notify -f                          # the SM_NOTIFY a restart sends
  sleep 1 # bounded: statd answers the notify before the unmount
  rm "$mnt/nlm-file"
  umount "$mnt"
}

# v4 <minor>: open/write/read, a lock, and a read delegation recalled by a
# write through the export on the server's side.
v4() {
  mount -t nfs -o "vers=4.$1" "127.0.0.1:$exp" "$mnt"
  echo hello >"$mnt/nfs4-file"
  python3 -c "$lockpy" lock "$mnt/nfs4-file" 0
  umount "$mnt"
  echo deleg >"$exp/nfs4-deleg"
  mount -t nfs -o "vers=4.$1" "127.0.0.1:$exp" "$mnt"
  sleep 1 # bounded: the callback path is up before the OPEN asks for a delegation
  exec 3<"$mnt/nfs4-deleg"             # OPEN for read: a read delegation
  cat <&3 >/dev/null
  echo server >>"$exp/nfs4-deleg"      # the server's own write recalls it
  sleep 2 # bounded: the client answers CB_RECALL and returns the delegation
  exec 3<&-
  rm "$mnt/nfs4-file" "$mnt/nfs4-deleg"
  umount "$mnt"
}
area_nfs40() { v4 0; }
area_nfs41() { v4 1; }

area_nfs42() {
  mount -t nfs -o vers=4.2 "127.0.0.1:$exp" "$mnt"
  f="$mnt/nfs42-file"
  xfs_io -f -c "pwrite -q 0 64k" -c fsync "$f"
  xfs_io -c "falloc 64k 64k" -c "fpunch 0 4k" -c "seek -d 0" -c "seek -h 4096" "$f"
  xfs_io -f -c "copy_range -s 8192 -d 0 -l 8192 $f" "$mnt/nfs42-copy"
  cp --reflink=always "$f" "$mnt/nfs42-clone"
  setfattr -n user.dittofs -v reference "$f"
  echo 3 >/proc/sys/vm/drop_caches     # GETXATTR asks the server, not the cache
  getfattr -n user.dittofs "$f"
  getfattr -d "$f"
  setfattr -x user.dittofs "$f"
  rm "$f" "$mnt/nfs42-copy" "$mnt/nfs42-clone"
  umount "$mnt"
}

for a in "${areas[@]}"; do
  start_dump "$a"
  "area_$a"
  stop_dump "$a"
done
