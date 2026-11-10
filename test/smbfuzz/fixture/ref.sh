#!/bin/bash
# The Samba reference of DESIGN.md §3.2: a member server of the AD-DC fixture's
# domain, serving [profiles] from a loop-mounted ext4 with Windows ACLs only.
# It runs in a container of the AD-DC image, which already carries samba,
# winbind and krb5-user, started with this script as its entrypoint.
set -euo pipefail

REALM="${AD_REALM:-DITTOFS.AD}"
DOMAIN="${AD_DOMAIN:-DITTOFS}"
DC_IP="${DC_IP:?}"
ADMIN_PASSWORD="${AD_ADMIN_PASSWORD:-Passw0rd!2024}"
realm_lc="${REALM,,}"

log() { echo "[ref] $*"; }

echo "$DC_IP dc.$realm_lc dc" >> /etc/hosts
printf 'search %s\nnameserver %s\n' "$realm_lc" "$DC_IP" > /etc/resolv.conf

cat > /etc/krb5.conf <<EOF
[libdefaults]
    default_realm = $REALM
    dns_lookup_realm = false
    dns_lookup_kdc = false
[realms]
    $REALM = {
        kdc = $DC_IP
        admin_server = $DC_IP
    }
[domain_realm]
    .$realm_lc = $REALM
    $realm_lc = $REALM
EOF

mkdir -p /srv/fuzz
if ! mountpoint -q /srv/fuzz; then
    truncate -s 8G /var/lib/fuzz.img
    mkfs.ext4 -q -F /var/lib/fuzz.img
    mount -o loop,user_xattr /var/lib/fuzz.img /srv/fuzz
fi
chmod 0777 /srv/fuzz

# Every setting below is the documented Windows-ACL-only share. A setting
# added for a divergence says which one beside it (DESIGN.md §3.2).
rm -f /etc/samba/smb.conf
cat > /etc/samba/smb.conf <<EOF
[global]
    security = ads
    realm = $REALM
    workgroup = $DOMAIN
    netbios name = REF
    idmap config * : backend = tdb
    idmap config * : range = 3000-7999
    idmap config $DOMAIN : backend = rid
    idmap config $DOMAIN : range = 10000-999999
    winbind use default domain = no
    server min protocol = SMB3_11
    kernel oplocks = no
    ea support = yes
    store dos attributes = yes
    map acl inherit = yes
    log level = 1
    dedicated keytab file = /etc/krb5.keytab
    kerberos method = secrets and keytab

[profiles]
    path = /srv/fuzz
    read only = no
    vfs objects = acl_xattr
    acl_xattr:ignore system acls = yes
    acl_xattr:default acl style = windows
EOF

log "joining $REALM"
for _ in 1 2 3 4 5; do
    if net ads join -U "Administrator%$ADMIN_PASSWORD" -S "dc.$realm_lc" --no-dns-updates; then
        break
    fi
    sleep 3
done
net ads testjoin

winbindd -D
smbd -D
for _ in $(seq 1 30); do
    wbinfo -p >/dev/null 2>&1 && wbinfo -u >/dev/null 2>&1 && break
    sleep 1
done
# The security privilege for the SACL (DESIGN.md §7.6).
net sam rights grant "$DOMAIN\\Domain Admins" SeSecurityPrivilege
net sam rights grant "$DOMAIN\\Administrator" SeSecurityPrivilege
# The share root's descriptor: Everyone full control, inherited by every round
# directory the admin creates. Without a stored one Samba synthesizes one from
# the Unix mode, which grants the domain admin nothing.
samba-tool ntacl set "O:BAG:DUD:PAI(A;OICI;0x1f01ff;;;WD)" /srv/fuzz --service=profiles
touch /srv/.ready
log "ready"
exec tail -f /var/log/samba/log.smbd
