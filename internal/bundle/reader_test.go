package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func readStaged(tb testing.TB, staging, name string) []byte {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(name)))
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	m, src := sampleManifest()
	for _, comp := range []Compression{CompNone, CompGzip} {
		t.Run(map[Compression]string{CompNone: "none", CompGzip: "gzip"}[comp], func(t *testing.T) {
			data, raw := build(t, m, src, comp)
			var calls []Progress
			outer, staging := stage(t)
			u, err := Unpack(context.Background(), bytes.NewReader(data), staging, sha(raw), DefaultLimits, true, func(p Progress) { calls = append(calls, p) })
			if err != nil {
				t.Fatalf("Unpack: %v", err)
			}
			if !bytes.Equal(u.RawManifest, raw) || u.Manifest.BundleID != bundleID || u.StagingDir != staging || len(u.Notes) != 0 {
				t.Fatalf("Unpacked = %+v", u)
			}
			if len(calls) != m.Totals.Files || calls[len(calls)-1].Files != m.Totals.Files || calls[len(calls)-1].Bytes != m.Totals.Bytes || calls[0].Phase != "unpack" {
				t.Fatalf("progress calls = %+v", calls)
			}
			var want []string
			for _, e := range m.Entries {
				for _, f := range e.Files {
					want = append(want, f.Path)
					got := readStaged(t, staging, f.Path)
					if !bytes.Equal(got, src[f.Path]) {
						t.Errorf("%s: staged %q, want %q", f.Path, got, src[f.Path])
					}
					if sha(got) != f.SHA256 || u.Files[f.Path] != f {
						t.Errorf("%s: digest or Files mismatch", f.Path)
					}
					info, err := os.Stat(filepath.Join(staging, filepath.FromSlash(f.Path)))
					if err != nil {
						t.Fatal(err)
					}
					if got, want := info.ModTime().Truncate(time.Second), f.ModTime.Truncate(time.Second); !got.Equal(want) {
						t.Errorf("%s: mtime %v, want %v", f.Path, got, want)
					}
					if runtime.GOOS != "windows" {
						if perm := info.Mode().Perm(); perm != 0o600 {
							t.Errorf("%s: perm %o, want 0600", f.Path, perm)
						}
					}
				}
			}
			sort.Strings(want)
			got := listFiles(t, staging)
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("staged files %v, want %v", got, want)
			}
			if runtime.GOOS != "windows" {
				dirInfo, _ := os.Stat(filepath.Join(staging, "projects"))
				if perm := dirInfo.Mode().Perm(); perm != 0o700 {
					t.Errorf("projects/ perm %o, want 0700", perm)
				}
			}
			if extra := listFiles(t, outer); len(extra) != len(want) {
				t.Fatalf("files outside staging: %v", extra)
			}
		})
	}
}

func TestPeekManifestThenUnpack(t *testing.T) {
	m, src := sampleManifest()
	for _, comp := range []Compression{CompNone, CompGzip} {
		data, raw := build(t, m, src, comp)
		// A one-byte-at-a-time reader proves rest replays exactly what was
		// consumed regardless of how the underlying stream is chunked.
		pm, praw, rest, err := PeekManifest(io.NopCloser(&oneByteReader{r: bytes.NewReader(data)}))
		if err != nil {
			t.Fatalf("comp %d: PeekManifest: %v", comp, err)
		}
		if !bytes.Equal(praw, raw) || pm.BundleID != bundleID || len(pm.Entries) != 2 {
			t.Fatalf("comp %d: peek = %+v", comp, pm)
		}
		_, staging := stage(t)
		u, err := Unpack(context.Background(), rest, staging, sha(raw), DefaultLimits, true, nil)
		if err != nil {
			t.Fatalf("comp %d: Unpack(rest): %v", comp, err)
		}
		if len(u.Files) != m.Totals.Files {
			t.Fatalf("comp %d: %d files", comp, len(u.Files))
		}
	}
	for _, bad := range []string{"", "BFFS", Magic + "\x02", Magic + "\x00", Magic + "\x01\x1f\x8b"} {
		if _, _, _, err := PeekManifest(strings.NewReader(bad)); err == nil {
			t.Errorf("PeekManifest(%q) = nil error", bad)
		}
	}
	// PeekManifest does not validate; the caller must.
	bad, _ := sampleManifest()
	bad.Source.Hostname = "../x"
	badRaw, _ := json.Marshal(bad)
	pm, _, _, err := PeekManifest(bytes.NewReader(envelope(t, CompGzip, makeTar(t, manifestEntry(badRaw)))))
	if err != nil || pm.Source.Hostname != "../x" {
		t.Fatalf("peek of an invalid manifest = %+v, %v", pm, err)
	}
	if err := pm.Validate(DefaultLimits); err == nil {
		t.Fatal("Validate accepted hostname ../x")
	}
}

type oneByteReader struct{ r io.Reader }

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestUnpackRejects(t *testing.T) {
	seeds := seedBundles(t)
	cases := []struct{ seed, want string }{
		{"symlink", "type"},
		{"hardlink", "type"},
		{"global-header", "type"},
		{"unlisted", "not listed"},
		{"size-mismatch", "bytes in the archive"},
		{"sha-mismatch", "sha256"},
		{"duplicate", "duplicate"},
		{"duplicate-case", "duplicate"},
		{"manifest-late", "must be entry 0"},
		{"no-manifest", "must be entry 0"},
		{"missing-file", "incomplete"},
		{"trailing", "trailing data"},
		{"bomb-tail", "after the end of the archive"},
		{"bomb-header", "bytes in the archive"},
		{"zstd", "unsupported compression 2; upgrade bffs"},
		{"comp-9", "unknown compression"},
		{"bad-magic", "bad magic"},
		{"not-a-bundle", "not a bffs bundle"},
		{"empty", "not a bffs bundle"},
		{"sparse", "sparse"},
		{"bad-hostname", "source.hostname"},
		{"bad-json", manifestName},
		{"truncated", ""},
		{"dir-unlisted", "not an ancestor"},
		{"dir-escape", "not local"},
		{"dir-duplicate", "duplicate entry"},
		{"fifo", "type"},
		{"pax-flood", "more tar bytes than the manifest implies"},
	}
	for _, c := range cases {
		t.Run(c.seed, func(t *testing.T) {
			data, ok := seeds[c.seed]
			if !ok {
				t.Fatalf("no seed %q", c.seed)
			}
			outer, staging := stage(t)
			_, err := Unpack(context.Background(), bytes.NewReader(data), staging, "", DefaultLimits, true, nil)
			if err == nil {
				t.Fatalf("Unpack accepted the %s bundle", c.seed)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Unpack = %q, want it to contain %q", err, c.want)
			}
			for _, f := range listFiles(t, outer) {
				if !strings.HasPrefix(f, "stage/") {
					t.Fatalf("file outside staging: %s", f)
				}
			}
		})
	}
	// The trailing-garbage bundle is fine when the caller owns the rest of
	// the stream (network mode).
	if _, _, err := unpack(t, seeds["trailing"], DefaultLimits, false); err != nil {
		t.Fatalf("requireEOF=false: %v", err)
	}
}

// TestUnpackPAXFloodStopsEarly pins the header-bomb bound: an archive whose
// PAX headers alone exceed what the manifest implies is refused after at
// most that many tar bytes, however long the stream goes on.
func TestUnpackPAXFloodStopsEarly(t *testing.T) {
	m, src := sampleManifest()
	raw, _ := json.Marshal(m)
	var fileEntries []tarEntry
	for _, e := range m.Entries {
		for _, f := range e.Files {
			fileEntries = append(fileEntries, regEntry(f.Path, src[f.Path]))
		}
	}
	flood := paxFloodTar(raw, fileEntries, 4000) // ~18 MiB of headers
	var counted countingReader
	counted.r = bytes.NewReader(envelope(t, CompNone, flood))
	_, staging := stage(t)
	_, err := Unpack(context.Background(), &counted, staging, "", DefaultLimits, true, nil)
	if err == nil || !strings.Contains(err.Error(), "more tar bytes than the manifest implies") {
		t.Fatalf("Unpack = %v", err)
	}
	// Generous bound: manifest + 8 files + implied dirs at 4 KiB each is
	// well under 128 KiB; bufio may read a little ahead.
	if counted.n > 256<<10 {
		t.Fatalf("read %d bytes of a %d-byte flood before refusing", counted.n, len(flood))
	}
	// The same flood with a few headers is an ordinary rejection too, and a
	// PAX path record on a real file (needed for >100-byte names) is fine.
	small := paxFloodTar(raw, fileEntries, 0)
	if _, _, err := unpack(t, envelope(t, CompNone, small), DefaultLimits, true); err != nil {
		t.Fatalf("hand-built PAX archive without a flood: %v", err)
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestUnpackTrailingTarPadding(t *testing.T) {
	// Uncompressed: bytes after the end-of-archive marker are trailing data
	// under requireEOF and left unread otherwise.
	m, src := sampleManifest()
	data, _ := build(t, m, src, CompNone)
	padded := append(append([]byte{}, data...), make([]byte, 512)...)
	if _, _, err := unpack(t, padded, DefaultLimits, true); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("padded + requireEOF: %v", err)
	}
	r := bytes.NewReader(padded)
	_, staging := stage(t)
	if _, err := Unpack(context.Background(), r, staging, "", DefaultLimits, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnpackExpectSHA256(t *testing.T) {
	m, src := sampleManifest()
	data, raw := build(t, m, src, CompGzip)
	_, staging := stage(t)
	if _, err := Unpack(context.Background(), bytes.NewReader(data), staging, strings.ToUpper(sha(raw)), DefaultLimits, true, nil); err != nil {
		t.Fatalf("case-insensitive hex should match: %v", err)
	}
	_, staging = stage(t)
	_, err := Unpack(context.Background(), bytes.NewReader(data), staging, strings.Repeat("0", 64), DefaultLimits, true, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong expectSHA256: %v", err)
	}
	if files := listFiles(t, staging); len(files) != 0 {
		t.Fatalf("files staged despite a manifest mismatch: %v", files)
	}
}

// TestUnpackExpectSHA256IsByteExact: the check is on entry 0's bytes, so an
// indented manifest passes with its own digest and fails with the compact
// one — the format is not fragile, only the bytes B accepted matter.
func TestUnpackExpectSHA256IsByteExact(t *testing.T) {
	m, src := sampleManifest()
	indented, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	compact, _ := json.Marshal(m)
	if bytes.Equal(indented, compact) {
		t.Fatal("test needs two different encodings")
	}
	entries := []tarEntry{manifestEntry(indented)}
	for _, e := range m.Entries {
		for _, f := range e.Files {
			entries = append(entries, regEntry(f.Path, src[f.Path]))
		}
	}
	data := envelope(t, CompGzip, makeTar(t, entries...))
	_, staging := stage(t)
	u, err := Unpack(context.Background(), bytes.NewReader(data), staging, sha(indented), DefaultLimits, true, nil)
	if err != nil {
		t.Fatalf("indented manifest with its own digest: %v", err)
	}
	if !bytes.Equal(u.RawManifest, indented) {
		t.Fatal("RawManifest is not entry 0's bytes")
	}
	_, staging = stage(t)
	_, err = Unpack(context.Background(), bytes.NewReader(data), staging, sha(compact), DefaultLimits, true, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("indented manifest with the compact digest: %v", err)
	}
}

func TestUnpackLimits(t *testing.T) {
	m, src := sampleManifest()
	data, _ := build(t, m, src, CompGzip)
	small := DefaultLimits
	small.MaxManifestBytes = 10
	if _, _, err := unpack(t, data, small, true); err == nil || !strings.Contains(err.Error(), "max 10") {
		t.Fatalf("manifest cap: %v", err)
	}
	small = DefaultLimits
	small.MaxEntryBytes = 8
	if _, _, err := unpack(t, data, small, true); err == nil || !strings.Contains(err.Error(), "max 8") {
		t.Fatalf("entry cap: %v", err)
	}
	small = DefaultLimits
	small.MaxTotalBytes = m.Totals.Bytes - 1
	if _, _, err := unpack(t, data, small, true); err == nil {
		t.Fatal("total cap ignored")
	}
	if _, _, err := unpack(t, data, Limits{}, true); err == nil {
		t.Fatal("zero limits accepted a bundle")
	}
}

func TestUnpackStagingMustBeEmpty(t *testing.T) {
	m, src := sampleManifest()
	data, _ := build(t, m, src, CompNone)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leftover"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(context.Background(), bytes.NewReader(data), dir, "", DefaultLimits, true, nil); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("non-empty staging: %v", err)
	}
	if _, err := Unpack(context.Background(), bytes.NewReader(data), filepath.Join(dir, "missing"), "", DefaultLimits, true, nil); err == nil {
		t.Fatal("missing staging dir accepted")
	}
}

func TestUnpackContextCancel(t *testing.T) {
	m, src := sampleManifest()
	data, _ := build(t, m, src, CompNone)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, staging := stage(t)
	if _, err := Unpack(ctx, bytes.NewReader(data), staging, "", DefaultLimits, true, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Unpack = %v", err)
	}
}

// TestUnpackContextCancelMidFile: cancelling while a file body is streaming
// stops the copy on the next read instead of waiting for the entry to end.
func TestUnpackContextCancelMidFile(t *testing.T) {
	m, src := sampleManifest()
	data, _ := build(t, m, src, CompNone)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel once the stream is inside the transcript body (entry 1), then
	// stall: without a ctx-aware reader the copy would spin on 0-byte reads.
	r := &cancelThenStall{r: bytes.NewReader(data), at: envelopeLen + 4*512 + 10, cancel: cancel}
	_, staging := stage(t)
	_, err := Unpack(ctx, r, staging, "", DefaultLimits, true, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Unpack = %v, want context.Canceled", err)
	}
}

// cancelThenStall serves bytes until at, then cancels and returns 0-byte
// reads for a while before giving up with its own error.
type cancelThenStall struct {
	r      io.Reader
	at     int
	n      int
	stalls int
	cancel context.CancelFunc
}

func (c *cancelThenStall) Read(p []byte) (int, error) {
	if c.n >= c.at {
		c.cancel()
		c.stalls++
		if c.stalls > 1000 {
			return 0, errors.New("stalled reader gave up")
		}
		return 0, nil
	}
	if len(p) > c.at-c.n {
		p = p[:c.at-c.n]
	}
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestUnpackSkipsReservedWithNote(t *testing.T) {
	m, src := sampleManifest()
	reserved := "claudejson/" + slugA + ".json"
	src[reserved] = []byte(`{"hasTrustDialogAccepted":true}`)
	m.Entries[0].Files = append(m.Entries[0].Files, fileFor(reserved, src[reserved], testNow))
	finalize(m)
	data, _ := build(t, m, src, CompGzip)
	u, staging, err := unpack(t, data, DefaultLimits, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Notes) != 1 || !strings.Contains(u.Notes[0], reserved) {
		t.Fatalf("Notes = %v", u.Notes)
	}
	if _, ok := u.Files[reserved]; ok {
		t.Fatal("reserved entry listed in Files")
	}
	if _, err := os.Stat(filepath.Join(staging, "claudejson")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved entry written: %v", err)
	}
	if len(u.Files) != m.Totals.Files-1 {
		t.Fatalf("%d files, want %d", len(u.Files), m.Totals.Files-1)
	}
}

func TestUnpackAcceptsImpliedDirEntries(t *testing.T) {
	m, src := sampleManifest()
	raw, _ := json.Marshal(m)
	entries := []tarEntry{manifestEntry(raw), {hdr: tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeDir, Name: "projects/" + slugA + "/" + sidA + "/subagents", Mode: 0o755}}}
	for _, e := range m.Entries {
		for _, f := range e.Files {
			entries = append(entries, regEntry(f.Path, src[f.Path]))
		}
	}
	data := envelope(t, CompGzip, makeTar(t, entries...))
	if _, _, err := unpack(t, data, DefaultLimits, true); err != nil {
		t.Fatalf("implied dir entry rejected: %v", err)
	}
}

func TestUnpackMtimeWindow(t *testing.T) {
	m, src := sampleManifest()
	old := "plans/quirky-lemur.md"
	future := "history/" + sidA + ".jsonl"
	m.Entries[0].Files[5].ModTime = time.Date(2019, 12, 31, 0, 0, 0, 0, time.UTC)
	m.Entries[0].Files[4].ModTime = testNow.Add(48 * time.Hour)
	defer func(f func() time.Time) { nowFn = f }(nowFn)
	nowFn = func() time.Time { return testNow }
	data, _ := build(t, m, src, CompNone)
	_, staging, err := unpack(t, data, DefaultLimits, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{old, future} {
		info, err := os.Stat(filepath.Join(staging, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		if y := info.ModTime().Year(); y < 2025 || y > time.Now().Year()+1 {
			t.Errorf("%s: mtime %v applied outside the window", p, info.ModTime())
		}
	}
}

// TestUnpackRootContainsEscape bypasses the lexical checks and proves
// os.Root alone keeps a hostile name inside stagingDir.
func TestUnpackRootContainsEscape(t *testing.T) {
	m := &Manifest{Format: 1, BundleID: bundleID, Created: testNow, Source: Source{Hostname: "h"}}
	raw, _ := json.Marshal(m)
	skipNameCheck = true
	defer func() { skipNameCheck = false }()
	for _, name := range []string{"../escape", "sub/../../escape2", "a/../../../escape3"} {
		data := envelope(t, CompGzip, makeTar(t, manifestEntry(raw), regEntry(name, []byte("pwned"))))
		outer, staging := stage(t)
		_, err := Unpack(context.Background(), bytes.NewReader(data), staging, "", DefaultLimits, true, nil)
		if err == nil {
			t.Fatalf("%q: Unpack succeeded with the name check bypassed", name)
		}
		for _, f := range listFiles(t, outer) {
			if !strings.HasPrefix(f, "stage/") {
				t.Fatalf("%q: escaped to %s", name, f)
			}
		}
		if _, err := os.Stat(filepath.Join(outer, "escape")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%q: file landed beside staging", name)
		}
	}
	// And a benign unlisted name under the bypass still lands only inside.
	data := envelope(t, CompGzip, makeTar(t, manifestEntry(raw), regEntry("inside/ok", []byte("x"))))
	outer, staging := stage(t)
	if _, err := Unpack(context.Background(), bytes.NewReader(data), staging, "", DefaultLimits, true, nil); err != nil {
		t.Fatalf("bypass: %v", err)
	}
	if got := listFiles(t, outer); len(got) != 1 || got[0] != "stage/inside/ok" {
		t.Fatalf("files = %v", got)
	}
}

func TestUnpackNoCleanupOnError(t *testing.T) {
	// Unpack leaves what it wrote; the caller owns the staging dir.
	seeds := seedBundles(t)
	_, staging, err := unpack(t, seeds["missing-file"], DefaultLimits, true)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := listFiles(t, staging); len(got) != 1 {
		t.Fatalf("staged files after failure = %v, want the one that arrived", got)
	}
}
