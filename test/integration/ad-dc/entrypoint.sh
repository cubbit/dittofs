#!/bin/bash
# Samba Active Directory Domain Controller bootstrap for AD-1 PAC testing.
#
# Provisions a fresh AD domain on first start, then creates test users and
# groups that exercise the Kerberos PAC group-SID path:
#
#   Users:
#     alice  -- member of devs (nested under engineering) + RFC2307 attrs set
#     bob    -- member of engineering only, NO RFC2307 attrs (RID-fallback case)
#
#   Groups:
#     engineering -- top-level group (group-level RFC2307 gidNumber is left to
#                    AD-3, which is the consumer; AD-1 only needs membership)
#     devs        -- nested member of engineering (so alice's PAC carries BOTH
#                    devs and engineering SIDs: AD resolves nesting at the DC)
#
# alice's ticket therefore carries a PAC whose GroupMembershipSIDs include the
# devs and engineering domain group SIDs. That is the property AD-1's test
# asserts: PAC group SIDs flow into the Kerberos AuthResult.
#
# A service keytab for the DittoFS SMB/NFS principals is exported to the shared
# /keytabs volume so the dittofs server (or a Go test) can authenticate tickets.
#
# RFC2307 attrs vs RID fallback: alice gets uidNumber/gidNumber (the idmap_ad
# path AD-3 will read over LDAP); bob gets none (the idmap_rid algorithmic-
# fallback path). AD-1 does not consume these user attrs — they are provisioned
# now so the AD-2/AD-3 PRs gate against the same fixture. Group-level gidNumbers
# are AD-3's concern and are stamped there when that path is built.

set -euo pipefail

REALM="${AD_REALM:-DITTOFS.AD}"
DOMAIN="${AD_DOMAIN:-DITTOFS}"
ADMIN_PASSWORD="${AD_ADMIN_PASSWORD:-Passw0rd!2024}"
KEYTAB_DIR="${KEYTAB_DIR:-/keytabs}"
# The provisioned smb.conf names the TLS files issue_tls_cert writes here.
TLS_DIR=/var/lib/samba/private/tls

# The DC's dynamic RPC ports, NETLOGON's on ncacn_ip_tcp among them, which the
# endpoint mapper on 135 hands out. Pinned to a small range, as a firewalled
# site pins a Windows DC's, so a firewall guide can name it. Read at provision
# only; the tests pass it from fixture_test.go's adRPCPortLow/adRPCPortHigh.
RPC_PORT_RANGE="${RPC_PORT_RANGE:-50000-50019}"

# Service principals the DittoFS server presents to clients, and the account
# that holds them.
SERVICE_ACCOUNT="${SERVICE_ACCOUNT:-dittofs-svc}"
SMB_SPN="${SMB_SPN:-cifs/dittofs.${REALM,,}}"
NFS_SPN="${NFS_SPN:-nfs/dittofs.${REALM,,}}"

# Test users + groups.
USER_ALICE="${USER_ALICE:-alice}"
USER_BOB="${USER_BOB:-bob}"
USER_PASSWORD="${USER_PASSWORD:-TestPassword01!}"

# UID/GID the dittofs container runs as; the exported keytab must be readable
# there. Defaults match the smb-conformance harness.
DITTOFS_UID="${DITTOFS_UID:-65532}"
DITTOFS_GID="${DITTOFS_GID:-65532}"

log() { echo "[AD-DC] $*"; }

mkdir -p "$KEYTAB_DIR"

# samba-tool domain provision is destructive and one-shot. Use the presence of
# the provisioned config as the "already initialised" sentinel so a container
# restart re-uses the existing domain database rather than re-provisioning.
if [ ! -f /var/lib/samba/.provisioned ]; then
    log "Provisioning AD domain $DOMAIN / realm $REALM"

    # Remove any stub smb.conf so provision writes a clean AD-DC config.
    rm -f /etc/samba/smb.conf

    # Provision the domain controller. SAMBA_INTERNAL DNS keeps the fixture
    # self-contained (no bind9 dependency). --use-rfc2307 enables the RFC2307
    # POSIX attribute schema so we can stamp uidNumber/gidNumber on some objects.
    #
    # The DC functional level is intentionally left at samba's default: the
    # "ad dc functional level" smb.conf parameter (and a fixed 2016 level) is not
    # accepted across all samba versions in debian:bookworm-slim, and the fixture
    # needs none of it — PAC group SIDs, RFC2307 attrs, and Kerberos all work at
    # the default level. Pinning it broke provisioning on a freshly rebuilt image
    # (issue #1252).
    samba-tool domain provision \
        --use-rfc2307 \
        --realm="$REALM" \
        --domain="$DOMAIN" \
        --server-role=dc \
        --dns-backend=SAMBA_INTERNAL \
        --adminpass="$ADMIN_PASSWORD" \
        --option="rpc server dynamic port range = $RPC_PORT_RANGE" \
        --option="tls certfile = $TLS_DIR/cert.pem" \
        --option="tls keyfile = $TLS_DIR/key.pem" \
        --option="tls cafile = $TLS_DIR/ca.pem"

    # Samba writes its own krb5.conf during provision; expose it to other
    # containers / the Go test on the shared volume.
    cp /var/lib/samba/private/krb5.conf "$KEYTAB_DIR/krb5.conf"
    chmod 644 "$KEYTAB_DIR/krb5.conf"

    touch /var/lib/samba/.provisioned
    log "Provision complete"
fi

# The DC serves LDAPS and StartTLS with a certificate from a CA made here once
# per domain and exported as $KEYTAB_DIR/ca.pem, so a client verifies the DC
# rather than skipping verification. Samba's own generated certificate names
# the DC in its CN only, which Go refuses. The provisioned smb.conf names the
# files written here, so Samba serves them rather than generating its own. The
# certificate is issued again at every start because its SANs carry the
# container's IPs, which a restart can change; its DNS SAN is the DC's host
# name, fixed by the container's --hostname.
issue_tls_cert() {
    local tls="$TLS_DIR" fqdn san ip
    fqdn="$(hostname -s).${REALM,,}"
    mkdir -p "$tls"
    if [ ! -f "$tls/ca.key" ]; then
        log "Creating the fixture CA"
        openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=DittoFS AD-DC test CA" \
            -addext basicConstraints=critical,CA:TRUE \
            -addext keyUsage=critical,keyCertSign,cRLSign \
            -keyout "$tls/ca.key" -out "$tls/ca.pem" 2>/dev/null
    fi
    san="DNS:$fqdn,DNS:$(hostname -s)"
    for ip in $(hostname -I) 127.0.0.1; do san="$san,IP:$ip"; done
    log "Issuing the DC certificate for $san"
    openssl req -newkey rsa:2048 -nodes -subj "/CN=$fqdn" \
        -keyout "$tls/key.pem" -out "$tls/csr.pem" 2>/dev/null
    openssl x509 -req -in "$tls/csr.pem" -CA "$tls/ca.pem" -CAkey "$tls/ca.key" \
        -CAcreateserial -days 825 -out "$tls/cert.pem" -extfile <(printf '%s\n' \
            "subjectAltName=$san" "extendedKeyUsage=serverAuth" "basicConstraints=critical,CA:FALSE") \
        2>/dev/null
    chmod 0600 "$tls/key.pem" "$tls/ca.key"
    cp "$tls/ca.pem" "$KEYTAB_DIR/ca.pem"
    chmod 0644 "$KEYTAB_DIR/ca.pem"
}

# Start samba once and keep it: samba-tool below talks to the running DC, and a
# test starts using the DC as soon as the setup marks it ready, so the samba that
# serves the setup must be the one that keeps serving. Restarting it after the
# setup would leave a window where the DC is down for a test that already started.
# Samba becomes PID 1 here so a stop signal reaches it, and this script re-runs
# itself in the background to create the test objects. A ready marker left by an
# earlier start of this container is dropped first.
if [ "${1:-}" != --populate ]; then
    rm -f "$KEYTAB_DIR/.ready"
    issue_tls_cert
    "$0" --populate &
    log "Starting samba in foreground..."
    exec samba --foreground --no-process-group
fi

# A failed setup step stops the container, so no test runs against a half-built
# DC. An EXIT trap rather than ERR: ERR misses the explicit exits below and
# failures inside functions.
trap '[ $? -eq 0 ] || { log "ERROR: setup failed, stopping samba"; kill -TERM 1; }' EXIT

# Wait for the LDAP/KDC stack to accept connections.
for _ in $(seq 1 30); do
    if samba-tool processes >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

create_user_if_absent() {
    local user="$1"; shift
    if ! samba-tool user show "$user" >/dev/null 2>&1; then
        log "Creating user $user"
        samba-tool user create "$user" "$USER_PASSWORD" "$@"
    fi
}

# dn_of KIND NAME prints the distinguishedName of a samba-tool user or computer.
dn_of() {
    samba-tool "$1" show "$2" --attributes=distinguishedName 2>/dev/null \
        | sed -n 's/^distinguishedName: //p' | head -n1
}

create_group_if_absent() {
    local group="$1"
    if ! samba-tool group show "$group" >/dev/null 2>&1; then
        log "Creating group $group"
        samba-tool group add "$group"
    fi
}

# --- Groups ---------------------------------------------------------------
create_group_if_absent engineering
create_group_if_absent devs

# Nest devs under engineering: AD resolves the nesting at the DC, so a member of
# devs gets BOTH the devs and engineering SIDs in their ticket's PAC. This is
# the "no LDAP group-walk needed" property AD-1 demonstrates.
samba-tool group addmembers engineering devs >/dev/null 2>&1 || true

# --- Users ----------------------------------------------------------------
# alice: RFC2307 attrs set (idmap_ad path), member of devs (=> nested in engineering).
create_user_if_absent "$USER_ALICE" \
    --uid-number=10001 --gid-number=10000 \
    --unix-home="/home/$USER_ALICE" --login-shell=/bin/bash
samba-tool group addmembers devs "$USER_ALICE" >/dev/null 2>&1 || true

# bob: NO RFC2307 attrs (idmap_rid algorithmic-fallback path), member of engineering.
create_user_if_absent "$USER_BOB"
samba-tool group addmembers engineering "$USER_BOB" >/dev/null 2>&1 || true

# --- Computer accounts for the joins --------------------------------------
# Each is set up once, its password set only then, so a restart leaves it as
# the tests or a join last left it. A marker written after the last step decides
# "done", not the account's existence, so a start that died half-way through
# an account finishes it on the next start.
#
# dittofs$ is the account an installation with the default NetBIOS name would
# join as. It already exists, so an online join of that name must refuse it
# rather than reset the password of whoever holds it; the NETLOGON tests sign
# in as it with its password.
#
# offjoinaes$ and offjoinlegacy$ are pre-staged for an offline join, as an
# administrator creates them and hands their password over: the first with
# msDS-SupportedEncryptionTypes 0x18, which an offline join accepts, the second
# without the attribute, as Active Directory Users and Computers creates one,
# which an offline join refuses.
#
# NOTE (#1345): `samba-tool computer create --computerpassword=...` is NOT a
# valid option on this samba (4.17.x). Create the computer first, then set its
# password via `user setpassword`, which derives its Kerberos keys.
MACHINE_PASSWORD="${MACHINE_PASSWORD:-MachinePass01!}"
OFFJOIN_AES_PASSWORD="${OFFJOIN_AES_PASSWORD:-OffJoinAES01!}"
OFFJOIN_LEGACY_PASSWORD="${OFFJOIN_LEGACY_PASSWORD:-OffJoinLegacy01!}"

# create_computer_if_absent NAME PASSWORD [ENCTYPES]
create_computer_if_absent() {
    local name="$1" password="$2" enctypes="${3:-}" dn
    local marker="/var/lib/samba/.computer-$name"
    [ -f "$marker" ] && return 0
    log "Creating computer account $name\$"
    samba-tool computer show "$name" >/dev/null 2>&1 || samba-tool computer create "$name"
    if [ -n "$enctypes" ]; then
        dn="$(dn_of computer "$name")"
        printf 'dn: %s\nchangetype: modify\nreplace: msDS-SupportedEncryptionTypes\nmsDS-SupportedEncryptionTypes: %s\n' \
            "$dn" "$enctypes" | ldbmodify -H /var/lib/samba/private/sam.ldb
    fi
    samba-tool user setpassword "$name\$" --newpassword="$password"
    touch "$marker"
}
create_computer_if_absent dittofs "$MACHINE_PASSWORD"
create_computer_if_absent offjoinaes "$OFFJOIN_AES_PASSWORD" 24
create_computer_if_absent offjoinlegacy "$OFFJOIN_LEGACY_PASSWORD"

# --- Combined service keytab ----------------------------------------------
# Register BOTH the DittoFS SMB (cifs/) AND NFS (nfs/) service principals on a
# service account of their own and export their keys into ONE keytab. A single
# keytab serving both protocols is what AD-4 exercises: the dittofs server loads
# one keytab and decrypts client AP-REQs for either the SMB or the NFS SPN.
#
# `samba-tool domain exportkeytab <file> --principal=...` APPENDS the principal's
# keys to <file> when it already exists, so two successive invocations into the
# same path produce a combined keytab containing both SPNs. AD-1's PAC test
# consumes the cifs/ entry of this same keytab.
#
# The account is AES only: msDS-SupportedEncryptionTypes 0x18 (AES128 + AES256),
# set before the keytab is exported, so the KDC refuses a ticket for these names
# under RC4 or DES and exportkeytab writes no RC4 key, whose key would be the
# account's NT hash.
keytab="$KEYTAB_DIR/dittofs.keytab"
if [ ! -f "$keytab" ]; then
    log "Registering SPNs on $SERVICE_ACCOUNT and exporting COMBINED keytab to $keytab ($SMB_SPN + $NFS_SPN)"

    if ! samba-tool user show "$SERVICE_ACCOUNT" >/dev/null 2>&1; then
        samba-tool user create "$SERVICE_ACCOUNT" --random-password
    fi
    for spn in "$SMB_SPN" "$NFS_SPN"; do
        samba-tool spn list "$SERVICE_ACCOUNT" | grep -qix "[[:space:]]*$spn" \
            || samba-tool spn add "$spn" "$SERVICE_ACCOUNT"
    done
    svc_dn="$(dn_of user "$SERVICE_ACCOUNT")"
    ldbmodify -H /var/lib/samba/private/sam.ldb <<EOF
dn: $svc_dn
changetype: modify
replace: msDS-SupportedEncryptionTypes
msDS-SupportedEncryptionTypes: 24
EOF

    # Export cifs/ then APPEND nfs/ into the same keytab file.
    samba-tool domain exportkeytab "$keytab" \
        --principal="$SMB_SPN@$REALM"
    samba-tool domain exportkeytab "$keytab" \
        --principal="$NFS_SPN@$REALM"

    # Verify the combined keytab actually carries BOTH principals before we hand
    # it off. klist -k lists keytab entries; fail loudly here rather than letting
    # the Go test discover a half-exported keytab much later.
    if klist -k "$keytab" >/dev/null 2>&1; then
        if ! klist -k "$keytab" 2>/dev/null | grep -qi "cifs/"; then
            log "ERROR: combined keytab missing cifs/ principal"; klist -k "$keytab" || true; exit 1
        fi
        if ! klist -k "$keytab" 2>/dev/null | grep -qi "nfs/"; then
            log "ERROR: combined keytab missing nfs/ principal"; klist -k "$keytab" || true; exit 1
        fi
        # Fast-fail if AES keys are missing (issue #1318). `klist -ke` prints the
        # enctype per entry; require at least one aes256-cts and one aes128-cts so
        # the Windows-11-incompatible RC4-only regression cannot slip through.
        if klist -ke "$keytab" >/dev/null 2>&1; then
            if ! klist -ke "$keytab" 2>/dev/null | grep -qi "aes256-cts"; then
                log "ERROR: combined keytab missing aes256-cts key (Win11 rejects RC4-only)"
                klist -ke "$keytab" || true; exit 1
            fi
            if ! klist -ke "$keytab" 2>/dev/null | grep -qi "aes128-cts"; then
                log "ERROR: combined keytab missing aes128-cts key (Win11 rejects RC4-only)"
                klist -ke "$keytab" || true; exit 1
            fi
            log "Combined keytab verified: carries cifs/ + nfs/ with aes256-cts and aes128-cts keys"
        else
            log "Combined keytab verified: carries both cifs/ and nfs/ principals (enctype check skipped)"
        fi
    else
        log "klist unavailable; skipping in-container keytab verification (Go test re-checks)"
    fi

    # The dittofs container runs as a non-root uid; make the keytab readable there.
    chown "$DITTOFS_UID:$DITTOFS_GID" "$keytab" 2>/dev/null || true
    chmod 0400 "$keytab" 2>/dev/null || true
fi

# Tests wait for this marker, so it is written last: once the keytab is whole
# and the KDC, LDAP, DNS and the RPC endpoint mapper accept connections.
for port in 88 389 53 135; do
    for _ in $(seq 1 60); do
        (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && continue 2
        sleep 1
    done
    log "ERROR: nothing listens on port $port"
    exit 1
done
touch "$KEYTAB_DIR/.ready"

log "AD-DC ready: realm=$REALM users=[$USER_ALICE,$USER_BOB] groups=[engineering,devs(nested)]"
