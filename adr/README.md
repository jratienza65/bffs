# Architecture decision records

One page per decision that is load-bearing: what forced it, what was
decided, what that rules out, and what it costs. They are numbered in
the order they were written down, not in the order they were made — most
of these were made long before they were recorded.

A record is here because changing it would change several files and
surprise someone. Anything that can be re-derived from the code (how a
function works, which package calls which) belongs in the code. The plan
documents under `docs/` are local to a machine and disappear when the
plan is finished; these do not, which is the point.

Write one when a choice closes a door: new file `NNNN-<slug>.md`, the
title as a sentence, the forces and the decision in prose, then
**Considered Options** with the reason each was not taken, then
**Consequences** — what someone will run into later because of this.

| # | Decision |
| --- | --- |
| [0001](0001-multi-call-binary.md) | The `claude` shim is the bffs binary under another name |
| [0002](0002-account-resolution-precedence.md) | Account resolution is a fixed precedence, and the config walk is bounded |
| [0003](0003-per-invocation-env-injection.md) | Accounts are selected by env vars on one child process |
| [0004](0004-isolation-presets.md) | An oauth account isolates identity, and symlinks the rest by default |
| [0005](0005-trust-sync-never-downgrades.md) | Trust sync only ever adds an answer |
| [0006](0006-bundle-format.md) | A transfer is one `.bffs` file whose manifest is the allow-list |
| [0007](0007-rehome-is-a-move-with-a-record.md) | Rehoming moves a session and records that it moved |
| [0008](0008-the-browser-never-deletes.md) | The browser never deletes |
| [0009](0009-listings-never-open-a-transcript.md) | Listings never open a transcript, and readers are capped |
