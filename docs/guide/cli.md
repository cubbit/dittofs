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
      --short[=true|false]   Show only version number
```

Global flags:

```
      --config string   config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)
```

## `dfsctl`

- [`dfsctl`](#dfsctl) — DittoFS Control - Remote management client
  - [`dfsctl adapter`](#dfsctl-adapter) — Manage protocol adapters
    - [`dfsctl adapter get`](#dfsctl-adapter-get) — Show an adapter
    - [`dfsctl adapter list`](#dfsctl-adapter-list) — List adapters
    - [`dfsctl adapter set`](#dfsctl-adapter-set) — Change an adapter
  - [`dfsctl audit`](#dfsctl-audit) — Read the audit of management actions
    - [`dfsctl audit list`](#dfsctl-audit-list) — List audit records
  - [`dfsctl completion`](#dfsctl-completion) — Generate shell completion script
  - [`dfsctl context`](#dfsctl-context) — Manage server contexts
    - [`dfsctl context current`](#dfsctl-context-current) — Show current context
    - [`dfsctl context list`](#dfsctl-context-list) — List all configured contexts
    - [`dfsctl context remove`](#dfsctl-context-remove) — Remove a context
    - [`dfsctl context rename`](#dfsctl-context-rename) — Rename a context
    - [`dfsctl context use`](#dfsctl-context-use) — Switch to a different context
  - [`dfsctl domain`](#dfsctl-domain) — Resolve principals, test the directory and join the domain
    - [`dfsctl domain join`](#dfsctl-domain-join) — Join the installation to the domain
    - [`dfsctl domain resolve`](#dfsctl-domain-resolve) — Resolve a directory principal by name or by SID
    - [`dfsctl domain rotate-machine-password`](#dfsctl-domain-rotate-machine-password) — Rotate the computer account's password
    - [`dfsctl domain test`](#dfsctl-domain-test) — Test the installation's directory
  - [`dfsctl grant`](#dfsctl-grant) — Manage principals' access to shares
    - [`dfsctl grant list`](#dfsctl-grant-list) — List a share's grants
    - [`dfsctl grant remove`](#dfsctl-grant-remove) — Remove a principal's grant on a share
    - [`dfsctl grant set`](#dfsctl-grant-set) — Set a principal's access to a share
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
  - [`dfsctl namespace`](#dfsctl-namespace) — Manage namespaces
    - [`dfsctl namespace create`](#dfsctl-namespace-create) — Create a namespace on a remote store
    - [`dfsctl namespace delete`](#dfsctl-namespace-delete) — Delete a namespace no share names
    - [`dfsctl namespace get`](#dfsctl-namespace-get) — Show a namespace
    - [`dfsctl namespace list`](#dfsctl-namespace-list) — List namespaces
  - [`dfsctl netgroup`](#dfsctl-netgroup) — Manage netgroups
    - [`dfsctl netgroup create`](#dfsctl-netgroup-create) — Create a netgroup
    - [`dfsctl netgroup delete`](#dfsctl-netgroup-delete) — Delete a netgroup
    - [`dfsctl netgroup get`](#dfsctl-netgroup-get) — Show a netgroup
    - [`dfsctl netgroup list`](#dfsctl-netgroup-list) — List netgroups
    - [`dfsctl netgroup update`](#dfsctl-netgroup-update) — Replace a netgroup's members
  - [`dfsctl node`](#dfsctl-node) — List the installation's nodes
    - [`dfsctl node get`](#dfsctl-node-get) — Show a node
    - [`dfsctl node list`](#dfsctl-node-list) — List nodes
  - [`dfsctl operation`](#dfsctl-operation) — Follow long-running operations
    - [`dfsctl operation cancel`](#dfsctl-operation-cancel) — Cancel an operation
    - [`dfsctl operation get`](#dfsctl-operation-get) — Show an operation
    - [`dfsctl operation list`](#dfsctl-operation-list) — List operations
    - [`dfsctl operation wait`](#dfsctl-operation-wait) — Wait for an operation to end
  - [`dfsctl password`](#dfsctl-password) — Manage your own password
    - [`dfsctl password change`](#dfsctl-password-change) — Change the signed-in account's password
  - [`dfsctl quota`](#dfsctl-quota) — Manage a share's quotas
    - [`dfsctl quota list`](#dfsctl-quota-list) — List a share's quotas, its own first
    - [`dfsctl quota remove`](#dfsctl-quota-remove) — Remove a quota
    - [`dfsctl quota set`](#dfsctl-quota-set) — Set a quota's limits
  - [`dfsctl setting`](#dfsctl-setting) — Read and change settings
    - [`dfsctl setting get`](#dfsctl-setting-get) — Show a setting
    - [`dfsctl setting set`](#dfsctl-setting-set) — Set a setting
    - [`dfsctl setting unset`](#dfsctl-setting-unset) — Reset a setting to the wider scope's value or the default
  - [`dfsctl share`](#dfsctl-share) — Manage shares
    - [`dfsctl share create`](#dfsctl-share-create) — Create a share with its export policy
    - [`dfsctl share delete`](#dfsctl-share-delete) — Delete a share
    - [`dfsctl share export-policy`](#dfsctl-share-export-policy) — Read and write a share's export policy
      - [`dfsctl share export-policy get`](#dfsctl-share-export-policy-get) — Show a share's export policy
      - [`dfsctl share export-policy set`](#dfsctl-share-export-policy-set) — Write a share's whole export policy
      - [`dfsctl share export-policy update`](#dfsctl-share-export-policy-update) — Change fields of a share's export policy
    - [`dfsctl share get`](#dfsctl-share-get) — Show a share
    - [`dfsctl share list`](#dfsctl-share-list) — List shares
    - [`dfsctl share mount`](#dfsctl-share-mount) — Mount a share on this host over NFS or SMB
    - [`dfsctl share quiesce`](#dfsctl-share-quiesce) — Quiesce a share: refuse new opens and drain its clients
    - [`dfsctl share resume`](#dfsctl-share-resume) — Resume a quiesced share
    - [`dfsctl share update`](#dfsctl-share-update) — Rename a share or move its NFS path
  - [`dfsctl status`](#dfsctl-status) — Show the node's readiness and the installation's health
  - [`dfsctl store`](#dfsctl-store) — Manage remote stores
    - [`dfsctl store create`](#dfsctl-store-create) — Create a remote store
    - [`dfsctl store delete`](#dfsctl-store-delete) — Delete a remote store no namespace names
    - [`dfsctl store get`](#dfsctl-store-get) — Show a remote store
    - [`dfsctl store list`](#dfsctl-store-list) — List remote stores
    - [`dfsctl store set-credential`](#dfsctl-store-set-credential) — Set a remote store's credential
    - [`dfsctl store update`](#dfsctl-store-update) — Change a remote store's settings
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

The context subcommand manages the server contexts kept in
~/.config/dfsctl/config.json.

Use "dfsctl [command] --help" for more information about a command.

Flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl adapter`

Manage protocol adapters

Read and set the protocol adapters, each named by its protocol: nfs or smb.

An adapter's settings are its fields below adapters.<protocol>, its port among
them; a node override of one, such as one host's port, is a setting at that
node's scope, set through 'dfsctl setting set node:<id>'.

```
dfsctl adapter [flags]
```

**Examples:**

```bash
# List the adapters
dfsctl adapter list

# Show the NFS adapter on every node
dfsctl adapter get nfs

# Enable the SMB adapter on port 1445
dfsctl adapter set smb --enabled --setting port=1445
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl adapter get`

Show an adapter

```
dfsctl adapter get <protocol> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl adapter list`

List adapters

```
dfsctl adapter list [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl adapter set`

Change an adapter

Change an adapter: --enabled or --enabled=false, each --setting key=value
set, a value that is JSON taken as it is and any other as a string, and each
--unset key removed, so its default holds. What is not named stays as it is.

```
dfsctl adapter set <protocol> [flags]
```

Flags:

```
      --enabled[=true|false]   Enable the adapter, or disable it with --enabled=false
      --setting stringArray    A setting of the adapter, key=value (repeatable)
      --unset stringArray      A setting of the adapter to remove (repeatable)
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl audit`

Read the audit of management actions

```
dfsctl audit [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl audit list`

List audit records

List the audit records the filters admit, oldest first: who acted, on what,
and whether the action was applied, refused or started an operation.

--actor names a user by name, or any account as kind:id, or the schedule as
schedule. A user named schedule, or whose name holds ':', is named as
user:<id>. --target names a resource as kind:id; --from and --to are RFC 3339
times.

```
dfsctl audit list [flags]
```

**Examples:**

```bash
# What alice changed today
dfsctl audit list --actor alice --from 2026-11-24T00:00:00Z

# Every action on one share
dfsctl audit list --target share:0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b
```

Flags:

```
      --actor string    Only the records of this account: a user's name, kind:id or schedule (a user named schedule or with ':' in its name as user:<id>)
      --from string     Only the records at or after this RFC 3339 time
      --target string   Only the records of this resource, as kind:id
      --to string       Only the records before this RFC 3339 time
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
  -f, --force[=true|false]   Skip confirmation
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl domain`

Resolve principals, test the directory and join the domain

Act on the installation's directory and its domain join.

Each command starts an operation and prints it; with --wait it follows the
operation to its end, and 'dfsctl operation wait <id>' resumes an interrupted
wait.

```
dfsctl domain [flags]
```

**Examples:**

```bash
# Find a directory user's SID
dfsctl domain resolve 'CORP\bob' --wait

# Join the domain as an administrator (prompts for the password)
dfsctl domain join --user administrator@corp.example.com --wait

# Join with a computer account an administrator created
dfsctl domain join --offline --account FILESRV --password-file machine.txt
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl domain join`

Join the installation to the domain

Join the installation to the directory's domain.

Online, the default, creates the computer account as the administrator --user
names, whose password is read from --password-file or a prompt. With --offline
the join takes a computer account an administrator created, and the password
read is that account's. The password is sent once, with no idempotency key, so
the server keeps no record of the request.

```
dfsctl domain join [flags]
```

Flags:

```
      --account string          The computer account's name (defaults to the installation's)
      --dns-host-name string    The DNS host name of an online join's computer account
      --offline[=true|false]    Take a computer account an administrator created
      --ou string               The organisational unit of an online join's computer account
      --password-file string    File holding the password (prompts without it)
      --takeover[=true|false]   Replace a join the installation holds, and online an account of that name it did not create
      --user string             The administrator an online join creates the computer account as
      --wait[=true|false]       Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl domain resolve`

Resolve a directory principal by name or by SID

```
dfsctl domain resolve [DOMAIN\name] [flags]
```

Flags:

```
      --sid string          The principal's SID, in place of its name
      --wait[=true|false]   Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl domain rotate-machine-password`

Rotate the computer account's password

```
dfsctl domain rotate-machine-password [flags]
```

Flags:

```
      --wait[=true|false]   Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl domain test`

Test the installation's directory

```
dfsctl domain test [flags]
```

Flags:

```
      --wait[=true|false]   Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl grant`

Manage principals' access to shares

Set, remove and list grants: a user's, a group's or a directory principal's
access to a share, none, read, read-write or admin.

A grant of none is kept and refuses the principal; removing a grant lets the
principal's groups' grants decide again. A directory principal is named by
its qualified name, DOMAIN\name, which the server first resolves.

```
dfsctl grant [flags]
```

**Examples:**

```bash
# Let alice read and write a share
dfsctl grant set archive --user alice --access read-write

# Let a directory user read it
dfsctl grant set archive --directory 'CORP\bob' --access read

# List a share's grants
dfsctl grant list archive
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl grant list`

List a share's grants

```
dfsctl grant list [share] [flags]
```

Flags:

```
      --id string   The share's ID, in place of its name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl grant remove`

Remove a principal's grant on a share

```
dfsctl grant remove [share] [flags]
```

Flags:

```
      --directory string      A directory principal's qualified name, DOMAIN\name
      --group string          The group's name
      --id string             The share's ID, in place of its name
      --principal-id string   The principal's ID, as a listing prints it
      --user string           The user's name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl grant set`

Set a principal's access to a share

```
dfsctl grant set [share] [flags]
```

Flags:

```
      --access string         The access: none, read, read-write or admin
      --directory string      A directory principal's qualified name, DOMAIN\name
      --group string          The group's name
      --id string             The share's ID, in place of its name
      --principal-id string   The principal's ID, as a listing prints it
      --user string           The user's name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl group add-member`

Add a user to a group

```
dfsctl group add-member <group> <user> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
  -f, --force[=true|false]   Delete without asking
      --id string            The group's ID, in place of its name
      --wait[=true|false]    Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl group list`

List groups

```
dfsctl group list [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl group remove-member`

Remove a user from a group

```
dfsctl group remove-member <group> <user> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl installation`

Act on the installation as a whole

```
dfsctl installation [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --wait[=true|false]   Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl namespace`

Manage namespaces

Create, list, read and delete namespaces.

A namespace is created on a remote store with its bound settings, such as its
prefix and chunking, which are fixed from then on.

```
dfsctl namespace [flags]
```

**Examples:**

```bash
# Create a namespace on a store
dfsctl namespace create --store 0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b --setting namespace.prefix=builds/

# List the namespaces
dfsctl namespace list
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl namespace create`

Create a namespace on a remote store

Create a namespace on the remote store --store names, with its settings, each
--setting key=value, a value that is JSON taken as it is and any other as a
string.

```
dfsctl namespace create [flags]
```

Flags:

```
      --setting stringArray   A setting of the namespace, key=value (repeatable)
      --store string          The ID of the remote store the namespace is created on
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl namespace delete`

Delete a namespace no share names

```
dfsctl namespace delete <id> [flags]
```

Flags:

```
  -f, --force[=true|false]   Delete without asking for confirmation
      --wait[=true|false]    Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl namespace get`

Show a namespace

```
dfsctl namespace get <id> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl namespace list`

List namespaces

```
dfsctl namespace list [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl netgroup`

Manage netgroups

Create, list, read, change and delete netgroups: named sets of host names,
addresses and CIDRs that an export policy admits.

A netgroup holds at least one member: create needs --member, and an update
replaces the members with those --member names.

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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl netgroup delete`

Delete a netgroup

```
dfsctl netgroup delete <name> [flags]
```

Flags:

```
  -f, --force[=true|false]   Delete without asking
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl netgroup get`

Show a netgroup

```
dfsctl netgroup get <name> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl netgroup list`

List netgroups

```
dfsctl netgroup list [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl netgroup update`

Replace a netgroup's members

Replace a netgroup's members with those --member names. To empty it, delete it.

```
dfsctl netgroup update <name> [flags]
```

Flags:

```
      --member stringArray   A host name, address or CIDR; repeat for each
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl node`

List the installation's nodes

List and read the installation's nodes: the address each serves on, the
binary it runs, its failure domain, and whether a takeover marked it lapsed.

```
dfsctl node [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl node get`

Show a node

```
dfsctl node get <id> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl node list`

List nodes

```
dfsctl node list [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl operation cancel`

Cancel an operation

```
dfsctl operation cancel <id> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl operation get`

Show an operation

```
dfsctl operation get <id> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl password`

Manage your own password

```
dfsctl password [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl quota`

Manage a share's quotas

Set, remove and list a share's quotas: on a user, on a group, or, with
neither named, on the share itself.

A quota is refused beyond its hard limit, allowed beyond its soft limit for
its grace and then refused, and never refused beyond its advisory limit,
which only raises an event. A limit of 0 limits nothing.

```
dfsctl quota [flags]
```

**Examples:**

```bash
# Limit alice to 10 GiB and 100000 files on a share
dfsctl quota set archive --user alice --hard-bytes 10737418240 --hard-files 100000

# Limit the share itself, with a soft limit and a week's grace
dfsctl quota set archive --soft-bytes 1099511627776 --grace 168h

# List a share's quotas
dfsctl quota list archive
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl quota list`

List a share's quotas, its own first

```
dfsctl quota list [share] [flags]
```

Flags:

```
      --id string   The share's ID, in place of its name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl quota remove`

Remove a quota

```
dfsctl quota remove [share] [flags]
```

Flags:

```
      --group string          The group's name
      --id string             The share's ID, in place of its name
      --principal-id string   The principal's ID, as a listing prints it
      --user string           The user's name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl quota set`

Set a quota's limits

Set a quota's limits, creating the quota when there is none; a limit whose
flag is not given keeps its value.

```
dfsctl quota set [share] [flags]
```

Flags:

```
      --advisory-bytes int    The advisory limit on bytes, 0 for none
      --advisory-files int    The advisory limit on files, 0 for none
      --grace duration        How long the soft limits may be passed before they are refused
      --group string          The group's name
      --hard-bytes int        The hard limit on bytes, 0 for none
      --hard-files int        The hard limit on files, 0 for none
      --id string             The share's ID, in place of its name
      --principal-id string   The principal's ID, as a listing prints it
      --soft-bytes int        The soft limit on bytes, 0 for none
      --soft-files int        The soft limit on files, 0 for none
      --user string           The user's name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl setting`

Read and change settings

Read, set and reset any setting by its scope and key.

The scope is installation, or node:<id>, namespace:<id>, store:<id> or
share:<name>. A value that is JSON, such as 8080, true or {"a":1}, is taken as
it is, and any other as a string; quote a string that reads as JSON: '"true"'.
Unsetting a setting removes it at that scope, so the wider scope's value, or
the default, holds.

```
dfsctl setting [flags]
```

**Examples:**

```bash
# Read the installation's session lifetime
dfsctl setting get installation api.session_lifetime

# Bind one node's NFS adapter to another port
dfsctl setting set node:7f3a9c1e-0b2d-4e5f-8a6b-1c2d3e4f5a6b adapters.nfs.port 2050

# Reset it to the installation's value
dfsctl setting unset node:7f3a9c1e-0b2d-4e5f-8a6b-1c2d3e4f5a6b adapters.nfs.port
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl setting get`

Show a setting

```
dfsctl setting get <scope> <key> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl setting set`

Set a setting

```
dfsctl setting set <scope> <key> <value> [flags]
```

Flags:

```
      --wait[=true|false]   Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl setting unset`

Reset a setting to the wider scope's value or the default

```
dfsctl setting unset <scope> <key> [flags]
```

Flags:

```
      --wait[=true|false]   Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share`

Manage shares

Create, read, change, quiesce, resume and delete shares, set their export
policies and mount them on this host.

A share is named by its name, the one an SMB client tree-connects to, or by
--id. grants and quotas on a share are the grant and quota commands.

```
dfsctl share [flags]
```

**Examples:**

```bash
# Create a share in a namespace, open to one network
dfsctl share create archive --path /archive --namespace <namespace-id> --flavor sys --client 10.0.0.0/8=read-write

# List the shares
dfsctl share list

# Move a share's NFS path
dfsctl share update archive --path /old-archive

# Delete a share and wait for its deletion to end
dfsctl share delete archive --force --wait
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share create`

Create a share with its export policy

Create a share with its export policy.

The share is named by the name an SMB client tree-connects to; --path is its
path in the NFS namespace. The export policy's flags are those of
'share export-policy set'; a policy states its client rules, so name at least
one with --client (0.0.0.0/0 and ::/0 admit every client).

```
dfsctl share create <name> [flags]
```

Flags:

```
      --case-insensitive[=true|false]             Fold the case of names; bound when the share is created
      --client stringArray                        A client rule, CIDR=access or @netgroup=access, access none, read or read-write; the first match decides, in order
      --flavor strings                            The flavours SECINFO answers, in preference order: none, sys or kerberos
      --manage-gids[=true|false]                  Replace an AUTH_SYS caller's gids with the groups the store and directory hold for its user
      --min-proof string                          The proof every caller gives: none, claimed, secret or ticket
      --min-protection string                     The protection messages need: none, integrity or privacy
      --namespace string                          The ID of the namespace the share's content is counted in
      --path string                               The share's path in the NFS namespace
      --read-only[=true|false]                    Hold every caller to read-only
      --smb-continuously-available[=true|false]   Make the share's SMB handles continuously available
      --smb-encrypt[=true|false]                  Seal every SMB message
      --smb-hidden[=true|false]                   Leave the share out of SMB share enumeration
      --squash string                             Whose identity is squashed: none, root or all
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share delete`

Delete a share

Delete a share. The deletion is an operation: the command prints it, or,
with --wait, follows it to its end.

```
dfsctl share delete [name] [flags]
```

Flags:

```
  -f, --force[=true|false]   Delete without asking
      --id string            The share's ID, in place of its name
      --wait[=true|false]    Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share export-policy`

Read and write a share's export policy

Read and write a share's export policy: who it admits, with which proof and
protection, how it squashes identities, and its SMB flags.

'set' writes the whole policy, --flavor and --client required, any other field
whose flag is not given taken back to its default; 'update' changes only the fields whose flags are given.

```
dfsctl share export-policy [flags]
```

**Examples:**

```bash
# Admit one network read-write and one netgroup read-only
dfsctl share export-policy set archive --flavor sys --client 10.0.0.0/8=read-write --client @auditors=read

# Require Kerberos with privacy, leaving the rest as it is
dfsctl share export-policy update archive --flavor kerberos --min-proof ticket --min-protection privacy
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share export-policy get`

Show a share's export policy

```
dfsctl share export-policy get [share] [flags]
```

Flags:

```
      --id string   The share's ID, in place of its name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share export-policy set`

Write a share's whole export policy

```
dfsctl share export-policy set [share] [flags]
```

Flags:

```
      --client stringArray                        A client rule, CIDR=access or @netgroup=access, access none, read or read-write; the first match decides, in order
      --flavor strings                            The flavours SECINFO answers, in preference order: none, sys or kerberos
      --id string                                 The share's ID, in place of its name
      --manage-gids[=true|false]                  Replace an AUTH_SYS caller's gids with the groups the store and directory hold for its user
      --min-proof string                          The proof every caller gives: none, claimed, secret or ticket
      --min-protection string                     The protection messages need: none, integrity or privacy
      --read-only[=true|false]                    Hold every caller to read-only
      --smb-continuously-available[=true|false]   Make the share's SMB handles continuously available
      --smb-encrypt[=true|false]                  Seal every SMB message
      --smb-hidden[=true|false]                   Leave the share out of SMB share enumeration
      --squash string                             Whose identity is squashed: none, root or all
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share export-policy update`

Change fields of a share's export policy

```
dfsctl share export-policy update [share] [flags]
```

Flags:

```
      --client stringArray                        A client rule, CIDR=access or @netgroup=access, access none, read or read-write; the first match decides, in order
      --flavor strings                            The flavours SECINFO answers, in preference order: none, sys or kerberos
      --id string                                 The share's ID, in place of its name
      --manage-gids[=true|false]                  Replace an AUTH_SYS caller's gids with the groups the store and directory hold for its user
      --min-proof string                          The proof every caller gives: none, claimed, secret or ticket
      --min-protection string                     The protection messages need: none, integrity or privacy
      --read-only[=true|false]                    Hold every caller to read-only
      --smb-continuously-available[=true|false]   Make the share's SMB handles continuously available
      --smb-encrypt[=true|false]                  Seal every SMB message
      --smb-hidden[=true|false]                   Leave the share out of SMB share enumeration
      --squash string                             Whose identity is squashed: none, root or all
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share get`

Show a share

```
dfsctl share get [name] [flags]
```

Flags:

```
      --id string   The share's ID, in place of its name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share list`

List shares

```
dfsctl share list [flags]
```

Flags:

```
      --namespace string   Only the shares counted in the namespace of this ID
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share mount`

Mount a share on this host over NFS or SMB

Mount a share at a mount point on this host, over NFS or SMB, from the server
the current context names, on the port its adapter's record gives.

Run dfsctl as yourself, not under sudo: it reads your login, and runs the
host's mount command through sudo itself where that needs root. A macOS SMB
mount runs as you, since macOS lets only a mount's owner reach its files, and
a Linux SMB mount is made for your uid and gid. For SMB the mount command asks
for the password itself; dfsctl never handles it. --dry-run prints the
command, sudo included, instead of running it.

An SMB mount on Windows from any port but 445 needs net use's /TCPPORT,
which Windows 11 24H2 and Windows Server 2025 are the first to have, and an
administrator prompt.

```
dfsctl share mount [share] <mountpoint> [flags]
```

**Examples:**

```bash
# Mount over NFS
dfsctl share mount archive /mnt/archive --protocol nfs

# Mount over NFSv4.1
dfsctl share mount archive /mnt/archive --protocol nfs --nfs-version 4.1

# Mount over SMB as alice
dfsctl share mount archive /mnt/archive --protocol smb --username alice

# Map a drive on Windows
dfsctl share mount archive Z: --protocol smb
```

Flags:

```
      --dry-run[=true|false]   Print the mount command instead of running it
      --id string              The share's ID, in place of its name
      --nfs-version string     The NFS version: 3, 4, 4.0, 4.1 or 4.2 (default "3")
  -p, --protocol string        The protocol to mount over: nfs or smb
  -u, --username string        The SMB user; the current context's account when not given
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share quiesce`

Quiesce a share: refuse new opens and drain its clients

```
dfsctl share quiesce [name] [flags]
```

Flags:

```
      --id string   The share's ID, in place of its name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share resume`

Resume a quiesced share

```
dfsctl share resume [name] [flags]
```

Flags:

```
      --id string   The share's ID, in place of its name
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl share update`

Rename a share or move its NFS path

```
dfsctl share update [name] [flags]
```

Flags:

```
      --id string     The share's ID, in place of its name
      --name string   The share's new name
      --path string   The share's new path in the NFS namespace
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl status`

Show the node's readiness and the installation's health

Show the readiness of the node the current context names, ready, starting,
draining or not_ready, and every health condition it holds, its own and the
installation's. Use -o json or -o yaml for machine-readable output.

```
dfsctl status [flags]
```

**Examples:**

```bash
# Show the status of the current context's node
dfsctl status

# Emit machine-readable JSON output
dfsctl status -o json
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl store`

Manage remote stores

Create, list, read, change and delete remote stores, and set a store's
credential.

A remote store is where namespaces keep their content: its endpoint, bucket and
the rest are its settings, and its credential is a secret the server seals and
never prints. The server refuses a change of a bound setting while a namespace
names the store, and a change of how the store is reached, its endpoint among
them, until it can prove the store holds the same content there.

```
dfsctl store [flags]
```

**Examples:**

```bash
# Create a store, its credential read from a file
dfsctl store create --setting remote.endpoint=https://s3.example.com --credential-file cred.txt

# Change a store's region and remove its storage class, so its default holds
dfsctl store update 0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b --setting remote.region=eu-west-1 --unset remote.storage_class

# Replace a store's credential (prompts for it)
dfsctl store set-credential 0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl store create`

Create a remote store

Create a remote store with its settings, each --setting key=value, a value
that is JSON taken as it is and any other as a string. With --credential-file
the store is created with the credential that file holds.

```
dfsctl store create [flags]
```

Flags:

```
      --credential-file string   File holding the store's credential
      --setting stringArray      A setting of the store, key=value (repeatable)
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl store delete`

Delete a remote store no namespace names

```
dfsctl store delete <id> [flags]
```

Flags:

```
  -f, --force[=true|false]   Delete without asking for confirmation
      --wait[=true|false]    Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl store get`

Show a remote store

```
dfsctl store get <id> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl store list`

List remote stores

```
dfsctl store list [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl store set-credential`

Set a remote store's credential

Set a remote store's credential, read from the file --credential-file names
or, without it, from a prompt. The server proves the credential before it
writes it.

```
dfsctl store set-credential <id> [flags]
```

Flags:

```
      --credential-file string   File holding the credential (prompts without it)
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl store update`

Change a remote store's settings

Change a remote store's settings in one write: each --setting key=value set, a
value that is JSON taken as it is and any other as a string, and each --unset
key removed, so its default holds. What is not named stays as it is. Its
credential is set by 'dfsctl store set-credential'.

```
dfsctl store update <id> [flags]
```

Flags:

```
      --setting stringArray   A setting of the store, key=value (repeatable)
      --unset stringArray     A setting of the store to remove (repeatable)
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
(umask 077; dfsctl token create --expires-in 720h > token.txt)

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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
      --user string             The account whose tokens to act on, by its name (defaults to the signed-in one)
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl token list`

List API tokens

```
dfsctl token list [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
      --user string             The account whose tokens to act on, by its name (defaults to the signed-in one)
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl token revoke`

Revoke an API token

Revoke an API token, named by its ID, so the server refuses it from then on.

```
dfsctl token revoke <id> [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
      --user string             The account whose tokens to act on, by its name (defaults to the signed-in one)
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl user create`

Create a user

Create a user. It has no password until 'dfsctl user set-password' gives it one.

```
dfsctl user create <name> [flags]
```

Flags:

```
      --disabled[=true|false]   Refuse the user on every protocol and on the API
      --email string            E-mail address; empty clears it
      --role string             Role in the management API (viewer|operator|admin); empty for none
      --uid uint32              Unix user ID, which a create leaving it out allocates
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
  -f, --force[=true|false]   Delete without asking
      --id string            The user's ID, in place of its name
      --wait[=true|false]    Follow the operation the command starts to its end
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

### `dfsctl user list`

List users

```
dfsctl user list [flags]
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --disabled[=true|false]   Refuse the user on every protocol and on the API
      --email string            E-mail address; empty clears it
      --id string               The user's ID, in place of its name
      --role string             Role in the management API (viewer|operator|admin); empty for none
      --uid uint32              Unix user ID, which a create leaving it out allocates
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
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
      --short[=true|false]   Show only version number
```

Global flags:

```
      --no-color[=true|false]   Disable colored output
  -o, --output string           Output format (table|json|yaml) (default "table")
  -v, --verbose[=true|false]    Enable verbose output
```

