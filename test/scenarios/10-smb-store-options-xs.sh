#!/usr/bin/env bash
# Store options are kept or refused, never silently dropped, clamped or replaced by a default (RFC 13
# §6: "Unknown fields are refused"). A setting bound to stored content cannot change under it (RFC 13
# §5.1). Each option is now a setting at the scope RFC 13 gives it: compression the remote store's,
# upload workers the node's, encryption the namespace's, the bucket the store's, the prefix the
# namespace's.
#   1. Each of compression, upload workers and encryption must show up where it was set, or the write
#      must fail and name it.
#   2. Bad values must be refused: an unknown compression, an empty one, a negative upload_workers,
#      and a misspelt key ("remote.compresion", which would leave it off).
#   3. Once a share has data in a namespace, changing its store's bucket or its prefix must be refused,
#      and the data must still read back after a restart, when a stored change takes effect.
# Failed under v1's CLI at all three: each flag next to --config was dropped; "compresion" was accepted
# and ignored; both edits were accepted, and after the restart the share looked for its blocks under the
# new prefix (NT_STATUS_UNEXPECTED_IO_ERROR, "CAS object missing for live FileChunk").
rclone mkdir s3:opts
rclone mkdir s3:other
NODE="$(dfsctl node list -o json | jq -r '.[0].id')"

# 1. Each option at its scope, then read back
dfs-store opts --setting remote.compression=zstd >/tmp/o1.out 2>&1 || echo "exit $?" >/tmp/o1.rc
dfsctl setting set "node:$NODE" upload_workers 4 >/tmp/o2.out 2>&1 || echo "exit $?" >/tmp/o2.rc
dfsctl namespace create --store "$(dfs-store opts)" --setting namespace.prefix=o3/ --setting namespace.encrypts=true >/tmp/o3.out 2>&1 || echo "exit $?" >/tmp/o3.rc
dfsctl store list -o json >/tmp/stores.json
dfsctl namespace list -o json >/tmp/namespaces.json
dfsctl setting get "node:$NODE" upload_workers -o json >/tmp/workers.json || true

# 2. Bad values
dfs-store opts --setting remote.compression=bogus >/tmp/b1.out 2>&1 || echo "exit $?" >/tmp/b1.rc
dfs-store opts --setting remote.compression='""' >/tmp/b2.out 2>&1 || echo "exit $?" >/tmp/b2.rc
dfsctl setting set "node:$NODE" upload_workers -1 >/tmp/b3.out 2>&1 || echo "exit $?" >/tmp/b3.rc
dfs-store opts --setting remote.compresion=zstd >/tmp/b4.out 2>&1 || echo "exit $?" >/tmp/b4.rc

# 3. A namespace with content: its store's bucket and its prefix must not change
S="$(dfs-store opts)"
N="$(dfsctl namespace create --store "$S" --setting namespace.prefix=used/ -o json | jq -r .id)"
dfsctl share create used --path /used --namespace "$N" --flavor sys --client 0.0.0.0/0=read-write
dfsctl grant set used --user tester --access read-write
head -c 8M /dev/urandom >/tmp/u.bin
smbclient //127.0.0.1/used -c 'lcd /tmp; put u.bin'
dfsctl system drain-uploads
dfsctl store update "$S" --setting remote.bucket=other >/tmp/e1.out 2>&1 || echo "exit $?" >/tmp/e1.rc
dfsctl setting set "namespace:$N" namespace.prefix moved/ >/tmp/e2.out 2>&1 || echo "exit $?" >/tmp/e2.rc
dfsctl store get "$S" -o json | jq -c '.spec.settings["remote.bucket"]' | tee /tmp/bucket.json
dfsctl namespace get "$N" -o json | jq -c '.spec.settings["namespace.prefix"]' | tee /tmp/prefix.json
dfs-server stop
dfs-server start
timeout 120 smbclient //127.0.0.1/used -c 'get u.bin /tmp/back.bin' || true

# Outcomes first, then the checks: each option kept, or the refusal naming it
for f in o1 o2 o3 b1 b2 b3 b4 e1 e2; do echo "== $f $(cat "/tmp/$f.rc" 2>/dev/null || echo accepted)"; cat "/tmp/$f.out"; done
jq -c '.[].spec.settings' /tmp/stores.json /tmp/namespaces.json
cat /tmp/workers.json
jq -e 'any(.[]; .spec.settings["remote.compression"] == "zstd")' /tmp/stores.json || grep -q remote.compression /tmp/o1.out
jq -e '.spec.value == 4' /tmp/workers.json || grep -q upload_workers /tmp/o2.out
jq -e 'any(.[]; .spec.settings["namespace.encrypts"] == true)' /tmp/namespaces.json || grep -q namespace.encrypts /tmp/o3.out
test -e /tmp/b1.rc
test -e /tmp/b2.rc
test -e /tmp/b3.rc
test -e /tmp/b4.rc
test -e /tmp/e1.rc
test -e /tmp/e2.rc
test "$(cat /tmp/bucket.json)/$(cat /tmp/prefix.json)" = '"opts"/"used/"'
cmp /tmp/back.bin /tmp/u.bin
