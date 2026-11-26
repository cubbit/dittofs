#!/usr/bin/env bash
# Issue #2897: with compression, a chunk zstd cannot shrink is stored as itself, unmarked, and a read
# takes any body starting with DFCMP plus a valid algorithm byte for a frame: a small file starting that
# way, random after it, uploads fine but cannot be read back once evicted. Control: another first letter.
# Compression is the remote store's setting.

rclone mkdir s3:zstd
S="$(dfs-store zstd --setting remote.compression=zstd)"
N="$(dfsctl namespace create --store "$S" --setting namespace.prefix=zstd/ -o json | jq -r .id)"
dfsctl share create zstd --path /zstd --namespace "$N" --flavor sys --client 0.0.0.0/0=read-write
dfsctl grant set zstd --user tester --access read-write

mkdir /tmp/z /tmp/back
head -c 64K /dev/urandom >/tmp/random.bin
printf 'DFCMP\001' | cat - /tmp/random.bin >/tmp/z/magic.bin
printf 'XFCMP\001' | cat - /tmp/random.bin >/tmp/z/plain.bin
(cd /tmp/z && sha256sum magic.bin plain.bin >/tmp/z.sha256)
smbclient //127.0.0.1/zstd -c 'lcd /tmp/z; put magic.bin; put plain.bin'
dfsctl system drain-uploads
smbclient //127.0.0.1/zstd -c 'lcd /tmp/back; get plain.bin; get magic.bin' || true
(cd /tmp/back && sha256sum -c /tmp/z.sha256) || true

cmp /tmp/z/plain.bin /tmp/back/plain.bin
cmp /tmp/z/magic.bin /tmp/back/magic.bin
