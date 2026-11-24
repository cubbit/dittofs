# Encryption

## File content is not encrypted

v1's client-side block encryption, the `encryption` block of a remote block
store's config, went with the v1 remote tier. The engine keeps file content in
the journal, in plaintext, and the metadata store keeps names, sizes and
timestamps in plaintext. For at-rest protection, put the journal devices and
`metadata.badger.path` on an encrypted filesystem (LUKS / dm-crypt, FileVault).

## The node's wrapping keys

What the node does encrypt are the secrets it keeps in the metadata store. Each
is sealed under one of two wrapping keys, configured per role under `keys`:

| Role | Seals | Required |
|------|-------|----------|
| `keys.storage` | the administrator's password hash and the API's signing key | by `dfs init` and `dfs start` |
| `keys.protocol` | an account's NT hash, for NTLM | to keep NT hashes: `dfs reset-admin` refuses an account with a uid without it |

A role's key is held either in a local key file or by a KMIP server:

```yaml
keys:
  storage:
    kind: local                          # local | kmip
    local:
      file: /etc/dittofs/keys/storage.key
      retired_files:                     # optional, open-only (see rotation)
        - /etc/dittofs/keys/storage-2026-02.key
  protocol:
    kind: kmip
    kmip:
      endpoint: kms.example.com:5696
      server_ca: /etc/dittofs/kmip/ca.pem
      client_cert: /etc/dittofs/kmip/client.pem
      client_key: /etc/dittofs/kmip/client.key
      key_uid: 12345-abcde-...
      retired_key_uids:                  # optional, open-only
        - 09876-zyxwv-...
      timeout: 5s
```

Only the block `kind` names is read: a key of the other block is refused at load.

### Local key files

A key file is protected by a passphrase, read **only** from the
`DITTOFS_ENCRYPTION_PASSPHRASE` environment variable, one for every key file of
the node. The file's key is derived from it with Argon2id (m = 64 MiB, t = 3,
p = 4).

`dfs init` creates the storage key file: when `keys.storage.local.file` names a
file that does not exist and the metadata store is not yet initialised, it
writes a new master key there, wrapped under the
passphrase, with mode 0600, and prints once where the file is. **Copy the file
off the host and keep the passphrase**: losing either loses every key it wraps,
and with them the data, and nothing recovers it. An existing file is used and
never overwritten. With `DITTOFS_ENCRYPTION_PASSPHRASE` unset, `dfs init`
refuses.

Any other key file, such as the next key of a rotation, is written with the Go
helper:

```go
import "github.com/cubbit/dittofs/pkg/keyprovider"

bytes, _ := keyprovider.GenerateKeyFile("your-strong-passphrase")
os.WriteFile("/etc/dittofs/keys/storage.key", bytes, 0o600)
```

### Rotation

`file` / `key_uid` name the **current** key, which seals everything written from
now on. `retired_files` / `retired_key_uids` name keys that only open what they
sealed. To rotate, move the current reference into the retired list, point
`file` / `key_uid` at the new key, and restart the node.

There is no bulk re-seal, so treat a retired key as keep-forever: dropping it, or
deleting its file, makes every secret it sealed unreadable. A retired key that
cannot be read at start is logged and skipped rather than failing the start; two
keys claiming the same identifier fail it.

### KMIP key states

The KMIP client only reads (`Get` and `GetAttributes`): it cannot create,
activate, revoke or destroy key material, so give it a read-only credential and
provision keys out of band.

The current `key_uid` must be `Active`: any other state, an unreadable one
included, refuses to open the key, naming the state and the uid. If you rotate
at the HSM, move `key_uid` too, or the node will not start on its next restart.
Retired uids are judged more loosely, because what they sealed must stay
readable:

| State of a `retired_key_uids` entry | Behaviour |
|---|---|
| `Active`, `Deactivated`, `Pre-Active` | Loaded normally |
| `Compromised` | Loaded, with a `WARN` |
| `Destroyed` | Not loaded: what it sealed is unreadable |
