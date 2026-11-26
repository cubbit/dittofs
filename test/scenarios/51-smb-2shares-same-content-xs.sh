#!/usr/bin/env bash
# A-07: two shares on different block stores but one metadata store. A file
# written to /export, then the same content to /cubbit: /cubbit's bucket must get the data, and
# /cubbit must read it back after its caches are evicted. Fails today: the upload is skipped
# (the content is already known) and the cold read on /cubbit returns an I/O error.

rclone mkdir s3:export
S="$(dfs-store export)"
N="$(dfsctl namespace create --store "$S" --setting namespace.prefix=export/ -o json | jq -r .id)"
dfsctl share create export --path /export --namespace "$N" --flavor sys --client 0.0.0.0/0=read-write
dfsctl grant set export --user tester --access read-write
rclone mkdir s3:cubbit
S="$(dfs-store cubbit)"
N="$(dfsctl namespace create --store "$S" --setting namespace.prefix=cubbit/ -o json | jq -r .id)"
dfsctl share create cubbit --path /cubbit --namespace "$N" --flavor sys --client 0.0.0.0/0=read-write
dfsctl grant set cubbit --user tester --access read-write

head -c 4M /dev/urandom >/tmp/d.bin
(cd /tmp && sha256sum d.bin >/tmp/d.sha256)
smbclient //127.0.0.1/export -c 'lcd /tmp; put d.bin'
dfsctl system drain-uploads
test "$(rclone size --json s3:export | jq .count)" -gt 0
smbclient //127.0.0.1/cubbit -c 'lcd /tmp; put d.bin'
dfsctl system drain-uploads
test "$(rclone size --json s3:cubbit | jq .count)" -gt 0
mkdir /tmp/back
smbclient //127.0.0.1/cubbit -c 'lcd /tmp/back; get d.bin'
(cd /tmp/back && sha256sum -c --quiet /tmp/d.sha256)
