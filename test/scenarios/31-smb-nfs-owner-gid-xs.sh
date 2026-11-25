#!/usr/bin/env bash
# Issue #2922: the group owner of a new file over SMB. bob is created with uid 2002 and is the one
# member of group bob, gid 2002, so the SMB session's GID is 2002 and bob's SMB files belong to it.
# Before, the session fell back to 1000 (tester's group) without reading the user's own GID.
# Control: the same user over NFS, whose AUTH_SYS credential carries gid 2002.

dfsctl user create bob --uid 2002
dfsctl group create bob --gid 2002
dfsctl group add-member bob bob
echo bob-secret >/tmp/bob.password
dfsctl user set-password bob --password-file /tmp/bob.password
dfsctl user list -o json | jq -c '.[] | {name: .spec.name, uid: .spec.uid}'
dfsctl group list -o json
N="$(dfsctl share get test -o json | jq -r .spec.namespace)"
dfsctl share create own --path /own --namespace "$N" --flavor sys --client 0.0.0.0/0=read-write
dfsctl grant set own --user bob --access read-write

smbclient //127.0.0.1/own -U bob%bob-secret -c 'put /etc/hostname smb.txt'
nfs-cp /etc/hostname 'nfs://127.0.0.1/own/nfs.txt?version=4&uid=2002&gid=2002'
nfs-ls 'nfs://127.0.0.1/own?version=4&uid=2002&gid=2002' | tee /tmp/own.ls

test "$(awk '$6 == "nfs.txt" {print $3 ":" $4}' /tmp/own.ls)" = 2002:2002
test "$(awk '$6 == "smb.txt" {print $3 ":" $4}' /tmp/own.ls)" = 2002:2002
