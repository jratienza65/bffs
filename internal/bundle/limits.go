package bundle

// Limits caps what a manifest may declare and what Unpack will stage.
//
// A zero field allows nothing: Limits{} rejects every non-empty manifest,
// so a caller that forgets to set limits fails loudly instead of silently
// dropping the caps. Start from DefaultLimits and override what the user
// asked for (--max-size raises MaxTotalBytes).
type Limits struct {
	MaxTotalBytes    int64 // totals.bytes (sum of every listed file)
	MaxEntryBytes    int64 // one file
	MaxManifestBytes int64 // manifest.json (tar entry 0)
	MaxEntries       int   // manifest entries (sessions + memories)
	MaxFiles         int   // files across every entry
}

// DefaultLimits are the caps bffs applies unless the user overrides them:
// 2 GiB per bundle, 512 MiB per file, 16 MiB manifest (also the transfer
// frame cap), 10 000 entries, 100 000 files.
var DefaultLimits = Limits{
	MaxTotalBytes:    2 << 30,
	MaxEntryBytes:    512 << 20,
	MaxManifestBytes: 16 << 20,
	MaxEntries:       10_000,
	MaxFiles:         100_000,
}

// Compression is the envelope's compression byte.
type Compression byte

const (
	// CompNone is a plain tar stream.
	CompNone Compression = 0
	// CompGzip is compress/gzip at BestSpeed, one member.
	CompGzip Compression = 1
	// CompZstd is reserved for a future zstd payload. Build refuses it and
	// Unpack reports "unsupported compression 2; upgrade bffs".
	CompZstd Compression = 2
)

const (
	// Magic opens every bundle: the ASCII tag plus the envelope version.
	Magic = "BFFS\x01"
	// Ext is the conventional file extension.
	Ext = ".bffs"
	// FormatVersion is the manifest format this package reads and writes.
	FormatVersion = 1
)
