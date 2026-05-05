# Security model

## Today (v0.1.0)

bffs stores account secrets in **plain TOML on disk**, in a single
file inside the user config directory.

For oauth accounts, `accounts.toml` holds *only display metadata* — the
credential itself never lives in `accounts.toml`. Each oauth account has a
session dir at `<config-dir>/sessions/<name>/`, and Claude Code stores the
credential there: in `.credentials.json` on Linux/Windows, or in the macOS
Keychain under a per-dir hashed service name (`Claude Code-credentials-<sha256>`).
The shim sets `CLAUDE_CONFIG_DIR=<sessions/name>` per-invocation so claude
reads the right credential automatically. Concurrent oauth sessions on
different accounts cannot collide.

For api_key accounts, the *secret* field is the bare `sk-ant-...` key.

Storage location:

| OS      | Path                                                           |
|---------|----------------------------------------------------------------|
| Linux   | `$XDG_CONFIG_HOME/bffs/accounts.toml` (default `~/.config/...`) |
| macOS   | `~/Library/Application Support/bffs/accounts.toml`   |
| Windows | `%AppData%\bffs\accounts.toml`                       |

Override the directory with `BFFS_HOME=<path>`.

The file is written atomically (temp file + rename) and chmod-ed to `0600` on
Unix. On Windows the default user-profile ACL applies; no extra hardening is
performed.

### What this protects against

- **Casual multi-user systems on Unix.** With `0600` the file is unreadable by
  other local users.
- **Dotfile sync.** Secrets live under the OS config dir, *not* in `~/`, so a
  naive `git add ~/.dotfiles` won't pick them up.

### What this does NOT protect against

- **Anyone with read access to your user account** (other processes you run,
  malware, an attacker with shell access as you). Plain text on disk is plain
  text.
- **Backups, snapshots, or full-disk indexers** (Time Machine, file-sync
  clients, Spotlight, etc.) — they can capture the file unredacted.
- **Windows multi-user systems** where another user is a local administrator.
- **`ps`-snooping during `claude auth login`.** Claude Code's own credential
  writes (Keychain on macOS, file elsewhere) happen inside the `claude`
  process during `bffs login`; bffs no longer shells out to `security
  add-generic-password` itself, so this is now Claude Code's concern, not
  bffs's.

If your threat model includes any of those, the v1 store is not adequate.

## Roadmap (post-v0.1.0)

The store is intentionally pluggable behind `internal/store`. The intended
follow-ups, in roughly increasing order of complexity:

1. **macOS Keychain.** Use `security add-generic-password` /
   `find-generic-password` (or the lower-level `Security.framework` via cgo).
   `accounts.toml` keeps metadata; the `secret` field becomes a Keychain
   reference.
2. **Linux libsecret.** Talk to `org.freedesktop.secrets` over D-Bus
   (godbus/dbus + the Secret Service API). Falls back to plain TOML if no
   secret service is available (headless boxes, containers).
3. **Windows DPAPI.** Encrypt the `secret` field with
   `CryptProtectData`/`CryptUnprotectData` scoped to the current user. No
   external services required.
4. **`apiKeyHelper` integration.** Have bffs register itself as
   Claude Code's `apiKeyHelper`, so even non-shim invocations route through it.
   Useful when the shim isn't installed (e.g., IDE integrations that bypass
   PATH).

Until those backends land, treat `accounts.toml` like an SSH private key:
`0600`, owned by you, not in version control, not in cloud-synced folders.
