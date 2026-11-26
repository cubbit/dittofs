#!/usr/bin/env bash
# Two namespaces on the same bucket and prefix, each on a remote store of its own (RFC 6 §2.6, App A;
# RFC 9 §5.3): they are one remote namespace under two names, so GC on one can delete the other's
# objects. The second namespace must be refused.
# If it is accepted, /x and /y get a file each; deleting /x's file, GC on /x and a reclaim must leave /y's
# objects and data intact.
# Fails today at the last check: the second store is accepted. /y's data survives (GC reports 8 objects
# swept, the bucket keeps its 4).

rclone mkdir s3:dittofs/shared
S="$(dfs-store dittofs)"
NX="$(dfsctl namespace create --store "$S" --setting namespace.prefix=shared/ -o json | jq -r .id)"
S="$(dfs-store dittofs)"
NY="$(dfsctl namespace create --store "$S" --setting namespace.prefix=shared/ -o json 2>/tmp/ny.err | jq -r .id)" && Y=accepted || Y=refused
cat /tmp/ny.err
echo "second namespace on the same bucket and prefix: $Y"
if test "$Y" = accepted; then
  dfsctl share create x --path /x --namespace "$NX" --flavor sys --client 0.0.0.0/0=read-write
  dfsctl grant set x --user tester --access read-write
  dfsctl share create y --path /y --namespace "$NY" --flavor sys --client 0.0.0.0/0=read-write
  dfsctl grant set y --user tester --access read-write
  mkdir /tmp/x /tmp/y /tmp/back
  head -c 8M /dev/urandom >/tmp/x/x.bin
  head -c 8M /dev/urandom >/tmp/y/y.bin
  (cd /tmp/y && sha256sum y.bin >/tmp/y.sha256)
  smbclient //127.0.0.1/x -c 'lcd /tmp/x; put x.bin'
  smbclient //127.0.0.1/y -c 'lcd /tmp/y; put y.bin'
  dfsctl system drain-uploads
  rclone size --json s3:dittofs/shared | tee /tmp/s0.json
  smbclient //127.0.0.1/x -c 'del x.bin'
  dfsctl store block gc /x --grace-period 0 -o json
  dfsctl store block reclaim
  rclone size --json s3:dittofs/shared | tee /tmp/s1.json
  smbclient //127.0.0.1/y -c 'lcd /tmp/back; get y.bin'
  test "$(jq .bytes /tmp/s1.json)" -ge "$(stat -c %s /tmp/y/y.bin)"
  (cd /tmp/back && sha256sum -c /tmp/y.sha256)
fi
test "$Y" = refused
# The refusal must be the overlap, named by the setting, not some other failure
if grep -q unknown_key /tmp/ny.err; then exit 1; fi
grep -Eq 'namespace.prefix|remote.bucket' /tmp/ny.err
