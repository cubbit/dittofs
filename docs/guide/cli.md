# CLI Reference

DittoFS ships two binaries:

- **`dfs`** — the server daemon. Runs the protocol adapters and the control-plane API; manages the local config file and the server process.
- **`dfsctl`** — the REST client. Talks to a running `dfs` over its control-plane API to manage users, groups, shares, stores, and adapters.

This page is generated from the command definitions (`go run ./cmd/gendocs`). Do not edit it by hand. Run `dfs <command> --help` or `dfsctl <command> --help` for the same content at the terminal.

## `dfs`

- [`dfs`](#dfs) — DittoFS - Modular virtual filesystem
  - [`dfs completion`](#dfs-completion) — Generate shell completion script
  - [`dfs config`](#dfs-config) — Configuration management
    - [`dfs config edit`](#dfs-config-edit) — Open configuration in editor
    - [`dfs config schema`](#dfs-config-schema) — Generate JSON schema for configuration
    - [`dfs config show`](#dfs-config-show) — Display current configuration
    - [`dfs config validate`](#dfs-config-validate) — Validate configuration file
  - [`dfs init`](#dfs-init) — Initialize a new installation
  - [`dfs reset-admin`](#dfs-reset-admin) — Reset the administrator's credentials
  - [`dfs start`](#dfs-start) — Start the DittoFS server
  - [`dfs version`](#dfs-version) — Show version information


### `dfs`

DittoFS - Modular virtual filesystem

DittoFS is an experimental modular virtual filesystem that decouples
file interfaces from storage backends, with pluggable metadata and content
stores. This build serves no file protocol.

Use "dfs [command] --help" for more information about a command.

Flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs completion`

Generate shell completion script

Generate shell completion script for dfs.

The generated script enables tab-completion for dfs commands, subcommands, and
flags in your shell. Pick the snippet for your shell and load it once.

```
dfs completion [bash|zsh|fish|powershell]
```

**Examples:**

```bash
# Bash (Linux): install system-wide
dfs completion bash > /etc/bash_completion.d/dfs

# Bash (macOS, requires the Homebrew bash-completion package)
dfs completion bash > $(brew --prefix)/etc/bash_completion.d/dfs

# Zsh: enable completion once (if not already enabled), then install
echo "autoload -U compinit; compinit" >> ~/.zshrc
dfs completion zsh > "${fpath[1]}/_dfs"

# Zsh (macOS, Homebrew)
dfs completion zsh > $(brew --prefix)/share/zsh/site-functions/_dfs

# Fish
dfs completion fish > ~/.config/fish/completions/dfs.fish

# PowerShell: load for the current session
dfs completion powershell | Out-String | Invoke-Expression

# PowerShell: persist across sessions by sourcing from your profile
dfs completion powershell > dfs.ps1
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config`

Configuration management

Manage the DittoFS server's configuration file.

Subcommands:

```
edit      Open configuration in editor
validate  Validate configuration file
show      Display current configuration
schema    Generate JSON schema for IDE/validation
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config edit`

Open configuration in editor

Open the configuration file in your default editor.

The file is --config, else the one DITTOFS_CONFIG names, else the default.
Uses the EDITOR environment variable, then VISUAL, falling back to 'vi'. The
file is not checked: run 'dfs config validate' after editing it.

```
dfs config edit [flags]
```

**Examples:**

```bash
# Edit default config
dfs config edit

# Edit specific config file
dfs config edit --config /etc/dittofs/config.yaml
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config schema`

Generate JSON schema for configuration

Generate a JSON schema for the DittoFS configuration file.

The schema covers each key's type, its allowed values and the keys required
whatever another key holds: journals with a path in each entry, and
keys.<role>.kind for both roles. It checks the file alone: a file whose
journals or keys come from DITTOFS_ variables fails its required keys, and
the rules that relate two keys are checked by 'dfs config validate'.

The schema can be used for:

```
- IDE autocompletion (VS Code, IntelliJ, etc.)
- Configuration file validation
```

```
dfs config schema [flags]
```

**Examples:**

```bash
# Print schema to stdout
dfs config schema

# Save schema to file
dfs config schema --output config.schema.json
```

Flags:

```
  -o, --output string   Output file (default: stdout)
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config show`

Display current configuration

Display the bootstrap a start would read, and where each value came from.

Prints the file read (empty when none was), the state and runtime directories
with the variable that chose each, or "default" when none did, the socket
path, the bootstrap's keys, and a sources map naming each key's source:
"file", the DITTOFS_ variable that set it, or "default". A value set by a
variable prints as the variable's name, never as its value, and the socket's
path is then empty. A credential prints as its file's path. The output is a
report, not a file a start can read.

The file is --config, else the one DITTOFS_CONFIG names, else the default.
show checks only what a load checks: run 'dfs config validate' for the rest.

```
dfs config show [flags]
```

**Examples:**

```bash
# Show default config as YAML
dfs config show

# Show as JSON
dfs config show --output json

# Show specific config file
dfs config show --config /etc/dittofs/config.yaml
```

Flags:

```
  -o, --output string   Output format (yaml|json) (default "yaml")
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs config validate`

Validate configuration file

Validate the DittoFS configuration file and environment.

Runs the load a start runs, then the checks of the host: both roles, the
journals, the two wrapping keys (each key file is opened with
DITTOFS_ENCRYPTION_PASSPHRASE), the API's certificate off loopback, the socket
path and the state and runtime directories. It refuses what they refuse, one
line per refusal, and starts nothing: no directory is created.

The file is --config, else the one DITTOFS_CONFIG names, else the default.

```
dfs config validate [flags]
```

**Examples:**

```bash
# Validate default config
dfs config validate

# Validate specific config file
dfs config validate --config /etc/dittofs/config.yaml
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs init`

Initialize a new installation

Initialize a new DittoFS installation: its metadata store, each journal
directory with the node's identity beside it, the administrator account
"admin" and the signing key, in the stores the server's configuration names.

The administrator's first password is read from a file or an environment
variable. With neither, a password is generated and printed once to standard
output, never to the log, and must be changed at the first sign-in.

When keys.storage.local.file names a file that does not exist and the
metadata store is not yet initialised, init creates it, a master key
wrapped under DITTOFS_ENCRYPTION_PASSPHRASE, and prints once where it is:
copy it off the host, since losing it or the passphrase loses the data. An
existing file is used, never overwritten. With the passphrase unset, init
refuses.

Run again after an interruption, init finishes what the interrupted run left;
over a finished installation it refuses and changes nothing.

```
dfs init [flags]
```

**Examples:**

```bash
# Initialize, generating the administrator's password
dfs init

# Initialize with the password in a file (one trailing newline is dropped)
dfs init --admin-password-file /run/secrets/dittofs-admin

# Initialize with the password in an environment variable
DFS_ADMIN_PASSWORD=... dfs init --admin-password-env DFS_ADMIN_PASSWORD
```

Flags:

```
      --admin-password-env string    environment variable holding the administrator's first password
      --admin-password-file string   file holding the administrator's first password, at most 4 KiB
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs reset-admin`

Reset the administrator's credentials

Reset the credentials of an administrator, on the host that holds the
installation's metadata store, for when no administrator can sign in.

The node must be stopped: while a node holds the store, the reset refuses and
changes nothing. It resets an existing, enabled account with role admin, and
refuses any other. It sets a new generated password, printed once to
standard output and never to the log, that must be changed at the first
sign-in; refuses every credential issued before it; clears the account's
sign-in failure count and every source's; and records the reset in the audit,
naming this host.

An account with a uid gets an NT hash of the new password while NTLM is on,
and an NT hash of the old password is overwritten otherwise. Both are sealed
under keys.protocol: the reset refuses such an account when the bootstrap
does not set it.

```
dfs reset-admin [flags]
```

**Examples:**

```bash
# Reset the administrator "admin"
dfs reset-admin

# Reset another administrator
dfs reset-admin --name ops
```

Flags:

```
      --name string   the administrator account to reset (default "admin")
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs start`

Start the DittoFS server

Start the DittoFS server in the foreground with the specified configuration.

The server runs until it receives SIGINT or SIGTERM, then stops and exits
zero once every part stopped. A start that fails for its own reason exits
non-zero, even when a signal arrived while it failed. Run it under a
process supervisor (systemd, Docker) to keep it in the background.

```
dfs start [flags]
```

**Examples:**

```bash
# Start with the default configuration
dfs start

# Start with debug logging
DITTOFS_LOGGING_LEVEL=debug dfs start

# Start with a custom config file
dfs start --config /etc/dittofs/config.yaml
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

### `dfs version`

Show version information

Display the DittoFS server build version and system information.

Shows the full semantic version, git commit, build date, Go toolchain version,
and OS/architecture. Use --short to emit only the version string for scripting.

```
dfs version [flags]
```

**Examples:**

```bash
# Show full version information
dfs version

# Print only the version number (useful in scripts)
dfs version --short
```

Flags:

```
      --short   Show only version number
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

## `dfsctl`

- [`dfsctl`](#dfsctl) — DittoFS Control - Remote management client
  - [`dfsctl completion`](#dfsctl-completion) — Generate shell completion script
  - [`dfsctl context`](#dfsctl-context) — Manage server contexts
    - [`dfsctl context current`](#dfsctl-context-current) — Show current context
    - [`dfsctl context list`](#dfsctl-context-list) — List all configured contexts
    - [`dfsctl context remove`](#dfsctl-context-remove) — Remove a context
    - [`dfsctl context rename`](#dfsctl-context-rename) — Rename a context
    - [`dfsctl context use`](#dfsctl-context-use) — Switch to a different context
  - [`dfsctl group`](#dfsctl-group) — Manage groups
    - [`dfsctl group add-member`](#dfsctl-group-add-member) — Add a user to a group
    - [`dfsctl group create`](#dfsctl-group-create) — Create a group
    - [`dfsctl group delete`](#dfsctl-group-delete) — Delete a group
    - [`dfsctl group get`](#dfsctl-group-get) — Show a group
    - [`dfsctl group list`](#dfsctl-group-list) — List groups
    - [`dfsctl group remove-member`](#dfsctl-group-remove-member) — Remove a user from a group
    - [`dfsctl group update`](#dfsctl-group-update) — Change a group
  - [`dfsctl installation`](#dfsctl-installation) — Act on the installation as a whole
    - [`dfsctl installation rotate-signing-key`](#dfsctl-installation-rotate-signing-key) — Rotate the key access tokens are signed with
  - [`dfsctl login`](#dfsctl-login) — Authenticate with DittoFS server
  - [`dfsctl logout`](#dfsctl-logout) — Clear stored credentials
  - [`dfsctl netgroup`](#dfsctl-netgroup) — Manage netgroups
    - [`dfsctl netgroup create`](#dfsctl-netgroup-create) — Create a netgroup
    - [`dfsctl netgroup delete`](#dfsctl-netgroup-delete) — Delete a netgroup
    - [`dfsctl netgroup get`](#dfsctl-netgroup-get) — Show a netgroup
    - [`dfsctl netgroup list`](#dfsctl-netgroup-list) — List netgroups
    - [`dfsctl netgroup update`](#dfsctl-netgroup-update) — Replace a netgroup's members
  - [`dfsctl operation`](#dfsctl-operation) — Follow long-running operations
    - [`dfsctl operation cancel`](#dfsctl-operation-cancel) — Cancel an operation
    - [`dfsctl operation get`](#dfsctl-operation-get) — Show an operation
    - [`dfsctl operation list`](#dfsctl-operation-list) — List operations
    - [`dfsctl operation wait`](#dfsctl-operation-wait) — Wait for an operation to end
  - [`dfsctl password`](#dfsctl-password) — Manage your own password
    - [`dfsctl password change`](#dfsctl-password-change) — Change the signed-in account's password
  - [`dfsctl quota`](#dfsctl-quota) — Per-identity quota management
    - [`dfsctl quota list`](#dfsctl-quota-list) — List all quotas on a share
    - [`dfsctl quota remove`](#dfsctl-quota-remove) — Remove a per-identity quota
    - [`dfsctl quota set`](#dfsctl-quota-set) — Create or update a per-identity quota
  - [`dfsctl share`](#dfsctl-share) — Share management
    - [`dfsctl share create`](#dfsctl-share-create) — Create a new share
    - [`dfsctl share disable`](#dfsctl-share-disable) — Disable a share (drain clients, block new connections)
    - [`dfsctl share edit`](#dfsctl-share-edit) — Edit a share
    - [`dfsctl share enable`](#dfsctl-share-enable) — Enable a share (accept new connections)
    - [`dfsctl share list`](#dfsctl-share-list) — List all shares
    - [`dfsctl share mount`](#dfsctl-share-mount) — Mount a share via NFS or SMB
    - [`dfsctl share nfs-config`](#dfsctl-share-nfs-config) — Manage per-share NFS adapter configuration
      - [`dfsctl share nfs-config set`](#dfsctl-share-nfs-config-set) — Update a share's NFS adapter configuration
      - [`dfsctl share nfs-config show`](#dfsctl-share-nfs-config-show) — Show a share's NFS adapter configuration
    - [`dfsctl share permission`](#dfsctl-share-permission) — Manage share permissions
      - [`dfsctl share permission grant`](#dfsctl-share-permission-grant) — Grant permission on a share
      - [`dfsctl share permission list`](#dfsctl-share-permission-list) — List permissions on a share
      - [`dfsctl share permission revoke`](#dfsctl-share-permission-revoke) — Revoke permission from a share
    - [`dfsctl share remove`](#dfsctl-share-remove) — Remove a share
    - [`dfsctl share show`](#dfsctl-share-show) — Show share details
  - [`dfsctl status`](#dfsctl-status) — Show server status
  - [`dfsctl token`](#dfsctl-token) — Manage API tokens
    - [`dfsctl token create`](#dfsctl-token-create) — Create an API token
    - [`dfsctl token list`](#dfsctl-token-list) — List API tokens
    - [`dfsctl token revoke`](#dfsctl-token-revoke) — Revoke an API token
  - [`dfsctl user`](#dfsctl-user) — Manage users
    - [`dfsctl user create`](#dfsctl-user-create) — Create a user
    - [`dfsctl user delete`](#dfsctl-user-delete) — Delete a user
    - [`dfsctl user get`](#dfsctl-user-get) — Show a user
    - [`dfsctl user list`](#dfsctl-user-list) — List users
    - [`dfsctl user set-password`](#dfsctl-user-set-password) — Set a user's password
    - [`dfsctl user update`](#dfsctl-user-update) — Change a user
  - [`dfsctl version`](#dfsctl-version) — Show version information


### `dfsctl`

DittoFS Control - Remote management client

dfsctl is the command-line client for managing DittoFS servers remotely.

Its commands for shares and quotas return with the server's v2 API; until
then each refuses. The context subcommand manages the server
contexts kept in ~/.config/dfsctl/config.json.

Use "dfsctl [command] --help" for more information about a command.

Flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl completion`

Generate shell completion script

Generate shell completion script for dfsctl.

The generated script enables tab-completion for dfsctl commands, subcommands,
and flags in your shell. Pick the snippet for your shell and load it once.

```
dfsctl completion [bash|zsh|fish|powershell]
```

**Examples:**

```bash
# Bash (Linux): install system-wide
dfsctl completion bash > /etc/bash_completion.d/dfsctl

# Bash (macOS, requires the Homebrew bash-completion package)
dfsctl completion bash > $(brew --prefix)/etc/bash_completion.d/dfsctl

# Zsh: enable completion once (if not already enabled), then install
echo "autoload -U compinit; compinit" >> ~/.zshrc
dfsctl completion zsh > "${fpath[1]}/_dfsctl"

# Zsh (macOS, Homebrew)
dfsctl completion zsh > $(brew --prefix)/share/zsh/site-functions/_dfsctl

# Fish
dfsctl completion fish > ~/.config/fish/completions/dfsctl.fish

# PowerShell: load for the current session
dfsctl completion powershell | Out-String | Invoke-Expression

# PowerShell: persist across sessions by sourcing from your profile
dfsctl completion powershell > dfsctl.ps1
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl context`

Manage server contexts

Manage named connection contexts for one or more DittoFS servers.

Each context stores a server URL, authentication credentials, and a display name. Contexts work similarly to kubectl contexts: log in once per server, then switch between them with 'context use'. All subsequent dfsctl commands use the active context automatically.

```
dfsctl context [flags]
```

**Examples:**

```bash
# List all saved contexts
dfsctl context list

# Switch to a context named "production"
dfsctl context use production

# Show which context is currently active
dfsctl context current

# Remove a context that is no longer needed
dfsctl context remove staging
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl context current`

Show current context

Display the name and connection details of the currently active context.

The output shows the server URL, logged-in user, and authentication status. Use this command to confirm which server subsequent dfsctl commands will target before running destructive operations.

```
dfsctl context current [flags]
```

**Examples:**

```bash
# Show the active context as a human-readable summary
dfsctl context current

# Get the active context as JSON for scripting
dfsctl context current --output json
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl context list`

List all configured contexts

List all configured server contexts stored in the local credential file.

Each row shows the context name, server URL, username, and login status. The active context is marked with an asterisk (*). Use this to identify which servers you have credentials for and which context is currently selected.

```
dfsctl context list [flags]
```

**Examples:**

```bash
# List all contexts as a table
dfsctl context list

# List contexts as JSON for scripting
dfsctl context list -o json
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl context remove`

Remove a context

Remove a saved server context and its stored credentials.

The context's configuration and access token are removed from the local credential store. Use this to clean up after decommissioning a server or when a context was created by mistake.

```
dfsctl context remove <name> [flags]
```

**Examples:**

```bash
# Remove the "staging" context with a confirmation prompt
dfsctl context remove staging

# Remove without the confirmation prompt (e.g. in a script)
dfsctl context remove staging --force
```

Flags:

```
  -f, --force   Skip confirmation
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl context rename`

Rename a context

Rename an existing server context to a new name.

All stored credentials and the active-context pointer are updated atomically; no re-authentication is needed. Use this after promoting a staging server to production, or simply to give a context a more descriptive name.

```
dfsctl context rename <old-name> <new-name> [flags]
```

**Examples:**

```bash
# Rename the "default" context to "production"
dfsctl context rename default production

# Rename a development context to something more descriptive
dfsctl context rename dev local-dev
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl context use`

Switch to a different context

Switch the active context so that subsequent dfsctl commands target a different server.

The new active context is saved to the local credential file. Run 'context current' afterwards to confirm the switch, or 'context list' to see all available context names.

```
dfsctl context use <name> [flags]
```

**Examples:**

```bash
# Switch to the "production" context
dfsctl context use production

# Switch to a local development server context
dfsctl context use local-dev
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group`

Manage groups

Create, list, read, change and delete the server's groups, and add and remove
their members.

Each command names a group by its name, or by its ID with --id; add-member
and remove-member name the group and the user by their names.

```
dfsctl group [flags]
```

**Examples:**

```bash
# Create a group with gid 1001
dfsctl group create editors --gid 1001

# Add alice to it
dfsctl group add-member editors alice

# Delete it and wait for the deletion to end
dfsctl group delete editors --force --wait
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group add-member`

Add a user to a group

```
dfsctl group add-member <group> <user> [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group create`

Create a group

```
dfsctl group create <name> [flags]
```

Flags:

```
      --gid uint32   Unix group ID, which a create leaving it out allocates
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group delete`

Delete a group

Delete a group. The deletion is an operation: the command prints its ID, or,
with --wait, follows it to its end.

```
dfsctl group delete <name> [flags]
```

Flags:

```
  -f, --force       Delete without asking
      --id string   The group's ID, in place of its name
      --wait        Follow the operation the command starts to its end
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group get`

Show a group

```
dfsctl group get <name> [flags]
```

Flags:

```
      --id string   The group's ID, in place of its name
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group list`

List groups

```
dfsctl group list [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group remove-member`

Remove a user from a group

```
dfsctl group remove-member <group> <user> [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group update`

Change a group

Change the fields of a group that the flags name: its name and its gid.

```
dfsctl group update <name> [flags]
```

Flags:

```
      --gid uint32    Unix group ID
      --id string     The group's ID, in place of its name
      --name string   New name
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl installation`

Act on the installation as a whole

```
dfsctl installation [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl installation rotate-signing-key`

Rotate the key access tokens are signed with

Rotate the installation's signing key, the key every node signs and verifies
access tokens with. The rotation is an operation: a new key signs from its
start, and the old one verifies the tokens it signed until they have expired,
about 15 minutes, when the operation ends. The command prints the operation's
ID, or, with --wait, follows it to its end.

```
dfsctl installation rotate-signing-key [flags]
```

Flags:

```
      --wait   Follow the operation the command starts to its end
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl login`

Authenticate with DittoFS server

Sign in to a DittoFS server and keep the session in a named context.

The server is an https URL, an http URL on a loopback address, or the path of
the node's local socket. The password is read from a prompt, or from the file
--password-file names; --token-file signs in with an API token instead. The
context keeps the server, the account, the CA bundle --cacert names and the
session's tokens, in a file only its owner can read; later commands refresh
the session themselves. Without --server, --cacert or --username, login reuses
the context's.

```
dfsctl login [flags]
```

**Examples:**

```bash
# Sign in to a node through its local socket (prompts for the password)
dfsctl login --server /run/dittofs/api.sock --username admin

# Sign in to a remote server whose certificate a private CA signs
dfsctl login --server https://dfs.example.com --username admin --cacert ca.pem

# Sign in from a script
dfsctl login --server https://dfs.example.com -u admin --password-file pw.txt

# Use an API token in a context of its own
dfsctl login --server https://dfs.example.com --token-file token.txt -c ci

# Sign in again to the current context (password prompt only)
dfsctl login
```

Flags:

```
      --cacert string          Path to a PEM CA bundle trusted for the server certificate
  -c, --context string         Context name (defaults to the current one, or "default")
      --password-file string   File holding the password (prompts without it)
      --server string          Server URL or local socket path (defaults to the context's)
      --token-file string      File holding an API token to sign in with instead of a password
  -u, --username string        Username
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl logout`

Clear stored credentials

End the current context's session and clear its tokens.

The server is asked to end the session, then the tokens are removed from the
context; if the server cannot be reached the context keeps them, the only
handle on the live session, and logout fails so it can be run again. A
context signed out keeps its server, account and CA bundle so dfsctl login
signs in again with only a password. A context signed in with an API token only forgets
the token, which stays valid until it is revoked. To switch between contexts or
remove one entirely, use the context subcommand.

```
dfsctl logout [flags]
```

**Examples:**

```bash
# Logout from the current context (clears tokens, keeps server URL)
dfsctl logout

# Logout then immediately log back in as a different user
dfsctl logout && dfsctl login --username operator
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup`

Manage netgroups

Create, list, read, change and delete netgroups: named sets of host names,
addresses and CIDRs that an export policy admits.

An update replaces the netgroup's members with those --member names.

```
dfsctl netgroup [flags]
```

**Examples:**

```bash
# Create a netgroup of two hosts and a subnet
dfsctl netgroup create builders --member build1.example.com --member 10.0.0.7 --member 10.1.0.0/16

# Replace its members
dfsctl netgroup update builders --member 10.1.0.0/16
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup create`

Create a netgroup

```
dfsctl netgroup create <name> [flags]
```

Flags:

```
      --member stringArray   A host name, address or CIDR; repeat for each
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup delete`

Delete a netgroup

```
dfsctl netgroup delete <name> [flags]
```

Flags:

```
  -f, --force   Delete without asking
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup get`

Show a netgroup

```
dfsctl netgroup get <name> [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup list`

List netgroups

```
dfsctl netgroup list [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl netgroup update`

Replace a netgroup's members

Replace a netgroup's members with those --member names; with no --member it is left empty.

```
dfsctl netgroup update <name> [flags]
```

Flags:

```
      --member stringArray   A host name, address or CIDR; repeat for each
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl operation`

Follow long-running operations

Read, list, wait for and cancel operations.

A command whose work outlasts its call, such as a deletion, starts an operation
and prints its ID; with --wait it follows the operation to its end. A wait that
is interrupted leaves the operation running, and 'dfsctl operation wait <id>'
resumes it, from this machine or any other signed in to the same server.

```
dfsctl operation [flags]
```

**Examples:**

```bash
# List the operations running
dfsctl operation list --state running

# Wait for an operation to end
dfsctl operation wait 1a92dcf1-bdbb-49eb-acc1-a78ef6b4e5f1
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl operation cancel`

Cancel an operation

```
dfsctl operation cancel <id> [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl operation get`

Show an operation

```
dfsctl operation get <id> [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl operation list`

List operations

```
dfsctl operation list [flags]
```

Flags:

```
      --state string   Only the operations not ended in this state (pending|running|cancelling)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl operation wait`

Wait for an operation to end

Wait for an operation to end and print it as it ended.

The command exits non-zero when the operation failed or was cancelled, and
when the wait itself is interrupted, which leaves the operation running.

```
dfsctl operation wait <id> [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl password`

Manage your own password

```
dfsctl password [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl password change`

Change the signed-in account's password

Change the password of the account the current context is signed in as.

The current password and the new one are read from prompts, the new one twice,
or from the files --current-password-file and --new-password-file name. The
change ends every other session of the account; the context keeps the new
session the server answers. An account whose password must change can make
this call, and sign out, and nothing else until it does.

```
dfsctl password change [flags]
```

**Examples:**

```bash
# Change the password interactively
dfsctl password change

# Change it from a script
dfsctl password change --current-password-file old.txt --new-password-file new.txt
```

Flags:

```
      --current-password-file string   File holding the current password (prompts without it)
      --new-password-file string       File holding the new password (prompts twice without it)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl quota`

Per-identity quota management

Manage per-identity (user/group/default-user) storage quotas on a share.

Quotas bound both bytes and inode (file) count, with optional soft thresholds
and a grace period before a soft threshold is enforced as hard. These operations
require admin privileges.

```
dfsctl quota [flags]
```

**Examples:**

```bash
# List all quotas on a share
dfsctl quota list /archive

# Set a per-user quota (uid 1000)
dfsctl quota set /archive --scope user --id 1000 --limit-bytes 10GiB --limit-files 100000

# Set the default-user fallback quota (applies to users without an explicit quota)
dfsctl quota set /archive --scope default-user --limit-bytes 1GiB

# Set a per-group quota with soft thresholds and a grace period
dfsctl quota set /archive --scope group --id 2000 --limit-bytes 50GiB --soft-bytes 45GiB --grace-seconds 604800

# Remove a per-user quota
dfsctl quota remove /archive --scope user --id 1000
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl quota list`

List all quotas on a share

List all per-identity quotas configured on a share.

Outputs one row per quota entry, showing the scope (user/group/default-user),
identity ID, hard and soft byte/file limits, current usage, and the grace
period. Use this to audit who has explicit limits before adjusting or removing
them.

```
dfsctl quota list <share> [flags]
```

**Examples:**

```bash
# List quotas as a table
dfsctl quota list /archive

# List as JSON
dfsctl quota list /archive -o json

# List as YAML
dfsctl quota list /archive -o yaml
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl quota remove`

Remove a per-identity quota

Remove a per-identity quota from a share.

Once removed, the identity reverts to the default-user fallback quota (if
one exists) or becomes unlimited. The operation is irreversible and requires
confirmation unless --force is specified.

```
dfsctl quota remove <share> [flags]
```

**Examples:**

```bash
# Remove a per-user quota (uid 1000)
dfsctl quota remove /archive --scope user --id 1000

# Remove the default-user fallback quota
dfsctl quota remove /archive --scope default-user

# Remove a per-group quota (gid 2000) without prompting
dfsctl quota remove /archive --scope group --id 2000 --force
```

Flags:

```
  -f, --force          Skip confirmation prompt
      --id int         Identity id (uid for user, gid for group). Required for user/group; omit for default-user. (default -1)
      --scope string   Quota scope (user|group|default-user) (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl quota set`

Create or update a per-identity quota

Create or update a per-identity quota on a share.

The --scope flag selects user, group, or default-user. For user/group scopes an
identity --id (uid or gid) is required. The default-user scope is a fallback
applied to any user without an explicit user quota and takes no --id.

A byte or file limit of 0 (the default) means "no limit on that dimension".

```
dfsctl quota set <share> [flags]
```

**Examples:**

```bash
# Per-user quota (uid 1000): 10GiB / 100k files
dfsctl quota set /archive --scope user --id 1000 --limit-bytes 10GiB --limit-files 100000

# Default-user fallback quota
dfsctl quota set /archive --scope default-user --limit-bytes 1GiB

# Per-group quota with soft thresholds and a 7-day grace period
dfsctl quota set /archive --scope group --id 2000 --limit-bytes 50GiB --soft-bytes 45GiB --grace-seconds 604800
```

Flags:

```
      --grace-seconds int    Seconds usage may exceed a soft threshold before it is enforced as hard. 0 = grace disabled.
      --id int               Identity id (uid for user, gid for group). Required for user/group; omit for default-user. (default -1)
      --limit-bytes string   Hard byte ceiling (e.g., '10GiB', '500MiB'). 0/empty = unlimited.
      --limit-files int      Hard inode (file-count) ceiling. 0 = unlimited.
      --scope string         Quota scope (user|group|default-user) (required)
      --soft-bytes string    Soft byte threshold (e.g., '8GiB'). 0/empty = none.
      --soft-files int       Soft inode threshold. 0 = none.
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share`

Share management

Manage shares on the DittoFS server.

The `share` tree uses `share <verb> <name>` — Cobra
resolves the verb as the subcommand and `<name>` as its positional
argument. These operations require admin privileges.

```
dfsctl share [flags]
```

**Examples:**

```bash
# List all shares
dfsctl share list

# Create a new share
dfsctl share create --name /archive --metadata default --block-store s3-store

# Show share details
dfsctl share show /archive

# Edit a share interactively
dfsctl share edit /archive

# Edit a share with flags
dfsctl share edit /archive --read-only true

# Disable a share (drain clients, block new connections)
dfsctl share disable /archive

# Re-enable a share
dfsctl share enable /archive

# Remove a share
dfsctl share remove /archive

# Grant permission
dfsctl share permission grant /archive --user alice --level read-write
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share create`

Create a new share

Create a new share on the DittoFS server.

A share requires a metadata store and a block store. The block store holds the
durable copy of the data; each share also keeps a local journal on disk.

```
dfsctl share create [flags]
```

**Examples:**

```bash
# Create a share
dfsctl share create --name /data --metadata default --block-store s3-store

# Create a read-only share
dfsctl share create --name /readonly --metadata default --block-store s3-store --read-only

# Create with default permission allowing all users read-write access
dfsctl share create --name /shared --metadata default --block-store s3-store --default-permission read-write

# Create with description
dfsctl share create --name /docs --metadata default --block-store s3-store --description "Documentation files"

# Create a pinned share (blocks never evicted)
dfsctl share create --name /edge-data --metadata default --block-store s3-store --retention pin

# Create with TTL retention (evict after 72 hours of no access)
dfsctl share create --name /logs --metadata default --block-store s3-store --retention ttl --retention-ttl 72h

# Create with per-share cache size overrides
dfsctl share create --name /bigdata --metadata default --block-store s3-store --journal-size 10GiB --read-buffer-size 2GiB

# Create with per-share quota
dfsctl share create --name /limited --metadata default --block-store s3-store --quota-bytes 10GiB

# Create an export that does not squash root (e.g. for root-mounted/benchmark clients)
dfsctl share create --name /export --metadata default --block-store s3-store --squash none
```

Flags:

```
      --access-based-enumeration     Enable Windows access-based enumeration (SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM). When true, SMB clients only see directory entries they can read.
      --acl-canonicalize-inherited   When false, preserves the SE_DACL_AUTO_INHERITED control bit verbatim on SET_INFO Security instead of applying MS-DTYP §2.5.3.4.2 canonicalization (Samba "acl flag inherited canonicalization = no"). Default true matches Windows. (default true)
      --allow-mfsymlink              Convert 1067-byte XSym (Minshall+French) symlink files written by macOS/Windows SMB clients into real symlinks on CLOSE. Off by default (XSym files are stored as regular files).
      --block-store string           Block store name (required)
      --change-notify-disabled       Reject SMB2 CHANGE_NOTIFY with STATUS_NOT_IMPLEMENTED on this share (mirrors Samba 'kernel change notify = no').
      --commit-ack string            What a COMMIT waits for: journal (survives host crash) or block-store (survives device loss)
      --continuous-availability      Advertise SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY and allow SMB3 persistent durable handles on this share.
      --default-permission string    Default permission for unmapped UIDs (none|read|read-write|admin) (default "none")
      --description string           Share description
      --encrypt-data                 Require SMB3 encryption for this share
      --journal-size string          Per-share journal size override (e.g., 10GiB, 500MiB)
      --metadata string              Metadata store name (required)
      --name string                  Share name/path (required)
      --owner string                 Username that owns the share's root directory (defaults to root). The owner can write at the share root; other principals are governed by POSIX mode plus their share permission grant.
      --quota-bytes string           Per-share byte quota (e.g., '10GiB', '500MiB'). 0 = unlimited (default)
      --read-buffer-size string      Per-share read buffer size override (e.g., 2GiB, 256MiB)
      --read-only                    Make share read-only
      --relaxed-metadata-commit      Let an operation that promised stable metadata return before the metadata fsync
      --retention string             Retention policy (pin|ttl|lru)
      --retention-ttl string         Retention TTL duration (e.g., 72h, 24h)
      --squash string                Squash mode, for NFS and SMB (none|root_to_admin|root_to_guest|all_to_admin|all_to_guest). Default root_to_guest (root_squash); use none or root_to_admin so a UID 0 caller (a root-mounted NFS client, the built-in SMB admin) is not squashed to guest.
      --streams-disabled             Reject SMB2 Alternate Data Stream opens with STATUS_OBJECT_NAME_INVALID on this share (mirrors Samba 'smbd:streams = no').
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share disable`

Disable a share (drain clients, block new connections)

Disable a share on the DittoFS server.

Disabling a share drains connected clients synchronously (NFS MOUNT / NFSv4
PUTFH / SMB TREE_CONNECT are refused for disabled shares) and blocks new
connections until the share is re-enabled. This is the safety gate that
must precede a metadata-store restore.

The command blocks until the drain completes (or the server's
lifecycle shutdown timeout fires). Exit code is 0 when the share has been
marked disabled and all in-flight clients have been notified.

```
dfsctl share disable <name> [flags]
```

**Examples:**

```bash
# Disable a share before restoring its metadata store
dfsctl share disable /archive

# Emit the updated Share record as JSON
dfsctl share disable /archive -o json
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share edit`

Edit a share

Edit an existing share on the DittoFS server.

When run without flags, opens an interactive editor to modify share properties.
When flags are provided, only the specified fields are updated.

```
dfsctl share edit <name> [flags]
```

**Examples:**

```bash
# Edit share interactively
dfsctl share edit /archive

# Update the block store reference
dfsctl share edit /archive --block-store new-s3-store

# Make share read-only
dfsctl share edit /archive --read-only true

# Make share writable
dfsctl share edit /archive --read-only false

# Set default permission to allow all users read-write access
dfsctl share edit /archive --default-permission read-write

# Update description
dfsctl share edit /archive --description "New description"

# Change retention policy to pin (blocks never evicted)
dfsctl share edit /archive --retention pin

# Change retention policy to TTL with 72-hour window
dfsctl share edit /archive --retention ttl --retention-ttl 72h

# Override the per-share journal size
dfsctl share edit /archive --journal-size 10GiB

# Override per-share read buffer size
dfsctl share edit /archive --read-buffer-size 2GiB

# Set per-share quota
dfsctl share edit /archive --quota-bytes 10GiB

# Remove quota (set to unlimited)
dfsctl share edit /archive --quota-bytes 0
```

Flags:

```
      --access-based-enumeration string     Enable/disable Windows access-based enumeration (true|false). Takes effect on adapter restart.
      --acl-canonicalize-inherited string   When false, preserves the SE_DACL_AUTO_INHERITED control bit verbatim on SET_INFO Security instead of applying MS-DTYP §2.5.3.4.2 canonicalization (Samba "acl flag inherited canonicalization = no"). Default true matches Windows. Takes effect on adapter restart.
      --block-store string                  Block store name
      --commit-ack string                   What a COMMIT waits for: journal (survives host crash) or block-store (survives device loss)
      --default-permission string           Default permission (none|read|read-write|admin)
      --description string                  Share description
      --encrypt-data string                 Require SMB3 encryption (true|false)
      --journal-size string                 Per-share journal size override (e.g., 10GiB, 500MiB)
      --quota-bytes string                  Per-share byte quota (e.g., '10GiB'). 0 = remove quota
      --read-buffer-size string             Per-share read buffer size override (e.g., 2GiB, 256MiB)
      --read-only string                    Set read-only (true|false)
      --relaxed-metadata-commit             Let an operation that promised stable metadata return before the metadata fsync
      --retention string                    Retention policy (pin|ttl|lru)
      --retention-ttl string                Retention TTL duration (e.g., 72h)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share enable`

Enable a share (accept new connections)

Enable a share on the DittoFS server.

Re-enabling a share allows new client connections and lifts the drain state
set by 'share disable'. Re-enabling is a deliberate operator act; no
mid-restore safety check is performed — the operator owns the timing.

```
dfsctl share enable <name> [flags]
```

**Examples:**

```bash
# Enable a share after a completed metadata-store restore
dfsctl share enable /archive

# Emit the updated Share record as JSON
dfsctl share enable /archive -o json
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share list`

List all shares

List all shares configured on the DittoFS server.

Each row shows the share's name, the metadata and block stores it uses, its
storage quota and current usage, the default permission level, the block
retention policy, and whether the share is currently enabled. Use this command
to get a quick overview of all shares before running share-specific commands.

USED is the share's logical size: the sum of its file sizes, the same figure
'du --apparent-size' would report over the mounted share. It is what QUOTA is
measured against. It is not a count of bytes on disk, and will not match one:
blocks are deduplicated and compressed.

```
dfsctl share list [flags]
```

**Examples:**

```bash
# List all shares as a table
dfsctl share list

# List shares and pipe to grep to find disabled ones
dfsctl share list | grep " -$"

# Output the full share list as JSON for scripting
dfsctl share list -o json

# Output as YAML
dfsctl share list -o yaml
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share mount`

Mount a share via NFS or SMB

Mount a DittoFS share at a local mount point using NFS or SMB protocol.

For SMB mounts, credentials are resolved in order:

1. --username/--password flags
2. DITTOFS_PASSWORD environment variable (for password)
3. Current login context username
4. Interactive password prompt

Mount commands typically require sudo/root privileges on Unix systems.

Platform differences for SMB with sudo:

- Linux: mount owner set to your user via uid/gid options (default mode 0755)
- macOS: mount owned by root (uid/gid removed in Catalina), default mode 0777
- macOS alternative: mount to ~/mnt without sudo for a user-owned mount
- Windows: uses 'net use' to map network drives (e.g. dfsctl share mount /export --protocol smb Z:)

```
dfsctl share mount [share] [mountpoint] [flags]
```

**Examples:**

```bash
# Mount via NFS
dfsctl share mount /export --protocol nfs /mnt/dittofs

# Mount via SMB
dfsctl share mount /export --protocol smb /mnt/dittofs

# Mount via SMB with explicit credentials
dfsctl share mount /export --protocol smb --username alice /mnt/dittofs

# Mount via SMB with password from environment
DITTOFS_PASSWORD=secret dfsctl share mount /export --protocol smb /mnt/dittofs

# Mount to user directory without sudo (macOS only, recommended)
mkdir -p ~/mnt/dittofs && dfsctl share mount /export --protocol smb ~/mnt/dittofs
```

Flags:

```
      --dir-mode string      Directory permissions for SMB mount (octal; default 0755 on Linux, 0777 on macOS)
      --file-mode string     File permissions for SMB mount (octal; default 0755 on Linux, 0777 on macOS)
      --nfs-version string   NFS protocol version for NFS mounts (3, 4, 4.0, 4.1, 4.2). v4 carries locking in-protocol; v3 locking needs the server UDP transport + portmapper (default "3")
  -P, --password string      Password for SMB mount (will prompt if not provided)
  -p, --protocol string      Protocol to use (nfs or smb) (required)
  -u, --username string      Username for SMB mount (defaults to login username)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share nfs-config`

Manage per-share NFS adapter configuration

View and update the NFS adapter configuration for a share.

This controls the share's export settings: the squash mode, the
authentication flags and the netgroup that restricts which clients may reach
the share. The authentication flags and the netgroup are the share's access
policy, which applies over NFS and SMB alike; the squash mode maps the
identities of SMB sessions too, guest sessions excepted. Changes take effect
immediately.

```
dfsctl share nfs-config [flags]
```

**Examples:**

```bash
# Show a share's NFS config
dfsctl share nfs-config show /export

# Associate a netgroup with the share's NFS export
dfsctl share nfs-config set /export --netgroup office-network

# Remove the netgroup association (allow all clients)
dfsctl share nfs-config set /export --netgroup ""
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share nfs-config set`

Update a share's NFS adapter configuration

Update the NFS adapter configuration for a share.

Only the flags you supply are changed; omitted flags leave the existing values
intact. Changes take effect immediately, without an adapter restart.

The netgroup allowlist and the authentication flags are the share's access
policy: they are enforced on NFSv3 mounts, on NFSv4 operations and on SMB
requests to the same share. The squash mode is not NFS-only either: it maps the
identities of SMB sessions on the share as well, guest sessions excepted.

```
dfsctl share nfs-config set <name> [flags]
```

**Examples:**

```bash
# Restrict access to a specific netgroup
dfsctl share nfs-config set /export --netgroup office-network

# Remove the netgroup restriction (allow all clients)
dfsctl share nfs-config set /export --netgroup ""

# Map root UID to guest on this export
dfsctl share nfs-config set /export --squash root_to_guest

# Require Kerberos authentication and disallow AUTH_SYS
dfsctl share nfs-config set /export --require-kerberos true --allow-auth-sys false
```

Flags:

```
      --allow-auth-sys string     Allow AUTH_SYS flavor (true|false)
      --netgroup string           Netgroup name to associate (empty string clears the association)
      --require-kerberos string   Require Kerberos auth (true|false)
      --squash string             Squash mode (none|root_to_admin|root_to_guest|all_to_admin|all_to_guest)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share nfs-config show`

Show a share's NFS adapter configuration

Show the NFS adapter configuration for a share.

Displays the netgroup association, squash mode, and authentication settings
(AUTH_SYS and Kerberos). Use this command to inspect the current NFS export
settings before making changes with 'nfs-config set'.

```
dfsctl share nfs-config show <name> [flags]
```

**Examples:**

```bash
# Show NFS config for a share
dfsctl share nfs-config show /export

# Emit as JSON
dfsctl share nfs-config show /export -o json
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share permission`

Manage share permissions

Manage permissions on shares.

Permission commands allow you to grant, revoke, and list permissions
for users and groups on shares.

```
dfsctl share permission [flags]
```

**Examples:**

```bash
# Grant read-write permission to a user
dfsctl share permission grant /archive --user alice --level read-write

# Grant read permission to a group
dfsctl share permission grant /archive --group editors --level read

# Revoke permission from a user
dfsctl share permission revoke /archive --user alice

# List permissions on a share
dfsctl share permission list /archive
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share permission grant`

Grant permission on a share

Grant a permission level to a user or group on a share.

Specify exactly one of --user, --group, or --sid together with --level.
Re-running the command on a principal that already has a permission replaces the
existing level. Permission levels in order of increasing access:

```
- none:       No access (explicitly blocks the principal)
- read:       Read-only access
- read-write: Read and write access
- admin:      Full administrative access including ACL management
```

Active Directory principals can be granted directly, with no local DittoFS
account (issue #1528). --user / --group accept a local name, an AD name
(user@REALM or DOMAIN\group, resolved to a SID via the configured LDAP
directory), or a raw Windows SID. A bare name resolves to a local user/group if
one exists, otherwise to the directory. --sid grants to a raw SID explicitly.

```
dfsctl share permission grant <share> [flags]
```

**Examples:**

```bash
# Grant read-write access to a local user
dfsctl share permission grant /archive --user alice --level read-write

# Grant read-only access to a local group
dfsctl share permission grant /archive --group editors --level read

# Grant directly to an AD group (resolved to its SID via LDAP) — no local group
dfsctl share permission grant /archive --group 'CUBBIT\Cubbit' --level read-write

# Grant directly to an AD user by Kerberos principal
dfsctl share permission grant /archive --user alice@cubbit.local --level read

# Grant to a raw Windows SID (no directory lookup)
dfsctl share permission grant /archive --sid S-1-5-21-1111-2222-3333-1104 --level read
```

Flags:

```
      --group string   Group to grant permission to (local name, AD name, or SID)
      --level string   Permission level (none|read|read-write|admin)
      --sid string     Raw Windows SID to grant permission to (e.g. S-1-5-21-...)
      --user string    User to grant permission to (local name, AD name, or SID)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share permission list`

List permissions on a share

List all user and group permissions configured on a share.

Each row shows the principal type (user or group), the principal's name, and
the permission level (none, read, read-write, or admin). Note that these are
per-principal overrides; clients without an explicit entry fall back to the
share's default permission (see 'dfsctl share show').

```
dfsctl share permission list <share> [flags]
```

**Examples:**

```bash
# List all permissions on a share
dfsctl share permission list /archive

# Emit permissions as JSON for scripting
dfsctl share permission list /archive -o json

# Emit as YAML
dfsctl share permission list /archive -o yaml
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share permission revoke`

Revoke permission from a share

Remove a per-principal permission entry from a share.

After revoking, the user or group falls back to the share's default permission
level (see 'dfsctl share show'). To explicitly block a principal rather than
fall back to the default, use 'dfsctl share permission grant ... --level none'
instead. Specify exactly one of --user, --group, or --sid.

--user / --group accept a local name, an AD name, or a raw SID (matching the
grant command); --sid revokes a raw SID grant directly.

```
dfsctl share permission revoke <share> [flags]
```

**Examples:**

```bash
# Revoke a user's explicit permission (they fall back to the share default)
dfsctl share permission revoke /archive --user alice

# Revoke a group's explicit permission
dfsctl share permission revoke /archive --group editors

# Revoke a direct AD/SID grant
dfsctl share permission revoke /archive --sid S-1-5-21-1111-2222-3333-1104
```

Flags:

```
      --group string   Group to revoke permission from (local name, AD name, or SID)
      --sid string     Raw Windows SID to revoke permission from
      --user string    User to revoke permission from (local name, AD name, or SID)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share remove`

Remove a share

Permanently remove a share from the DittoFS server.

Removing a share deletes its configuration, its records in its metadata store
and its local journal, so a share created again under the same name starts
empty. Blocks already synced to a remote block store are left to garbage
collection, and the metadata and block stores themselves are kept. If the
share's data cannot be deleted, the removal fails and keeps the share, so it
can be repeated. This operation is irreversible: you will be prompted for
confirmation unless --force is specified. Disable the share first
('dfsctl share disable') if you want to drain active clients before removing.

```
dfsctl share remove <name> [flags]
```

**Examples:**

```bash
# Remove a share, prompted for confirmation
dfsctl share remove /archive

# Remove without a confirmation prompt (useful in scripts)
dfsctl share remove /archive --force

# Drain clients first, then remove without prompting
dfsctl share disable /archive && dfsctl share remove /archive --force
```

Flags:

```
  -f, --force   Skip confirmation prompt
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share show`

Show share details

Show a detailed, field-by-field view of a single share.

Unlike 'share list', which shows summary columns, 'share show' displays every
attribute of the share: store IDs, read-only state, ACL settings, retention
policy and TTL, cache size overrides, quota, and creation/update timestamps. Use this command when debugging a misconfigured
share or before editing it.

```
dfsctl share show <name> [flags]
```

**Examples:**

```bash
# Show all fields for a share
dfsctl share show /archive

# Emit the full share record as JSON (useful for scripting or diffing)
dfsctl share show /archive -o json

# Emit as YAML
dfsctl share show /archive -o yaml
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl status`

Show server status

Display the status of the connected DittoFS server.

Calls the /health endpoint on the server configured in the current context and
reports whether the server is running, how long it has been up, and whether the
control-plane database is reachable. When a valid token is present, per-entity
detail (shares, stores) is fetched from the API and rendered as a
color-coded table. Use -o json or -o yaml for machine-readable output.

```
dfsctl status [flags]
```

**Examples:**

```bash
# Show status of the currently active server
dfsctl status

# Emit machine-readable JSON output
dfsctl status -o json
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl token`

Manage API tokens

Create, list and revoke API tokens: credentials a script or a service signs
in with, through 'dfsctl login --token-file', in place of a password.

Each command acts on the signed-in account's tokens; an admin names another
account with --user, and creates one for another account only when that
account is a service account, one holding no password. A context signed in
with an API token names no account to dfsctl, so it needs --user, and it
creates no token.

```
dfsctl token [flags]
```

**Examples:**

```bash
# Create a token valid for 30 days, kept in a file
dfsctl token create --expires-in 720h > token.txt

# Sign a context in with it
dfsctl login --server https://dfs.example.com --token-file token.txt -c ci

# Revoke it
dfsctl token revoke 1a92dcf1-bdbb-49eb-acc1-a78ef6b4e5f1
```

Flags:

```
      --user string   The account whose tokens to act on, by its name (defaults to the signed-in one)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl token create`

Create an API token

Create an API token expiring --expires-in from now. The token is shown this
once: as a table, stdout holds the token alone, so it can be written to a file;
-o json or -o yaml print the whole record, the token in its status.

```
dfsctl token create [flags]
```

Flags:

```
      --expires-in duration   How long the token is valid, at most the server's api.token_max_lifetime (default 720h0m0s)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --user string     The account whose tokens to act on, by its name (defaults to the signed-in one)
  -v, --verbose         Enable verbose output
```

### `dfsctl token list`

List API tokens

```
dfsctl token list [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --user string     The account whose tokens to act on, by its name (defaults to the signed-in one)
  -v, --verbose         Enable verbose output
```

### `dfsctl token revoke`

Revoke an API token

Revoke an API token, named by its ID, so the server refuses it from then on.

```
dfsctl token revoke <id> [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --user string     The account whose tokens to act on, by its name (defaults to the signed-in one)
  -v, --verbose         Enable verbose output
```

### `dfsctl user`

Manage users

Create, list, read, change and delete the server's users, and set their
passwords.

A user is an account of the management API when it holds a role, and an owner
of files when it holds a uid; one may be both. Each command names a user by its
name, or by its ID with --id.

```
dfsctl user [flags]
```

**Examples:**

```bash
# Create a user owning files as uid 1000
dfsctl user create alice --uid 1000 --email alice@example.com

# Give it a password, read from a file
dfsctl user set-password alice --password-file pw.txt

# Clear its e-mail address
dfsctl user update alice --email ""

# Delete it and wait for the deletion to end
dfsctl user delete alice --force --wait
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user create`

Create a user

Create a user. It has no password until 'dfsctl user set-password' gives it one.

```
dfsctl user create <name> [flags]
```

Flags:

```
      --disabled       Refuse the user on every protocol and on the API
      --email string   E-mail address; empty clears it
      --role string    Role in the management API (viewer|operator|admin); empty for none
      --uid uint32     Unix user ID, which a create leaving it out allocates
      --wait           Follow the operation the command starts to its end
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user delete`

Delete a user

Delete a user. The deletion is an operation: the command prints its ID, or,
with --wait, follows it to its end.

```
dfsctl user delete <name> [flags]
```

Flags:

```
  -f, --force       Delete without asking
      --id string   The user's ID, in place of its name
      --wait        Follow the operation the command starts to its end
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user get`

Show a user

```
dfsctl user get <name> [flags]
```

Flags:

```
      --id string   The user's ID, in place of its name
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user list`

List users

```
dfsctl user list [flags]
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user set-password`

Set a user's password

Set a user's password, read from a prompt or from the file --password-file
names. The user's sessions and API tokens are refused from then on. To change
your own password, run 'dfsctl password change'.

```
dfsctl user set-password <name> [flags]
```

Flags:

```
      --id string              The user's ID, in place of its name
      --password-file string   File holding the password (prompts without it)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user update`

Change a user

Change the fields of a user that the flags name, leaving the others as they
are. An empty --email clears the address, and an empty --role takes the role
away.

```
dfsctl user update <name> [flags]
```

Flags:

```
      --disabled       Refuse the user on every protocol and on the API
      --email string   E-mail address; empty clears it
      --id string      The user's ID, in place of its name
      --role string    Role in the management API (viewer|operator|admin); empty for none
      --uid uint32     Unix user ID, which a create leaving it out allocates
      --wait           Follow the operation the command starts to its end
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl version`

Show version information

Display the dfsctl build version and system information.

Shows the full semantic version, git commit, build date, Go toolchain version,
and OS/architecture. Use --short to emit only the version string for scripting.

```
dfsctl version [flags]
```

**Examples:**

```bash
# Show full version information
dfsctl version

# Print only the version number (useful in scripts)
dfsctl version --short
```

Flags:

```
      --short   Show only version number
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

