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
  - [`dfsctl grant`](#dfsctl-grant) — Manage principals' access to shares
    - [`dfsctl grant list`](#dfsctl-grant-list) — List a share's grants
    - [`dfsctl grant remove`](#dfsctl-grant-remove) — Remove a principal's grant on a share
    - [`dfsctl grant set`](#dfsctl-grant-set) — Set a principal's access to a share
  - [`dfsctl group`](#dfsctl-group) — Group management
    - [`dfsctl group add-user`](#dfsctl-group-add-user) — Add a user to a group
    - [`dfsctl group create`](#dfsctl-group-create) — Create a new group
    - [`dfsctl group edit`](#dfsctl-group-edit) — Edit a group
    - [`dfsctl group get`](#dfsctl-group-get) — Get group details
    - [`dfsctl group list`](#dfsctl-group-list) — List all groups
    - [`dfsctl group remove`](#dfsctl-group-remove) — Remove a group
    - [`dfsctl group remove-user`](#dfsctl-group-remove-user) — Remove a user from a group
  - [`dfsctl login`](#dfsctl-login) — Authenticate with DittoFS server
  - [`dfsctl logout`](#dfsctl-logout) — Clear stored credentials
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
  - [`dfsctl status`](#dfsctl-status) — Show server status
  - [`dfsctl user`](#dfsctl-user) — User management
    - [`dfsctl user change-password`](#dfsctl-user-change-password) — Change your own password
    - [`dfsctl user create`](#dfsctl-user-create) — Create a new user
    - [`dfsctl user edit`](#dfsctl-user-edit) — Edit a user
    - [`dfsctl user get`](#dfsctl-user-get) — Get user details
    - [`dfsctl user list`](#dfsctl-user-list) — List all users
    - [`dfsctl user password`](#dfsctl-user-password) — Reset a user's password
    - [`dfsctl user remove`](#dfsctl-user-remove) — Remove a user
  - [`dfsctl version`](#dfsctl-version) — Show version information


### `dfsctl`

DittoFS Control - Remote management client

dfsctl is the command-line client for managing DittoFS servers remotely.

Its commands for users and groups return with the server's v2 API; until
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group`

Group management

Manage Unix groups on the DittoFS server. Groups bundle users together so
that share permissions can be granted to multiple users at once using a single
group reference. Each group carries a Unix GID used for NFS uid/gid resolution.
All subcommands require admin privileges.

```
dfsctl group [flags]
```

**Examples:**

```bash
# List all groups
dfsctl group list

# Get group details including current members
dfsctl group get editors

# Create a group with an explicit GID
dfsctl group create --name editors --gid 1001

# Edit a group interactively
dfsctl group edit editors

# Add a user to a group
dfsctl group add-user editors alice

# Remove a user from a group
dfsctl group remove-user editors alice

# Remove a group (prompts for confirmation)
dfsctl group remove editors
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group add-user`

Add a user to a group

Add a user to a group on the DittoFS server. The user will immediately
gain any permissions associated with that group on shares and other resources.
Both the group name and the username are required positional arguments.

```
dfsctl group add-user <group> <username> [flags]
```

**Examples:**

```bash
# Add alice to the editors group
dfsctl group add-user editors alice

# Add a user to the admins group
dfsctl group add-user admins bob

# Verify membership after adding
dfsctl group get editors
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group create`

Create a new group

Create a new group on the DittoFS server. Groups are used to organise
users and can be referenced in share permissions to grant access to multiple
users at once. The group's Unix GID is auto-assigned from the server's
allocation range unless you provide one explicitly with --gid.

```
dfsctl group create [flags]
```

**Examples:**

```bash
# Create a group with an auto-assigned GID
dfsctl group create --name editors

# Create a group with an explicit GID
dfsctl group create --name editors --gid 1001

# Create a group with a description
dfsctl group create --name editors --gid 1001 --description "Content editors"
```

Flags:

```
      --description string   Group description
      --gid uint32           Group ID (auto-generated if not set)
      --name string          Group name (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group edit`

Edit a group

Edit an existing group on the DittoFS server. When run without flags, an
interactive prompt walks you through each editable field (GID and description),
showing the current value so you can press Enter to keep it. When flags are
provided, only those fields are updated and no prompt appears.

```
dfsctl group edit <name> [flags]
```

**Examples:**

```bash
# Edit the group interactively (shows current values)
dfsctl group edit editors

# Change the group's GID to a specific value
dfsctl group edit editors --gid 1002

# Update only the description
dfsctl group edit editors --description "Senior content editors"

# Update both GID and description in one command
dfsctl group edit editors --gid 1002 --description "Senior content editors"
```

Flags:

```
      --description string   Group description
      --gid uint32           Group ID
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group get`

Get group details

Get detailed information about a specific group on the DittoFS server.
Accepts either the group name or the group's full ID (see 'dfsctl group list').
The output includes the group's GID, description, member list, and creation
timestamp. Use -o json or -o yaml for machine-readable output.

```
dfsctl group get <name|id> [flags]
```

**Examples:**

```bash
# Show group details as a table
dfsctl group get editors

# By ID
dfsctl group get 7a1e9f02-...

# Output as JSON (useful for scripting)
dfsctl group get editors -o json

# Output as YAML
dfsctl group get editors -o yaml
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group list`

List all groups

List all groups registered on the DittoFS server. The table view
shows each group's name, GID, member count, and description. Use -o json or
-o yaml to get machine-readable output suitable for piping into other tools.

```
dfsctl group list [flags]
```

**Examples:**

```bash
# List all groups as a table
dfsctl group list

# Output the full group list as JSON
dfsctl group list -o json

# Output as YAML
dfsctl group list -o yaml
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl group remove`

Remove a group

Remove a group from the DittoFS server. Accepts either the group name
or the group's full ID (see 'dfsctl group list'). This action is irreversible:
the group record is permanently removed and any users that had it as their
primary group will lose that association. You will be prompted for
confirmation unless --force is specified.

```
dfsctl group remove <name|id> [flags]
```

**Examples:**

```bash
# Remove a group (prompts for confirmation)
dfsctl group remove editors

# Remove a group non-interactively (for scripts and automation)
dfsctl group remove editors --force
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

### `dfsctl group remove-user`

Remove a user from a group

Remove a user from a group on the DittoFS server. The user loses any
permissions that were derived solely from that group membership. Both the
group name and the username are required positional arguments.

```
dfsctl group remove-user <group> <username> [flags]
```

**Examples:**

```bash
# Remove alice from the editors group
dfsctl group remove-user editors alice

# Remove a user from the admins group
dfsctl group remove-user admins bob

# Verify membership after removal
dfsctl group get editors
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --case-insensitive             Fold the case of names; bound when the share is created
      --client stringArray           A client rule, CIDR=access or @netgroup=access, access none, read or read-write; the first match decides, in order
      --flavor strings               The flavours SECINFO answers, in preference order: none, sys or kerberos
      --min-proof string             The proof every caller gives: none, claimed, secret or ticket
      --min-protection string        The protection messages need: none, integrity or privacy
      --namespace string             The ID of the namespace the share's content is counted in
      --path string                  The share's path in the NFS namespace
      --read-only                    Hold every caller to read-only
      --smb-continuously-available   Make the share's SMB handles continuously available
      --smb-encrypt                  Seal every SMB message
      --smb-hidden                   Leave the share out of SMB share enumeration
      --squash string                Whose identity is squashed: none, root or all
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
  -f, --force       Delete without asking
      --id string   The share's ID, in place of its name
      --wait        Follow the operation the command starts to its end
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share export-policy set`

Write a share's whole export policy

```
dfsctl share export-policy set [share] [flags]
```

Flags:

```
      --client stringArray           A client rule, CIDR=access or @netgroup=access, access none, read or read-write; the first match decides, in order
      --flavor strings               The flavours SECINFO answers, in preference order: none, sys or kerberos
      --id string                    The share's ID, in place of its name
      --min-proof string             The proof every caller gives: none, claimed, secret or ticket
      --min-protection string        The protection messages need: none, integrity or privacy
      --read-only                    Hold every caller to read-only
      --smb-continuously-available   Make the share's SMB handles continuously available
      --smb-encrypt                  Seal every SMB message
      --smb-hidden                   Leave the share out of SMB share enumeration
      --squash string                Whose identity is squashed: none, root or all
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl share export-policy update`

Change fields of a share's export policy

```
dfsctl share export-policy update [share] [flags]
```

Flags:

```
      --client stringArray           A client rule, CIDR=access or @netgroup=access, access none, read or read-write; the first match decides, in order
      --flavor strings               The flavours SECINFO answers, in preference order: none, sys or kerberos
      --id string                    The share's ID, in place of its name
      --min-proof string             The proof every caller gives: none, claimed, secret or ticket
      --min-protection string        The protection messages need: none, integrity or privacy
      --read-only                    Hold every caller to read-only
      --smb-continuously-available   Make the share's SMB handles continuously available
      --smb-encrypt                  Seal every SMB message
      --smb-hidden                   Leave the share out of SMB share enumeration
      --squash string                Whose identity is squashed: none, root or all
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --dry-run              Print the mount command instead of running it
      --id string            The share's ID, in place of its name
      --nfs-version string   The NFS version: 3, 4, 4.0, 4.1 or 4.2 (default "3")
  -p, --protocol string      The protocol to mount over: nfs or smb
  -u, --username string      The SMB user; the current context's account when not given
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
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

### `dfsctl user`

User management

Manage local user accounts on the DittoFS server. Local users are
distinct from identities resolved via Kerberos or LDAP — they are accounts
stored in the DittoFS control plane and used for direct authentication.
Most subcommands require admin privileges; "change-password" operates on the
currently authenticated account and is available to all users.

```
dfsctl user [flags]
```

**Examples:**

```bash
# List all registered users
dfsctl user list

# Create a new user interactively
dfsctl user create

# Create a user with an explicit UID for NFS access
dfsctl user create --username alice --password secret --uid 1000 --role user

# Edit a user's group membership
dfsctl user edit alice --groups editors,viewers

# Reset a user's password as an admin
dfsctl user password alice

# Remove a user (prompts for confirmation)
dfsctl user remove alice
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user change-password`

Change your own password

Change your own password as the currently authenticated user. This is
distinct from the admin-only "dfsctl user password" command: it verifies your
current password before accepting the new one, and updates the stored session
tokens automatically. You will be prompted for both passwords unless flags are
provided (flags are less secure as passwords may appear in shell history).

```
dfsctl user change-password [flags]
```

**Examples:**

```bash
# Change password interactively (recommended — passwords are not echoed)
dfsctl user change-password

# Change password non-interactively (use with caution)
dfsctl user change-password --current oldpass --new newpass
```

Flags:

```
  -c, --current string   Current password (prompts if not provided)
  -n, --new string       New password (prompts if not provided)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user create`

Create a new user

Create a new local user on the DittoFS server. The user can be assigned
a Unix UID and primary GID for NFS uid/gid resolution; omitting these causes
the server to auto-assign them from its allocation range. If --username or
--password are not provided as flags, you will be prompted interactively.

```
dfsctl user create [flags]
```

**Examples:**

```bash
# Create a user interactively (prompted for username, password, role, groups)
dfsctl user create

# Create a regular user with username and password
dfsctl user create --username alice --password secret

# Create an admin user belonging to the editors group
dfsctl user create --username admin2 --password secret --role admin --groups editors

# Create a user whose UID/GID match the current host user (useful for NFS mounts)
dfsctl user create --username alice --password secret --host-uid --host-gid

# Create a user with an explicit UID, GID, and email
dfsctl user create --username bob --password secret --uid 1000 --gid 1001 --email bob@example.com
```

Flags:

```
      --email string      Email address
      --enabled           Enable account (default true)
      --gid uint32        Unix primary group ID (auto-assigned if not specified)
      --groups string     Comma-separated list of groups
      --host-gid          Use current host user's GID (for NFS access)
      --host-uid          Use current host user's UID (for NFS access)
  -p, --password string   Password (prompts if not provided)
      --role string       Role (user|admin) (default "user")
      --uid uint32        Unix user ID (auto-assigned if not specified)
  -u, --username string   Username (required)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user edit`

Edit a user

Edit an existing user on the DittoFS server. When run without flags, an
interactive prompt walks you through each editable field, showing the current
value so you can press Enter to keep it unchanged. When flags are provided,
only the specified fields are updated and no prompt appears.

```
dfsctl user edit <username> [flags]
```

**Examples:**

```bash
# Edit all fields interactively (shows current values)
dfsctl user edit alice

# Move alice to the admin role
dfsctl user edit alice --role admin

# Update alice's primary GID to match a new group
dfsctl user edit alice --gid 1002

# Disable an account and change its group membership in one command
dfsctl user edit alice --enabled false --groups viewers

# Update display name and email
dfsctl user edit alice --display-name "Alice Smith" --email alice@newdomain.com
```

Flags:

```
      --display-name string   Display name
      --email string          Email address
      --enabled string        Enable/disable account (true|false)
      --gid uint32            Unix primary group ID
      --groups string         Comma-separated list of groups
      --role string           Role (user|admin)
      --uid uint32            Unix user ID
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user get`

Get user details

Get detailed information about a specific user on the DittoFS server.
Accepts either the username or the user's full ID (see 'dfsctl user list').
The output includes the user's role, UID, group memberships, account status,
and last-login timestamp. Use -o json or -o yaml for machine-readable output.

```
dfsctl user get <username|id> [flags]
```

**Examples:**

```bash
# Show user details as a table
dfsctl user get alice

# By ID
dfsctl user get 3f2b1c4d-...

# Output as JSON (useful for scripting)
dfsctl user get alice -o json

# Output as YAML
dfsctl user get alice -o yaml
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user list`

List all users

List all local users registered on the DittoFS server. The table view
shows each user's username, UID, role, email, group memberships, and whether
the account is enabled. Use -o json or -o yaml to get machine-readable output
suitable for piping into other tools.

```
dfsctl user list [flags]
```

**Examples:**

```bash
# List all users as a table
dfsctl user list

# Output the full user list as JSON
dfsctl user list -o json

# Output as YAML
dfsctl user list -o yaml
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user password`

Reset a user's password

Reset a user's password (admin operation). Unlike "dfsctl user
change-password", this command does not require knowledge of the current
password and is intended for administrators recovering access for a user.
After the reset the account is marked as must-change-password, requiring
the user to set a new password on their next login.

```
dfsctl user password <username> [flags]
```

**Examples:**

```bash
# Reset a user's password interactively (password not echoed)
dfsctl user password alice

# Reset password non-interactively (use with caution)
dfsctl user password alice --password newsecret
```

Flags:

```
  -p, --password string   New password (prompts if not provided)
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl user remove`

Remove a user

Remove a user from the DittoFS server. Accepts either the username or
the user's full ID (see 'dfsctl user list'). This action is irreversible:
the account and its authentication tokens are permanently removed, though
files the user owns are not deleted. You will be prompted for confirmation
unless --force is specified.

```
dfsctl user remove <username|id> [flags]
```

**Examples:**

```bash
# Remove a user (prompts for confirmation)
dfsctl user remove alice

# Remove a user non-interactively (for scripts and automation)
dfsctl user remove alice --force
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

