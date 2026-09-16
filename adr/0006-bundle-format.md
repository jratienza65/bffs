# A transfer is one `.bffs` file whose manifest is the allow-list

Moving sessions between machines means moving a set of files that a
program on the other side will read as its own: transcripts, sidecar
directories, file history, auto-memory. The format therefore has to be
inspectable before anything is written, and the writer has to be unable
to reach outside where it was told to write. We decided on a single
file: a short envelope (`BFFS\x01` plus one compression byte), then a
PAX tar whose first entry is a JSON manifest that lists every following
entry with its kind, its name in a fixed grammar, its size and its
sha256.

The manifest is not a description of the archive, it is the allow-list
for unpacking it. `bundle.Unpack` refuses an entry the manifest does not
list, bounds the stream to what the manifest implies, writes only
through an `os.OpenRoot` over a staging directory that must not already
exist, never deletes, and applies a modification time only when it falls
inside `[2020-01-01, now+24h]`. Names are validated by
`ValidateEntryName` before they are joined to anything, and a name of a
kind this version does not know is drained into a note rather than
written. The receiving side compares the manifest it showed the user
against the one it committed (`ExpectManifestSHA256`), so what was
reviewed is what lands.

## Considered Options

- **A zip of the project directory** — no place for a manifest that is
  read first, and every reader would have to trust the entry names.
- **rsync or scp of `projects/<slug>/`** — no identity, no integrity,
  no review step, and it silently overwrites what is already there.
- **Trusting the tar reader** — `archive/tar` will happily hand you
  `../../..`; the grammar plus `os.Root` is what makes that a refusal
  rather than a code review.

## Consequences

Everything that crosses a machine boundary goes through the same door,
so the LAN transfer, the file export and the same-machine copy share one
writer and one reader. The manifest's strings — cwd, project key, home,
hostname — are compared and printed but never joined into a path; every
suffix bffs creates derives from the bundle id. `internal/bundle` stays
a standard-library leaf so the fuzz targets are cheap to run, and its
copy of the reserved-name list is pinned equal to `transcripts`' by a
test, because the two drifting apart is how a reserved directory becomes
a writable one.
