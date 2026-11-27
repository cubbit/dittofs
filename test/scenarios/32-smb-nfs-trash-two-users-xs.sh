#!/usr/bin/env bash
# Issues #2920 and #2921: the recycle bin with two users. The first delete creates #recycle owned by the
# deleting user, mode 0700, and a delete that cannot be recycled is refused, so every other user's delete
# then fails. dfsctl trash empty must refuse bob, who is no admin, but over SMB a non-admin can still
# delete from #recycle. The new CLI has no recycle bin: no share setting for it and no dfsctl trash
# command, so a delete creates no #recycle and the run stops at the nfs-ls check for it. The last check,
# that trash empty's refusal is not an unknown command, cannot run until the recycle bin returns.

dfsctl user create bob --uid 2002 --role viewer
dfsctl group create bob --gid 2002
dfsctl group add-member bob bob
echo bob-secret >/tmp/bob.password
dfsctl user set-password bob --password-file /tmp/bob.password
XDG_CONFIG_HOME=/tmp/bob dfsctl login --server "$(dfsctl context current -o json | jq -r .server_url)" --username bob --password-file /tmp/bob.password
N="$(dfsctl share get test -o json | jq -r .spec.namespace)"
dfsctl share create shared --path /shared --namespace "$N" --flavor sys --client 0.0.0.0/0=read-write
dfsctl grant set shared --user tester --access read-write
dfsctl grant set shared --user bob --access read-write

smbclient //127.0.0.1/shared -c 'put /etc/hostname t1.txt'
smbclient //127.0.0.1/shared -U bob%bob-secret -c 'put /etc/hostname b1.txt; put /etc/hostname b2.txt'
smbclient //127.0.0.1/shared -c 'del t1.txt'
nfs-ls 'nfs://127.0.0.1/shared?version=4&uid=1000&gid=1000' | grep recycle
smbclient //127.0.0.1/shared -U bob%bob-secret -c 'del b1.txt' || true
nfs-io unlink 'nfs://127.0.0.1/shared/b2.txt?version=4&uid=2002&gid=2002' || true
smbclient //127.0.0.1/shared -c 'ls "#recycle/*"'
smbclient //127.0.0.1/shared -c 'del "#recycle/t1.txt"' || true
smbclient //127.0.0.1/shared -c 'ls "#recycle/*"' || true
XDG_CONFIG_HOME=/tmp/bob dfsctl trash empty /shared --force >/tmp/empty.out 2>&1 && echo emptied >/tmp/empty || echo refused >/tmp/empty
smbclient //127.0.0.1/shared -U bob%bob-secret -c 'ls' | tee /tmp/root.ls

test -z "$(grep 'b1\.txt' /tmp/root.ls)"
test -z "$(grep 'b2\.txt' /tmp/root.ls)"
smbclient //127.0.0.1/shared -c 'ls "#recycle/t1.txt"'
test "$(cat /tmp/empty)" = refused
if grep -q 'unknown command' /tmp/empty.out; then exit 1; fi
