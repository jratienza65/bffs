package bundle

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Unpacked is what Unpack leaves in stagingDir: the parsed manifest, its
// exact bytes, and every listed file that was written, keyed by bundle
// path. Reserved-kind entries are drained rather than written; each is
// reported in Notes and does not appear in Files.
type Unpacked struct {
	Manifest    *Manifest
	RawManifest []byte
	StagingDir  string
	Files       map[string]File
	Notes       []string
}

const (
	envelopeLen = len(Magic) + 1
	// totalSlack is how far the running total may exceed totals.bytes
	// before the archive is refused (belt-and-braces: with the per-entry
	// size check it cannot exceed it at all).
	totalSlack = 1 << 20
	// trailerSlack is how much a gzip member may carry after the tar
	// end-of-archive marker (tar writers pad to a record boundary).
	trailerSlack    = 64 << 10
	paxSparsePrefix = "GNU.sparse."
	// headerAllowance is how many tar bytes one entry may spend on headers:
	// a ustar block, a PAX extended-header block and its records (path,
	// mtime, size) padded to a block come to under 3 KiB for the longest
	// name the grammar allows. Unpack refuses to read more tar bytes than
	// the manifest implies at this rate, so a stream of bare PAX headers (a
	// header bomb that costs the reader CPU but never a listed file) stops
	// early instead of running to the end of the input.
	headerAllowance = 4 << 10
	// endOfArchive is the two zero blocks that close a tar stream.
	endOfArchive = 1024
)

var errTarCap = errors.New("archive carries more tar bytes than the manifest implies")

var (
	// skipNameCheck bypasses ValidateEntryName, ClassifyName and the
	// allow-list in Unpack so tests can prove os.Root alone still contains a
	// hostile name. Never set outside tests.
	skipNameCheck bool
	// nowFn is the clock for the mtime window; a var so tests can pin it.
	nowFn = time.Now
	// syncFile flushes a staged file before it is closed, like fsutil does.
	// FuzzUnpack stubs it: its invariants are about names and bytes, and an
	// F_FULLFSYNC per file starves the fuzz minimizer.
	syncFile = (*os.File).Sync
	// mtimeFloor is the oldest mtime Unpack will apply to a staged file.
	mtimeFloor = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
)

// PeekManifest reads the envelope and tar entry 0 from r and returns the
// parsed manifest, its exact bytes and a reader that replays everything
// consumed followed by the rest of r — hand rest to Unpack to read the same
// stream from the start. Nothing is validated beyond the envelope and the
// entry-0 shape (name, type, DefaultLimits.MaxManifestBytes); call
// Manifest.Validate before trusting any field.
func PeekManifest(r io.Reader) (*Manifest, []byte, io.Reader, error) {
	rec := &recorder{r: r}
	br := bufio.NewReader(rec)
	comp, err := readEnvelope(br)
	if err != nil {
		return nil, nil, nil, err
	}
	payload, _, err := openPayload(br, comp)
	if err != nil {
		return nil, nil, nil, err
	}
	cr := &cappedReader{ctx: context.Background(), r: payload, limit: manifestCap(DefaultLimits.MaxManifestBytes)}
	raw, err := readManifestEntry(tar.NewReader(cr), DefaultLimits.MaxManifestBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	m, err := decodeManifest(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	return m, raw, io.MultiReader(bytes.NewReader(rec.buf), r), nil
}

// Unpack reads a bundle from r and stages every listed file under
// stagingDir (which must exist and be empty), verifying each against the
// manifest allow-list. expectSHA256, when not empty, must equal the
// lowercase hex sha256 of tar entry 0. l caps the manifest and the staged
// bytes. requireEOF additionally demands that r end right after the
// archive. progress may be nil; ctx is checked per entry and on every read.
//
// On error nothing is removed: the caller owns stagingDir.
func Unpack(ctx context.Context, r io.Reader, stagingDir string, expectSHA256 string, l Limits, requireEOF bool, progress func(Progress)) (*Unpacked, error) {
	if err := checkEmptyDir(stagingDir); err != nil {
		return nil, err
	}
	br := bufio.NewReader(r)
	comp, err := readEnvelope(br)
	if err != nil {
		return nil, err
	}
	payload, gz, err := openPayload(br, comp)
	if err != nil {
		return nil, err
	}
	cr := &cappedReader{ctx: ctx, r: payload, limit: manifestCap(l.MaxManifestBytes)}
	tr := tar.NewReader(cr)

	raw, err := readManifestEntry(tr, l.MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	if expectSHA256 != "" {
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, expectSHA256) {
			return nil, fmt.Errorf("manifest sha256 %s does not match the expected %s", got, expectSHA256)
		}
	}
	m, err := decodeManifest(raw)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(l); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}

	allow := make(map[string]*File)
	dirs := make(map[string]bool)
	// The tar stream may be as long as the manifest implies and no longer:
	// every listed file with its data and header allowance, one allowance
	// per implied directory, the manifest itself and the end marker.
	tarCap := satAdd(blocks(int64(len(raw))), headerAllowance)
	for i := range m.Entries {
		for j := range m.Entries[i].Files {
			f := &m.Entries[i].Files[j]
			allow[f.Path] = f
			tarCap = satAdd(tarCap, satAdd(blocks(f.Size), headerAllowance))
			for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
				if !dirs[d] {
					dirs[d] = true
					tarCap = satAdd(tarCap, headerAllowance)
				}
			}
		}
	}
	cr.limit = satAdd(tarCap, endOfArchive)

	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return nil, fmt.Errorf("open staging dir: %w", err)
	}
	defer root.Close()

	u := &Unpacked{Manifest: m, RawManifest: raw, StagingDir: stagingDir, Files: make(map[string]File)}
	st := &unpackState{
		u:     u,
		root:  root,
		allow: allow,
		dirs:  dirs,
		seen:  make(map[string]bool),
		l:     l,
		now:   nowFn(),
		prog:  Progress{Phase: "unpack", TotalFiles: len(allow), TotalBytes: m.Totals.Bytes},
		notif: progress,
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if err := st.entry(tr, hdr); err != nil {
			return nil, err
		}
	}

	if st.arrived != len(allow) {
		return nil, fmt.Errorf("bundle is incomplete: %s", st.missing())
	}
	if gz != nil {
		n, err := io.Copy(io.Discard, io.LimitReader(gz, trailerSlack+1))
		if err != nil {
			return nil, fmt.Errorf("gzip stream: %w", err)
		}
		if n > trailerSlack {
			return nil, fmt.Errorf("gzip stream carries more than %d bytes after the end of the archive", trailerSlack)
		}
	}
	if requireEOF {
		if _, err := br.ReadByte(); err == nil {
			return nil, fmt.Errorf("trailing data after the end of the archive")
		} else if !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	return u, nil
}

type unpackState struct {
	u       *Unpacked
	root    *os.Root
	allow   map[string]*File
	dirs    map[string]bool
	seen    map[string]bool
	l       Limits
	now     time.Time
	arrived int
	total   int64
	prog    Progress
	notif   func(Progress)
}

func (st *unpackState) entry(tr *tar.Reader, hdr *tar.Header) error {
	name := hdr.Name
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeDir:
	default:
		return fmt.Errorf("entry %q has type %q; only regular files and directories are allowed", name, hdr.Typeflag)
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, paxSparsePrefix) {
			return fmt.Errorf("entry %q is a sparse file, which bundles never contain", name)
		}
	}
	if hdr.Size < 0 {
		return fmt.Errorf("entry %q has a negative size", name)
	}

	if hdr.Typeflag == tar.TypeDir {
		if hdr.Size != 0 {
			return fmt.Errorf("directory entry %q has a size", name)
		}
		if !skipNameCheck {
			if err := ValidateEntryName(name); err != nil {
				return err
			}
			if !st.dirs[name] {
				return fmt.Errorf("directory entry %q is not an ancestor of any listed file", name)
			}
		}
		lower := strings.ToLower(name)
		if st.seen[lower] {
			return fmt.Errorf("duplicate entry %q", name)
		}
		st.seen[lower] = true
		if err := st.root.MkdirAll(name, 0o700); err != nil {
			return fmt.Errorf("create %q: %w", name, err)
		}
		return nil
	}

	var (
		want *File
		kind string
	)
	if !skipNameCheck {
		var err error
		if kind, _, _, err = ClassifyName(name); err != nil {
			return err
		}
	}
	lower := strings.ToLower(name)
	if st.seen[lower] {
		return fmt.Errorf("duplicate entry %q", name)
	}
	st.seen[lower] = true
	want = st.allow[name]
	if want == nil && !skipNameCheck {
		return fmt.Errorf("entry %q is not listed in the manifest", name)
	}
	if want != nil && hdr.Size != want.Size {
		return fmt.Errorf("entry %q is %d bytes in the archive but %d in the manifest", name, hdr.Size, want.Size)
	}
	if hdr.Size > st.l.MaxEntryBytes {
		return fmt.Errorf("entry %q is %d bytes; max %d", name, hdr.Size, st.l.MaxEntryBytes)
	}
	st.total += hdr.Size
	if st.total > st.u.Manifest.Totals.Bytes+totalSlack {
		return fmt.Errorf("archive exceeds the manifest total of %d bytes at entry %q", st.u.Manifest.Totals.Bytes, name)
	}

	if kind == NameReserved {
		n, err := io.Copy(io.Discard, io.LimitReader(tr, hdr.Size))
		if err != nil {
			return fmt.Errorf("reading %q: %w", name, err)
		}
		if n != hdr.Size {
			return fmt.Errorf("entry %q ended after %d of %d bytes", name, n, hdr.Size)
		}
		st.u.Notes = append(st.u.Notes, fmt.Sprintf("skipped reserved entry %q (this bffs does not import it)", name))
		st.arrived++
		st.progress(hdr)
		return nil
	}

	if dir := path.Dir(name); dir != "." {
		if err := st.root.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create directory for %q: %w", name, err)
		}
	}
	f, err := st.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %q: %w", name, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(tr, hdr.Size))
	if err != nil {
		f.Close()
		return fmt.Errorf("writing %q: %w", name, err)
	}
	if n != hdr.Size {
		f.Close()
		return fmt.Errorf("entry %q ended after %d of %d bytes", name, n, hdr.Size)
	}
	if err := syncFile(f); err != nil {
		f.Close()
		return fmt.Errorf("sync %q: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", name, err)
	}
	if want != nil {
		if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 {
			return fmt.Errorf("entry %q has sha256 %s; manifest says %s", name, got, want.SHA256)
		}
	}
	if mt := hdr.ModTime; !mt.Before(mtimeFloor) && !mt.After(st.now.Add(24*time.Hour)) {
		if err := st.root.Chtimes(name, mt, mt); err != nil {
			return fmt.Errorf("set mtime on %q: %w", name, err)
		}
	}
	if want != nil {
		st.u.Files[name] = *want
		st.arrived++
	}
	st.progress(hdr)
	return nil
}

func (st *unpackState) progress(hdr *tar.Header) {
	if st.notif == nil {
		return
	}
	st.prog.Files = st.arrived
	st.prog.Bytes = st.total
	st.prog.Current = hdr.Name
	st.notif(st.prog)
}

// missing describes which listed files never arrived (first few, sorted).
func (st *unpackState) missing() string {
	var names []string
	for p := range st.allow {
		if !st.seen[strings.ToLower(p)] {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	n := len(names)
	if n > 5 {
		names = names[:5]
	}
	return fmt.Sprintf("%d of %d listed files did not arrive (%s)", n, len(st.allow), strings.Join(quoteAll(names), ", "))
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}

func checkEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("staging dir: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("staging dir %q is not empty", dir)
	}
	return nil
}

func readEnvelope(r io.Reader) (Compression, error) {
	var env [envelopeLen]byte
	if _, err := io.ReadFull(r, env[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fmt.Errorf("not a bffs bundle: stream shorter than the %d-byte envelope", envelopeLen)
		}
		return 0, fmt.Errorf("read envelope: %w", err)
	}
	if string(env[:len(Magic)]) != Magic {
		return 0, fmt.Errorf("not a bffs bundle: bad magic %q", env[:len(Magic)])
	}
	comp := Compression(env[len(Magic)])
	switch comp {
	case CompNone, CompGzip:
		return comp, nil
	case CompZstd:
		return 0, fmt.Errorf("unsupported compression %d; upgrade bffs", comp)
	default:
		return 0, fmt.Errorf("unknown compression %d", comp)
	}
}

// openPayload returns the tar byte stream for comp. br implements
// io.ByteReader, so gzip reads exactly its member and never past it.
func openPayload(br *bufio.Reader, comp Compression) (io.Reader, *gzip.Reader, error) {
	if comp != CompGzip {
		return br, nil, nil
	}
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, nil, fmt.Errorf("gzip: %w", err)
	}
	gz.Multistream(false)
	return gz, gz, nil
}

// readManifestEntry requires tar entry 0 to be manifest.json, a regular
// file of at most maxBytes, and returns its bytes.
func readManifestEntry(tr *tar.Reader, maxBytes int64) ([]byte, error) {
	hdr, err := tr.Next()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("archive is empty; %s must be entry 0", manifestName)
	}
	if err != nil {
		return nil, fmt.Errorf("reading archive: %w", err)
	}
	if hdr.Name != manifestName {
		return nil, fmt.Errorf("first entry is %q; %s must be entry 0", hdr.Name, manifestName)
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("%s has type %q; must be a regular file", manifestName, hdr.Typeflag)
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, paxSparsePrefix) {
			return nil, fmt.Errorf("%s is a sparse file", manifestName)
		}
	}
	if hdr.Size < 0 || hdr.Size > maxBytes {
		return nil, fmt.Errorf("%s is %d bytes; max %d", manifestName, hdr.Size, maxBytes)
	}
	raw := make([]byte, hdr.Size)
	if _, err := io.ReadFull(tr, raw); err != nil {
		return nil, fmt.Errorf("reading %s: %w", manifestName, err)
	}
	return raw, nil
}

func decodeManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", manifestName, err)
	}
	return &m, nil
}

// cappedReader is the reader the tar parser sees: it fails once ctx is
// done (so a copy of one large entry is cancellable) and once more than
// limit bytes have been read (the header-bomb bound).
type cappedReader struct {
	ctx   context.Context
	r     io.Reader
	n     int64
	limit int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	if c.n >= c.limit {
		return 0, errTarCap
	}
	if rem := c.limit - c.n; int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// manifestCap is the tar bytes entry 0 may span: its header allowance plus
// its data padded to a block.
func manifestCap(maxManifest int64) int64 {
	return satAdd(blocks(maxManifest), headerAllowance)
}

// blocks rounds n up to the tar block size, saturating.
func blocks(n int64) int64 {
	if n < 0 {
		return 0
	}
	return satAdd(n, 511) &^ 511
}

func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// recorder captures every byte read through it so PeekManifest can hand
// back a reader that replays the consumed prefix.
type recorder struct {
	r   io.Reader
	buf []byte
}

func (rec *recorder) Read(p []byte) (int, error) {
	n, err := rec.r.Read(p)
	rec.buf = append(rec.buf, p[:n]...)
	return n, err
}
