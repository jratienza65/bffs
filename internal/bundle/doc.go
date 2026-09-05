// Package bundle defines the .bffs bundle format — the single byte stream
// bffs uses to move Claude Code sessions and auto-memory between accounts,
// machines and roots — and the two operations on it: Build (write) and
// Unpack (read into a staging directory with every byte verified).
//
// The same bytes are written to a file (bffs export --out), to stdout
// (--out -), over the LAN (the transfer body) and through an io.Pipe for a
// same-machine copy; one reader serves all of them.
//
// # Envelope
//
//	offset 0   "BFFS\x01"   Magic: four ASCII bytes plus the envelope version
//	offset 5   1 byte       compression: 0 none, 1 gzip (BestSpeed), 2 zstd (reserved)
//	offset 6…  tar stream   PAX headers (tar.FormatPAX) for every entry
//
// The payload is self-delimiting: a gzip member (read with Multistream(false))
// or, uncompressed, the tar end-of-archive marker. Unpack with requireEOF
// rejects any byte after that; on the wire the connection stays open for the
// transfer protocol's own trailer, so the caller passes requireEOF=false and
// completeness comes from the manifest allow-list instead.
//
// Compression 2 is reserved for zstd: the reader reports
// "unsupported compression 2; upgrade bffs" so an older bffs fails clearly
// once a newer one starts emitting it.
//
// # Tar layout — the namespace grammar (ClassifyName)
//
//	manifest.json                                              MUST be entry 0, TypeReg, ≤ Limits.MaxManifestBytes
//	projects/<slug>/<sid>.jsonl                                kind "transcript"
//	projects/<slug>/<sid>/(subagents|workflows|tool-results|
//	                       remote-agents|mcp-tasks)/**          kind "sidecar"
//	projects/<slug>/<sid>/*.cast | custom-title.json |
//	                       .ccr-tip.json | .precompact.json |
//	                       sent-prefix.json                     kind "sidecar"
//	file-history/<sid>/<name>   <name> = ^[0-9a-f]{16}@v[0-9]+$  kind "file-history"
//	plans/<planSlug>.md | plans/<planSlug>-agent-<id>.md |
//	plans/<planSlug>.workshop.md                                kind "plans"
//	history/<sid>.jsonl                                        kind "history"
//	tasks/<sid>/**              no ".lock" component            kind "tasks"
//	memory/<slug>/**.md         no proposals/ directory, no
//	                            component starting with "index"  kind "memory"
//	claudejson/<slug>.json | session-env/<sid>/** | user-memory/**
//	                            RESERVED: grammar accepts, Build never emits,
//	                            Unpack drains without writing and records a note
//
// where <slug> matches ^[A-Za-z0-9_-]{1,240}$ and is not one of Claude's
// reserved projects/ entries (memory, tiny_memory, bagel, cloud-snapshots,
// bridge-pointer.json, .session-aliases — compared case-insensitively),
// <sid> is a lowercase UUID and <planSlug> matches ^[a-z0-9-]{1,80}$.
//
// Every name additionally passes ValidateEntryName: every byte in
// [A-Za-z0-9._@/-] (so no backslash, NUL, space or non-ASCII byte — Unicode
// look-alikes are rejected), filepath.IsLocal, path.Clean(name) == name, no
// leading "/", no empty, "." or ".." component, no Windows device name as a
// component (NUL, CON, COM1, … on every OS, so a bundle written on macOS
// unpacks on Windows), no component ending in "." (Windows strips trailing
// dots), each component ≤ 255 bytes, the whole name ≤ 1024 bytes. Paths
// must be unique after strings.ToLower and no path may be a directory of
// another, because macOS and Windows filesystems fold case and a file
// cannot also be a directory anywhere. Anything outside the grammar rejects the
// whole bundle; the writer side (porter.BuildManifest) runs the same checks
// on every candidate and skips out-of-grammar files with a warning so a
// bundle never fails only on the receiving machine.
//
// Directories are implicit: Build never writes TypeDir entries and Unpack
// creates parents with MkdirAll. A TypeDir entry is tolerated only when it is
// an ancestor directory of a listed file, named without a trailing slash,
// and appears at most once.
//
// # manifest.json (format 1)
//
// Manifest, marshalled once with compact json.Marshal (snake_case keys, see
// the struct tags). The bytes Build returns are exactly entry 0, so a
// transfer can send sha256(manifest) ahead of the body and Unpack can require
// entry 0 to hash to it byte-for-byte. Unknown fields are ignored on decode.
//
// entries[*].files[*] is the allow-list: every tar entry after manifest.json
// MUST be listed exactly once with matching size and sha256. Unlisted,
// duplicated (also case-insensitively), mis-sized or mis-hashed entries
// reject the bundle, as does a listed file that never arrives.
//
// Source paths (cwd, project_key, home, config_dir, root_dir) are
// informational: they are compared and printed (sanitised by the caller),
// never joined into a local path. source.hostname is required and
// hostname/user/account must match ^[A-Za-z0-9_.-]{1,64}$ (user and account
// may be empty). Every on-disk suffix bffs derives from a bundle comes from
// bundle_id[:8], which Validate guarantees is a lowercase UUID.
//
// # Writer rules (Build)
//
// Header{Format: PAX, Typeflag: TypeReg, Mode: 0600, Uid/Gid: 0, Uname/Gname:
// "", ModTime: File.ModTime}; no xattrs, no extra PAX records. Each file is
// copied through io.LimitReader(File.Size) from the Opener, so a live
// transcript that grew after the manifest pre-pass contributes exactly the
// bytes the manifest describes; a source shorter than File.Size is a
// "short read" error and the export fails before the receiver commits
// anything. The streamed bytes are hashed and compared against File.SHA256
// so a file rewritten between pre-pass and write fails on the sender, not
// the receiver. ctx is checked per file and on every read of a source.
//
// # Reader rules (Unpack)
//
// The staging directory must exist and be empty; every write goes through
// os.OpenRoot(stagingDir), the belt-and-braces against a name check bypass:
// os.Root refuses traversal and out-of-root symlinks on its own. Per header:
// Typeflag ∈ {TypeReg, TypeDir}; no GNU.sparse PAX records; ValidateEntryName;
// ClassifyName; listed with the same size; unseen (case-insensitively, files
// and directory entries alike); ≤ Limits.MaxEntryBytes; running total ≤
// totals.bytes + 1 MiB; root.MkdirAll(dir, 0700);
// root.OpenFile(O_WRONLY|O_CREATE|O_EXCL, 0600); copy through
// io.LimitReader(hdr.Size) with a sha256 tee; n == size and digest ==
// manifest. Mode, Uid/Gid, Uname/Gname, PAXRecords and Xattrs are ignored;
// ModTime is applied only when it lies in [2020-01-01, now+24h].
// Reserved-kind entries are drained, never written, and reported in
// Unpacked.Notes. The tar stream as a whole may not exceed what the manifest
// implies (each listed file's data padded to a block plus a 4 KiB header
// allowance per file and per implied directory, the manifest, the end
// marker), which bounds the work a stream of bare PAX headers can cause.
// After the archive: every listed file arrived, the gzip member ends (CRC
// verified) within 64 KiB of the end-of-archive marker, and with requireEOF
// the underlying reader is at io.EOF. ctx is checked per entry and on every
// read.
//
// Unpack never deletes: on error the caller owns cleanup of stagingDir (it is
// the caller's directory and may be journalled by the import pipeline).
//
// A staged transcript's head sessionId is NOT checked against its file
// name here — that needs transcripts.ReadHead and is porter's job (plan
// §9.1 step 5).
//
// # Limits
//
// Limits caps what a manifest may declare and what Unpack will stage. A zero
// field allows nothing (fail closed): start from DefaultLimits and override.
// Build validates the manifest structurally with no size caps except the
// 16 MiB manifest cap, because the size policy belongs to the receiver
// (--max-size).
//
// Dependency rule: bundle imports only the standard library (it may use
// internal/fsutil) — never transcripts, rehome, store or the network.
package bundle
