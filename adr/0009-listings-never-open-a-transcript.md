# Listings never open a transcript, and readers are capped

A Claude transcript is a JSONL file that grows without bound; a busy
project has hundreds of them and single lines reach multiple megabytes.
Any listing that opens them is fast on the machine it was written on and
unusable six months later. We decided that the fast path never opens a
transcript: `transcripts.List` reads the directory and the file
information, and nothing else.

What a listing still needs is a title, and Claude keeps one inside the
file. It is read from the same two windows Claude reads — 64 KiB at the
head and 64 KiB at the tail — with field-keyed last-wins semantics, and
only for the rows that are about to be drawn. The picker is a fixed
precedence (custom, ai, last prompt, summary, first prompt, history), so
two different views of the same session agree. The browser loads titles
for the visible window plus one screen either side, caches them by path,
size and modification time, and drops the result if the selection moved
on.

Every reader that does open a file is bounded: the transcript viewer
stops at 8 MiB and says so, a memory file preview at 1 MB, the usage
scan is bounded by the retention horizon and reads with a `bufio.Reader`
rather than a `Scanner`, whose default limit a single line will exceed.
Everything rendered from a transcript goes through `transcripts.Sanitize`
first, because the text came from somewhere else.

## Considered Options

- **An index or cache of parsed transcripts** — a second source of truth
  to invalidate, and Claude writes these files continuously.
- **Reading whole files for titles** — correct and slow; the windows are
  what Claude itself uses to decide what to show.
- **No titles in listings** — a list of UUIDs and timestamps, which is
  what the tool exists to improve on.

## Consequences

A title can be missing or stale in a way a full read would not be: a
session whose only title-bearing record sits in the middle of a very
large file shows its first prompt instead. That is the trade, and it is
why the picker's precedence is written down. Commands outside `bffs
usage`, `sessions` and `memory` do not parse transcripts at all, and a
new feature that wants to should reach for the same windows rather than
opening the file.
