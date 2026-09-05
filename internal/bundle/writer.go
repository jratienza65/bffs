package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"
)

// Progress is passed to the optional progress callbacks of Build and Unpack
// after every file. Phase is "build" or "unpack".
type Progress struct {
	Phase      string
	Files      int
	TotalFiles int
	Bytes      int64
	TotalBytes int64
	Current    string
}

// Opener maps a bundle path (File.Path) to a reader over its source bytes.
// Sizes come from the manifest: Build reads exactly File.Size bytes and
// ignores anything after them, so a live transcript that grew after the
// pre-pass is fine, while a source that ends early is a "short read" error.
type Opener interface {
	Open(path string) (io.ReadCloser, error)
}

// unlimited is what Build validates against: structure only, the size
// policy belongs to the receiver.
var unlimited = Limits{
	MaxTotalBytes:    math.MaxInt64,
	MaxEntryBytes:    math.MaxInt64,
	MaxManifestBytes: math.MaxInt64,
	MaxEntries:       math.MaxInt,
	MaxFiles:         math.MaxInt,
}

// Build writes a complete bundle to w: the envelope, then a PAX tar whose
// entry 0 is the manifest (marshalled once, compact) followed by every
// m.Entries[*].Files[*] in order. It returns the manifest bytes it wrote,
// so callers can hash or resend exactly what entry 0 contains. m is
// validated (structure, names, uniqueness; no size caps beyond the 16 MiB
// manifest cap) before anything is written. ctx is checked per file and on
// every read of a source; progress may be nil.
func Build(ctx context.Context, w io.Writer, m *Manifest, src Opener, comp Compression, progress func(Progress)) ([]byte, error) {
	if err := m.Validate(unlimited); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	switch comp {
	case CompNone, CompGzip:
	case CompZstd:
		return nil, fmt.Errorf("compression %d (zstd) is reserved and not implemented", comp)
	default:
		return nil, fmt.Errorf("unknown compression %d", comp)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	if int64(len(raw)) > DefaultLimits.MaxManifestBytes {
		return nil, fmt.Errorf("manifest is %d bytes; max %d", len(raw), DefaultLimits.MaxManifestBytes)
	}

	if _, err := w.Write(append([]byte(Magic), byte(comp))); err != nil {
		return nil, fmt.Errorf("write envelope: %w", err)
	}
	var (
		out io.Writer = w
		gz  *gzip.Writer
	)
	if comp == CompGzip {
		gz, err = gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			return nil, err
		}
		out = gz
	}
	tw := tar.NewWriter(out)

	if err := tw.WriteHeader(header(manifestName, int64(len(raw)), m.Created)); err != nil {
		return nil, fmt.Errorf("write %s: %w", manifestName, err)
	}
	if _, err := tw.Write(raw); err != nil {
		return nil, fmt.Errorf("write %s: %w", manifestName, err)
	}

	p := Progress{Phase: "build", TotalFiles: m.Totals.Files, TotalBytes: m.Totals.Bytes}
	for i := range m.Entries {
		for j := range m.Entries[i].Files {
			f := &m.Entries[i].Files[j]
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := writeFile(ctx, tw, f, src); err != nil {
				return nil, err
			}
			p.Files++
			p.Bytes += f.Size
			p.Current = f.Path
			if progress != nil {
				progress(p)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("finish archive: %w", err)
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return nil, fmt.Errorf("finish gzip: %w", err)
		}
	}
	return raw, nil
}

func writeFile(ctx context.Context, tw *tar.Writer, f *File, src Opener) error {
	if err := tw.WriteHeader(header(f.Path, f.Size, f.ModTime)); err != nil {
		return fmt.Errorf("write header for %q: %w", f.Path, err)
	}
	rc, err := src.Open(f.Path)
	if err != nil {
		return fmt.Errorf("open %q: %w", f.Path, err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(&ctxReader{ctx: ctx, r: rc}, f.Size))
	if err != nil {
		return fmt.Errorf("copy %q: %w", f.Path, err)
	}
	if n != f.Size {
		return fmt.Errorf("short read on %q: manifest says %d bytes, source had %d", f.Path, f.Size, n)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%q changed during export: sha256 %s, manifest says %s", f.Path, got, f.SHA256)
	}
	return nil
}

// ctxReader fails the next Read once ctx is done, so a copy of one large
// file is cancellable, not only the gap between files.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// header is the one tar.Header shape Build emits. A zero mtime becomes the
// Unix epoch so the ustar field always formats.
func header(name string, size int64, mtime time.Time) *tar.Header {
	if mtime.IsZero() {
		mtime = time.Unix(0, 0).UTC()
	}
	return &tar.Header{
		Format:   tar.FormatPAX,
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     size,
		Mode:     0o600,
		ModTime:  mtime,
	}
}
