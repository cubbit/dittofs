#!/usr/bin/env bash
# A namespace that encrypts, under the storage key setup's dfs init made and its passphrase. No
# plaintext may reach the bucket, a cold read must round-trip, also after a restart, and with a wrong
# passphrase the server must refuse to start instead of writing unencrypted (RFC 5 §2.7, §5.1).
# Passed with v1's per-store key file: no marker reached the bucket, and the cold reads matched before
# and after a restart. With a wrong passphrase the server started, but the share was not served: a tree
# connect got NT_STATUS_BAD_NETWORK_NAME, the log said the key provider failed, and the bucket was
# unchanged. The storage key is now the installation's, so a wrong passphrase stops the start: dfs
# exits with "the storage key: keyprovider: unwrap failed" before it listens.

# The store, a namespace that encrypts, the share and a file with a plaintext marker in it
rclone mkdir s3:enc
N="$(dfsctl namespace create --store "$(dfs-store enc)" --setting namespace.prefix=enc/ --setting namespace.encrypts=true -o json | jq -r .id)"
dfsctl share create enc --path /enc --namespace "$N" --flavor sys --client 0.0.0.0/0=read-write
dfsctl grant set enc --user tester --access read-write
awk 'BEGIN { for (i = 0; i < 150000; i++) print "PLAINTEXT-MARKER-0123456789" }' >/tmp/m.bin
head -c 16M /dev/urandom >>/tmp/m.bin
smbclient //127.0.0.1/enc -c 'lcd /tmp; put m.bin'
dfsctl system drain-uploads
dfsctl namespace get "$N" -o json | tee /tmp/namespace.json
rclone size --json s3:enc | tee /tmp/enc0.json
rclone cat s3:enc | grep -a -c PLAINTEXT-MARKER | tee /tmp/marker.count || true

# Cold reads: now, and after a restart with the same passphrase
smbclient //127.0.0.1/enc -c 'get m.bin /tmp/back1.bin'
dfs-server stop
dfs-server start
smbclient //127.0.0.1/enc -c 'get m.bin /tmp/back2.bin'

# A wrong passphrase: the start must fail on the storage key, so nothing is served
dfs-server stop
dfs-server start DITTOFS_ENCRYPTION_PASSPHRASE=wrong-passphrase >/tmp/wrong.out 2>&1 || echo "exit $?" >/tmp/wrong.rc

# Outcomes first, then the checks
cat /tmp/wrong.out
cat /tmp/*.rc 2>/dev/null || true
grep 'the storage key' /logs/dfs.log || true
jq -e '.spec.settings["namespace.encrypts"] == true' /tmp/namespace.json
test "$(cat /tmp/marker.count)" -eq 0
cmp /tmp/back1.bin /tmp/m.bin
cmp /tmp/back2.bin /tmp/m.bin
test -e /tmp/wrong.rc
grep -q 'the storage key' /logs/dfs.log
