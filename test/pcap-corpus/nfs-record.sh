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
#   gss    RPCSEC_GSS: a throwaway KDC in the harness's image (dittofs-dt-kdc,
#          built from test/smb-conformance/kdc), the export re-exported
#          sec=krb5:krb5i:krb5p, and for NFSv3 with MOUNT, NFSv4.0 and
#          NFSv4.1 a mount at each of krb5, krb5i and krb5p, where a user
#          principal stats, writes and reads a file before the unmount. Its
#          nfs/ keytab is kept as <outdir>/gss/nfs.keytab, rpc.gssd's and
#          rpc.svcgssd's logs beside it. It restarts nfs-server, and runs
#          only when named, since it needs docker and the image
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
# export and its loop-mounted image, and for gss its KDC container, its
# rpc.gssd and rpc.svcgssd and the user's credential cache.
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
kdc="" gssd="" svcgssd=""

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
  for p in "$gssd" "$svcgssd"; do
    [[ -n "$p" ]] && kill "$p" 2>/dev/null && wait "$p" 2>/dev/null
  done
  [[ -n "$kdc" ]] && docker rm -f "$kdc" >/dev/null
  [[ -n "$exported" ]] && exportfs -u "127.0.0.1:$exp" && exportfs -f
  # Polled, bounded at 10 s: knfsd's file cache lets go of the export's files
  # a moment after the export is gone, and until then the unmount is busy.
  local i
  for ((i = 0; i < 100; i++)); do
    mountpoint -q "$exp" || break
    umount "$exp" 2>/dev/null || sleep 0.1
  done
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

# The gss area's realm. Its keys protect nothing but the capture: the nfs/
# keytab is committed beside it so tests can decrypt the captured tickets.
# The user principal is nobody@ so that knfsd maps it to the host's nobody
# user rather than to no uid, which it refuses to create files as.
realm=DITTOFS.TEST kdc_port=18888 gss_user=nobody gss_uid=65534

# as_user <cmd...>: run as the user principal's uid, whose credential cache
# rpc.gssd finds under its -d directory.
as_user() { setpriv --reuid="$gss_uid" --regid="$gss_uid" --clear-groups "$@"; }

area_gss() {
  for t in docker rpc.gssd rpc.svcgssd kinit setpriv; do
    command -v "$t" >/dev/null || { echo "missing: $t" >&2; exit 2; }
  done
  # rpc.gssd names the server by a reverse lookup of 127.0.0.1, the address
  # mounted, so the nfs/ principal holds the host's name for it.
  local srv krb="$scratch/krb" cc="$scratch/ccache"
  srv="$(getent hosts 127.0.0.1 | awk '{print $2; exit}')"
  mkdir -p "$krb" "$cc"
  chmod 755 "$scratch" # the user reaches $mnt through it
  chmod 1777 "$cc"
  kdc="nfs-record-kdc-$$"
  docker run -d --rm --name "$kdc" --network host -v "$krb:/keytabs" \
    -e KRB5_REALM="$realm" -e KDC_HOST=127.0.0.1 -e KDC_PORT="$kdc_port" \
    -e DITTOFS_SPN="nfs/$srv" -e DITTOFS_UID=0 -e DITTOFS_GID=0 \
    -e USER_PRINCIPAL="$gss_user" -e USER_PASSWORD=nfs-record dittofs-dt-kdc >/dev/null
  export KRB5_CONFIG="$krb/krb5.conf" KRB5_KTNAME="$krb/dittofs.keytab"
  # Until the KDC answers with the keytab it wrote, bounded at 60 s.
  local i
  for ((i = 0; i < 600; i++)); do
    [[ -s $krb/dittofs.keytab ]] && kinit -k -c "MEMORY:probe" "nfs/$srv@$realm" 2>/dev/null && break
    sleep 0.1
  done
  ((i < 600)) || { docker logs "$kdc" >&2; exit 1; }
  # The client's machine principal, which rpc.gssd finds in its own keytab.
  docker exec "$kdc" kadmin.local -q "addprinc -randkey host/$(hostname -f)@$realm" >/dev/null
  docker exec "$kdc" kadmin.local -q "ktadd -k /keytabs/client.keytab host/$(hostname -f)@$realm" >/dev/null
  kinit -c "FILE:$cc/krb5cc_$gss_uid" "$gss_user@$realm" <<<nfs-record >/dev/null
  chown "$gss_uid:$gss_uid" "$cc/krb5cc_$gss_uid"
  cp "$krb/dittofs.keytab" "$out/gss/nfs.keytab"

  # A restart forgets the NFSv4 clients of earlier areas, whose AUTH_SYS
  # principal would make the client's krb5 SETCLIENTID NFS4ERR_CLID_INUSE.
  systemctl restart nfs-server
  rpc.svcgssd -f -vvv -p "nfs/$srv@$realm" 2>"$scratch/svcgssd.log" &
  svcgssd=$!
  rpc.gssd -f -vvv -k "$krb/client.keytab" -d "$cc" 2>"$scratch/gssd.log" &
  gssd=$!
  # fsid=0 makes the export NFSv4's root: knfsd's generated pseudo-root
  # allows only AUTH_SYS, so a sec=krb5 PUTROOTFH there is NFS4ERR_WRONGSEC.
  exportfs -o rw,sync,no_subtree_check,no_root_squash,fsid=0,sec=krb5:krb5i:krb5p "127.0.0.1:$exp"

  local m vers path sec f
  for m in "vers=3,proto=tcp,mountproto=tcp,port=2049 $exp" "vers=4.0 /" "vers=4.1 /"; do
    read -r vers path <<<"$m"
    for sec in krb5 krb5i krb5p; do
      mount -t nfs -o "$vers,sec=$sec" "127.0.0.1:$path" "$mnt"
      f="$mnt/gss-$sec"
      as_user stat "$mnt" >/dev/null
      as_user sh -c "echo hello >'$f'"
      echo 3 >/proc/sys/vm/drop_caches
      as_user cat "$f" >/dev/null
      as_user rm "$f"
      umount "$mnt"
    done
  done

  kill "$gssd" "$svcgssd"; wait "$gssd" "$svcgssd" || true; gssd="" svcgssd=""
  cp "$scratch/gssd.log" "$scratch/svcgssd.log" "$out/gss/"
  docker rm -f "$kdc" >/dev/null; kdc=""
  unset KRB5_CONFIG KRB5_KTNAME
  export_scratch
}

for a in "${areas[@]}"; do
  start_dump "$a"
  "area_$a"
  stop_dump "$a"
done
