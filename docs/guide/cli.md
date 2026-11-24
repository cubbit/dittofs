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

When keys.storage.local.file names a file that does not exist, init creates
it, a master key wrapped under DITTOFS_ENCRYPTION_PASSPHRASE, and prints once
where it is: copy it off the host, since losing it or the passphrase loses
the data. An existing file is used, never overwritten. With the passphrase
unset, init refuses.

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

Its commands for users, groups, shares and quotas return with the server's v2
API; until then each refuses. The context subcommand manages the server
contexts kept in ~/.config/dfsctl/config.json.

Use "dfsctl [command] --help" for more information about a command.

Flags:

```
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
  -v, --verbose         Enable verbose output
```

### `dfsctl login`

Authenticate with DittoFS server

Authenticate with a DittoFS server and store credentials locally.

Not available until the CLI is rebuilt on the v2 API: signing in returns with
that API's accounts. Credentials are kept in ~/.config/dfsctl/config.json under
a named context, which the context subcommand manages.

```
dfsctl login [flags]
```

**Examples:**

```bash
# First login to a local server (prompts for password)
dfsctl login --server http://localhost:8080 --username admin

# Login to a remote server whose certificate a private CA signs
dfsctl login --server https://dfs.example.com --username admin --cacert ca.pem

# Login passing password on the command line (less secure; avoid in shared environments)
dfsctl login --server http://localhost:8080 -u admin -p secret

# Re-login to the already-stored server (password prompt only)
dfsctl login
```

Flags:

```
      --cacert string     Path to a PEM CA bundle trusted for the server certificate
  -c, --context string    Context name (defaults to current or auto-generated)
  -p, --password string   Password
      --server string     Server URL (default "http://localhost:8080")
  -u, --username string   Username
```

Global flags:

```
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
  -v, --verbose         Enable verbose output
```

### `dfsctl logout`

Clear stored credentials

Clear stored credentials for the current context.

Removes the access and refresh tokens from the active context in
~/.config/dfsctl/config.json but preserves the server URL and context name so
you can re-authenticate quickly with dfsctl login. To switch between contexts
or remove a context entirely, use the dfsctl context subcommand.

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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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

# Check a specific server without logging in (token fetched from stored context)
dfsctl status --server http://dfs.example.com:8080
```

Global flags:

```
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
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
      --cacert string   Path to a PEM CA bundle trusted for the server certificate (overrides stored)
      --no-color        Disable colored output
  -o, --output string   Output format (table|json|yaml) (default "table")
      --server string   Server URL (overrides stored credential)
  -v, --verbose         Enable verbose output
```

