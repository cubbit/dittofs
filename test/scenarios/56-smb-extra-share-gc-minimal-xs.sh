#!/usr/bin/env bash
# The smallest check for A-08: a second block store on md (/extra, own bucket) beside setup.sh's
# /test. Each round deletes one file and runs GC on /extra; its bucket must shrink. A-08 failed it
# whenever GC visited /test's remote first. 55-smb-extra-share-gc-xs.sh is the same setup with the full checks.
rclone mkdir s3:extra
N="$(dfsctl namespace create --store "$(dfs-store extra)" --setting namespace.prefix=extra/ -o json | jq -r .id)"
dfsctl share create extra --path /extra --namespace "$N" --flavor sys --client 0.0.0.0/0=read-write
dfsctl grant set extra --user tester --access read-write
mkdir /tmp/up
for i in 1 2 3 4 5 6 7; do echo "$i" >"/tmp/up/f$i"; done
smbclient //127.0.0.1/extra -c 'lcd /tmp/up; prompt; mput f*'
dfsctl system drain-uploads
for i in 1 2 3 4 5 6 7; do before="$(rclone size --json s3:extra | jq .count)"; smbclient //127.0.0.1/extra -c "del f$i"; dfsctl store block gc /extra --grace-period 0 -o json >/dev/null; test "$(rclone size --json s3:extra | jq .count)" -lt "$before"; done
