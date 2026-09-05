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

## Transfer between machines

`bffs export --serve` / `bffs import --from <host>` move one bundle between
two machines on the same local network, gated by an eight-character pairing
code (40 bits from `crypto/rand`) shown on the serving side and typed on the
receiving side. The code never crosses the wire. Each connection is TLS 1.3
with an ephemeral self-signed certificate; both sides export keying material
from that exact connection (RFC 5705), derive `K = argon2id(code, salt =
exporter, t=3, m=64 MiB)`, and exchange HMACs over the exporter — the
receiver proves first, the server proves back, and only then is the manifest
shown and the body streamed. Listeners bind only on-link addresses (private
or link-local, never `0.0.0.0`), and the receiver refuses to dial anything
outside its own interfaces' prefixes before a byte is sent.

| Threat | Handling | Residual risk |
|---|---|---|
| Passive sniffing | TLS 1.3; the code never crosses the wire; proofs are HMACs over channel-bound material | none practical |
| Active MITM / relay | Proofs include the exporter of *this* connection; a relay has different exporters on its two legs; `K` is salted with the exporter | **whoever intercepts B's connection gets one 2^40 argon2id target per intercepted connection, no precomputation, useless once the code's TTL passes.** `--from <ip>` narrows "intercept" to ARP/ND spoofing; a name lookup that any LAN host can answer widens it |
| Stranger guessing at A | one proof per connection, three failures per serve, ten-minute TTL, 40-bit code → at most 3·2^-40 | none practical |
| Fake A pushing a bundle | B verifies A's proof before any payload; the bundle is untrusted input regardless (see below) | none |
| Malicious frames B→A | A reads at most 4 KiB of JSON per frame with a strict schema and never writes files from B's input | none |
| Denial of service on A | at most eight concurrent handshakes (10 s each), proof verification serialised, only well-formed hellos burn attempts, every peer logged with its outcome | **a hostile host on the LAN can deny (not break) a transfer**; the file and ssh fallbacks exist |
| Denial of service on B | manifest totals and `--max-size` checked before staging, per-entry limits, decompressed-byte counter, entry and file caps, read deadlines | none |
| Code leakage on the host | never on argv, in logs, in mDNS or in any event; prompted with echo off; `$BFFS_TRANSFER_CODE` is read once and cleared; the code type has no `String` | **anyone who can see A's screen within the TTL can pull the bundle**; shell history keeps the variable if the user exports it |
| Identity leakage before pairing | the hello carries no hostname, user or version; B's identity travels only after A's proof is verified | none |
| Crafted bundle content | the transport is content-agnostic; import validates every name and digest in a staging directory, neutralises `pinned:` memory frontmatter, discloses memory before asking, and never merges memory from the MCP tools | **imported memory, plans and transcripts are untrusted prompt content**; memory files will be loaded into every future session for that directory |

Two further details worth knowing. When the receiver's own import fails
mid-stream, it reports the failure to A and then lingers for up to five
seconds before closing, so A can read the report instead of a reset; nothing
is drained beyond that wait and nothing is written. And a name (`mac-a`,
`mac-a.local`) is resolved once, every address is checked, and the literal
is dialled — but the resolution itself is only as trustworthy as the
network's mDNS or DNS, which is why the banner shows an IPv4 address.

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
