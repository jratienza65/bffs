# The `claude` shim is the bffs binary under another name

bffs works by getting in front of `claude` on `PATH`: something has to
resolve the account, set one environment variable and exec the real
binary. That something could be a shell script, a second Go binary, or
the bffs binary itself. We decided on the last: `main.go` inspects
`argv[0]`, and a process invoked as `claude` runs `internal/shim.Run`
instead of the Cobra root command. `bffs init` installs it by symlinking
(or hardlinking, or wrapping in a small `sh` script where neither is
available) `<install-dir>/claude` at the bffs binary.

One binary means the shim can never be a version behind the CLI that
installed it, there is nothing to keep in sync, and `bffs init` has one
artefact to check. What it rules out is a cheap shim: every package
linked into bffs pays its `init` on every `claude` launch, including the
launches that have nothing to do with bffs.

## Considered Options

- **A shell script** — portable and tiny, but it cannot read `accounts.toml`,
  resolve a project config or take the Keychain path without calling a
  binary anyway, and it would be a second implementation of the
  resolution order.
- **A separate shim binary** — no init cost from the CLI's dependencies,
  but two artefacts to install, version and repair, and the resolution
  order implemented twice or exported through a third package.
- **A shell function or alias** — invisible to anything that does not go
  through an interactive shell, which is most of what runs `claude`.

## Consequences

The shim's launch cost is a budget, not an afterthought: `make
bench-shim` must stay within +10 % p50 or +5 ms, and a dependency that
builds tables in `init` is a defect. Two have already been caught this
way — `go-runewidth` v0.0.27 cost ~16 ms per launch and is pinned at
≥ v0.0.29, and Glamour cost +6.8 ms (chroma's lexer and style
registries, bluemonday), which is why the browser renders Markdown
itself. `internal/shim/graph_test.go` pins the shim's dependency set so
a stray import is a test failure rather than a slow launch. The escape
hatch, if the budget is ever lost for good, is the `bffs_notui` build
tag: it drops the charm stack and makes bare `bffs` print help.
