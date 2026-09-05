package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

const (
	sidA     = "0f3b2c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e"
	sidB     = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	slugA    = "-Users-jonas-build-projects-bffs"
	bundleID = "6f1e2c0a-1111-4222-8333-444455556666"
	// longName is a sidecar path longer than the 100-byte ustar name field,
	// so the writer must emit a PAX path record for it.
	longName = "projects/" + slugA + "/" + sidA + "/subagents/agent-a1b2c3d.jsonl"
)

// memOpener is the tiny in-memory Opener tests feed to Build.
type memOpener map[string][]byte

func (m memOpener) Open(p string) (io.ReadCloser, error) {
	b, ok := m[p]
	if !ok {
		return nil, fmt.Errorf("open %q: %w", p, fs.ErrNotExist)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fileFor(p string, data []byte, mtime time.Time) File {
	return File{Path: p, Size: int64(len(data)), SHA256: sha(data), ModTime: mtime}
}

// finalize recomputes Totals from the entries.
func finalize(m *Manifest) {
	m.Totals = Totals{Entries: len(m.Entries)}
	for _, e := range m.Entries {
		for _, f := range e.Files {
			m.Totals.Files++
			m.Totals.Bytes += f.Size
		}
	}
}

// sampleManifest is a valid two-entry manifest (one session with every
// part, one memory directory) and the opener that serves its bytes.
func sampleManifest() (*Manifest, memOpener) {
	texts := map[string]string{
		"projects/" + slugA + "/" + sidA + ".jsonl": `{"type":"user","sessionId":"` + sidA + `","cwd":"/Users/jonas/build/projects/bffs"}` + "\n" + `{"type":"assistant","message":{"usage":{"input_tokens":1}}}` + "\n",
		longName: `{"type":"user","message":"sub"}` + "\n",
		"projects/" + slugA + "/" + sidA + "/custom-title.json": `{"title":"fix the shim"}`,
		"file-history/" + sidA + "/deadbeefdeadbeef@v1":         "package main\n",
		"history/" + sidA + ".jsonl":                            `{"display":"hi","sessionId":"` + sidA + `","timestamp":1}` + "\n",
		"plans/quirky-lemur.md":                                 "# plan\n",
		"memory/" + slugA + "/MEMORY.md":                        "# index\n- [topic](topic.md)\n",
		"memory/" + slugA + "/topic.md":                         "---\nname: topic\n---\nbody\n",
	}
	src := memOpener{}
	for k, v := range texts {
		src[k] = []byte(v)
	}
	m := &Manifest{
		Format:        FormatVersion,
		BundleID:      bundleID,
		BFFSVersion:   "0.3.0",
		ClaudeVersion: "2.1.259",
		Created:       testNow,
		Source: Source{
			Hostname: "mac-a", User: "jonas", Home: "/Users/jonas", OS: "darwin", Arch: "arm64",
			ConfigDir: "/Users/jonas/Library/Application Support/bffs/sessions/aviate",
			RootDir:   "/Users/jonas/.claude/projects", Account: "aviate", AccountType: "oauth", Isolation: "partial",
		},
		Entries: []Entry{
			{
				Kind: EntrySession, Slug: slugA, Cwd: "/Users/jonas/build/projects/bffs",
				ProjectKey: "/Users/jonas/build/projects/bffs", GitRemote: "git@github.com:jratienza65/bffs.git",
				SessionID: sidA, Title: "fix the shim", GitBranch: "main", PlanSlug: "quirky-lemur",
				Started: testNow.Add(-time.Hour), Last: testNow.Add(-30 * time.Minute),
				Parts:       []string{"transcript", "sidecar", "file-history", "history", "plans"},
				SourceTrust: &TrustInfo{Accepted: true},
				Files: []File{
					fileFor("projects/"+slugA+"/"+sidA+".jsonl", []byte(src["projects/"+slugA+"/"+sidA+".jsonl"]), testNow.Add(-30*time.Minute)),
					fileFor(longName, []byte(src[longName]), testNow.Add(-31*time.Minute).Add(123456789*time.Nanosecond)),
					fileFor("projects/"+slugA+"/"+sidA+"/custom-title.json", []byte(src["projects/"+slugA+"/"+sidA+"/custom-title.json"]), testNow.Add(-29*time.Minute)),
					fileFor("file-history/"+sidA+"/deadbeefdeadbeef@v1", []byte(src["file-history/"+sidA+"/deadbeefdeadbeef@v1"]), testNow.Add(-40*time.Minute)),
					fileFor("history/"+sidA+".jsonl", []byte(src["history/"+sidA+".jsonl"]), testNow.Add(-28*time.Minute)),
					fileFor("plans/quirky-lemur.md", []byte(src["plans/quirky-lemur.md"]), testNow.Add(-50*time.Minute)),
				},
			},
			{
				Kind: EntryMemory, Slug: slugA, Cwd: "/Users/jonas/build/projects/bffs", ProjectKey: "/Users/jonas/build/projects/bffs",
				Files: []File{
					fileFor("memory/"+slugA+"/MEMORY.md", []byte(src["memory/"+slugA+"/MEMORY.md"]), testNow.Add(-24*time.Hour)),
					fileFor("memory/"+slugA+"/topic.md", []byte(src["memory/"+slugA+"/topic.md"]), testNow.Add(-25*time.Hour)),
				},
			},
		},
	}
	finalize(m)
	return m, src
}

func build(tb testing.TB, m *Manifest, src Opener, comp Compression) ([]byte, []byte) {
	tb.Helper()
	var buf bytes.Buffer
	raw, err := Build(context.Background(), &buf, m, src, comp, nil)
	if err != nil {
		tb.Fatalf("Build: %v", err)
	}
	return buf.Bytes(), raw
}

// stage makes an outer temp dir with an empty "stage" subdirectory so tests
// can assert nothing lands beside it.
func stage(tb testing.TB) (outer, staging string) {
	tb.Helper()
	outer = tb.TempDir()
	staging = filepath.Join(outer, "stage")
	if err := os.Mkdir(staging, 0o700); err != nil {
		tb.Fatal(err)
	}
	return outer, staging
}

func unpack(tb testing.TB, data []byte, l Limits, requireEOF bool) (*Unpacked, string, error) {
	tb.Helper()
	_, staging := stage(tb)
	u, err := Unpack(context.Background(), bytes.NewReader(data), staging, "", l, requireEOF, nil)
	return u, staging, err
}

// listFiles returns every regular file and symlink under dir, relative,
// slash-separated, sorted by WalkDir.
func listFiles(tb testing.TB, dir string) []string {
	tb.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// tarEntry is one entry for makeTar; Data is written after the header.
type tarEntry struct {
	hdr  tar.Header
	data []byte
}

func regEntry(name string, data []byte) tarEntry {
	return tarEntry{hdr: tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeReg, Name: name, Size: int64(len(data)), Mode: 0o600, ModTime: testNow}, data: data}
}

func manifestEntry(raw []byte) tarEntry {
	return regEntry(manifestName, raw)
}

// makeTar writes entries with archive/tar and returns the plain tar bytes.
func makeTar(tb testing.TB, entries ...tarEntry) []byte {
	tb.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := e.hdr
		if err := tw.WriteHeader(&h); err != nil {
			tb.Fatalf("WriteHeader(%q): %v", e.hdr.Name, err)
		}
		if _, err := tw.Write(e.data); err != nil {
			tb.Fatalf("Write(%q): %v", e.hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(tb testing.TB, payload []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(payload); err != nil {
		tb.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// envelope wraps a tar stream in the bundle envelope, compressing per comp.
func envelope(tb testing.TB, comp Compression, tarBytes []byte) []byte {
	tb.Helper()
	out := append([]byte(Magic), byte(comp))
	if comp == CompGzip {
		return append(out, gzipBytes(tb, tarBytes)...)
	}
	return append(out, tarBytes...)
}

// tarPayload strips the envelope from a bundle and returns the tar bytes.
func tarPayload(tb testing.TB, bundle []byte) []byte {
	tb.Helper()
	if len(bundle) < envelopeLen || string(bundle[:len(Magic)]) != Magic {
		tb.Fatal("not a bundle")
	}
	rest := bundle[envelopeLen:]
	if Compression(bundle[len(Magic)]) == CompNone {
		return rest
	}
	gz, err := gzip.NewReader(bytes.NewReader(rest))
	if err != nil {
		tb.Fatal(err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// tarHeaders lists the headers of a tar stream.
func tarHeaders(tb testing.TB, tarBytes []byte) []*tar.Header {
	tb.Helper()
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	var out []*tar.Header
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, h)
	}
}

// ustarBlock hand-crafts a 512-byte USTAR header so tests can emit header
// types archive/tar refuses to write (PAX extended headers with sparse
// records, for one).
func ustarBlock(name string, typeflag byte, size int64, linkname string) []byte {
	blk := make([]byte, 512)
	copy(blk[0:100], name)
	copy(blk[100:108], "0000600\x00")
	copy(blk[108:116], "0000000\x00")
	copy(blk[116:124], "0000000\x00")
	copy(blk[124:136], fmt.Sprintf("%011o\x00", size))
	copy(blk[136:148], fmt.Sprintf("%011o\x00", testNow.Unix()))
	copy(blk[148:156], "        ")
	blk[156] = typeflag
	copy(blk[157:257], linkname)
	copy(blk[257:263], "ustar\x00")
	copy(blk[263:265], "00")
	var sum int
	for _, b := range blk {
		sum += int(b)
	}
	copy(blk[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return blk
}

func padBlock(data []byte) []byte {
	if rem := len(data) % 512; rem != 0 {
		data = append(data, make([]byte, 512-rem)...)
	}
	return data
}

// paxRecords formats PAX "len key=value\n" records.
func paxRecords(kv ...string) []byte {
	var buf bytes.Buffer
	for i := 0; i+1 < len(kv); i += 2 {
		body := " " + kv[i] + "=" + kv[i+1] + "\n"
		n := len(body)
		for l := n; ; l++ {
			if len(strconv.Itoa(l))+n == l {
				n = l
				break
			}
		}
		buf.WriteString(strconv.Itoa(n))
		buf.WriteString(body)
	}
	return buf.Bytes()
}

// sparseTar is a plain tar (manifest first) followed by a hand-built PAX
// 0.1 sparse entry for name: logical size 1 MiB, 4 physical bytes.
func sparseTar(tb testing.TB, raw []byte, name string) []byte {
	tb.Helper()
	out := padBlock(append(ustarBlock(manifestName, tar.TypeReg, int64(len(raw)), ""), raw...))
	recs := paxRecords(
		"GNU.sparse.major", "0",
		"GNU.sparse.minor", "1",
		"GNU.sparse.numblocks", "1",
		"GNU.sparse.map", "0,4",
		"GNU.sparse.size", "1048576",
		"path", name,
	)
	out = append(out, ustarBlock("PaxHeaders.0/sparse", tar.TypeXHeader, int64(len(recs)), "")...)
	out = append(out, padBlock(recs)...)
	out = append(out, ustarBlock(name, tar.TypeReg, 4, "")...)
	out = append(out, padBlock([]byte("abcd"))...)
	return append(out, make([]byte, 1024)...)
}

// seedBundles is every bundle the reader must reject (plus the two it
// must accept), shared by the rejection tests and the fuzz corpus.
func seedBundles(tb testing.TB) map[string][]byte {
	tb.Helper()
	m, src := sampleManifest()
	raw, err := json.Marshal(m)
	if err != nil {
		tb.Fatal(err)
	}
	transcript := "projects/" + slugA + "/" + sidA + ".jsonl"
	var fileEntries []tarEntry
	for _, e := range m.Entries {
		for _, f := range e.Files {
			fileEntries = append(fileEntries, regEntry(f.Path, src[f.Path]))
		}
	}
	withAll := func(extra ...tarEntry) []tarEntry {
		out := []tarEntry{manifestEntry(raw)}
		out = append(out, fileEntries...)
		return append(out, extra...)
	}
	replace := func(name string, data []byte) []tarEntry {
		out := []tarEntry{manifestEntry(raw)}
		for _, e := range fileEntries {
			if e.hdr.Name == name {
				e = regEntry(name, data)
			}
			out = append(out, e)
		}
		return out
	}
	gz := func(entries []tarEntry) []byte { return envelope(tb, CompGzip, makeTar(tb, entries...)) }

	badHost, _ := sampleManifest()
	badHost.Source.Hostname = "../x"
	badHostRaw, _ := json.Marshal(badHost)

	validGzip, _ := build(tb, m, src, CompGzip)
	validNone, _ := build(tb, m, src, CompNone)

	seeds := map[string][]byte{
		"valid-gzip":     validGzip,
		"valid-none":     validNone,
		"symlink":        gz(withAll(tarEntry{hdr: tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeSymlink, Name: "projects/" + slugA + "/" + sidA + "/subagents/link", Linkname: "../../../../../etc/passwd"}})),
		"hardlink":       gz(withAll(tarEntry{hdr: tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeLink, Name: "projects/" + slugA + "/" + sidA + "/subagents/hard", Linkname: transcript}})),
		"global-header":  gz(withAll(tarEntry{hdr: tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeXGlobalHeader, Name: "g", PAXRecords: map[string]string{"comment": "x"}}})),
		"unlisted":       gz(withAll(regEntry("projects/"+slugA+"/"+sidA+"/subagents/extra.jsonl", []byte("x")))),
		"size-mismatch":  gz(replace(transcript, append(append([]byte{}, src[transcript]...), "extra line\n"...))),
		"sha-mismatch":   gz(replace(transcript, bytes.ToUpper(src[transcript]))),
		"duplicate":      gz(withAll(regEntry(transcript, src[transcript]))),
		"duplicate-case": gz(withAll(regEntry(strings.TrimSuffix(longName, "agent-a1b2c3d.jsonl")+"AGENT-A1B2C3D.jsonl", src[longName]))),
		"manifest-late":  gz(append([]tarEntry{fileEntries[0]}, manifestEntry(raw))),
		"no-manifest":    gz(nil),
		"missing-file":   gz([]tarEntry{manifestEntry(raw), fileEntries[0]}),
		"trailing":       append(append([]byte{}, validGzip...), []byte("junk")...),
		"bomb-tail":      envelope(tb, CompGzip, append(makeTar(tb, withAll()...), make([]byte, trailerSlack+512)...)),
		"bomb-header":    envelope(tb, CompGzip, bombHeaderTar(tb, raw, fileEntries, transcript)),
		"zstd":           append([]byte(Magic+"\x02"), validNone[envelopeLen:]...),
		"comp-9":         append([]byte(Magic+"\x09"), validNone[envelopeLen:]...),
		"bad-magic":      append([]byte("BFFS\x02\x00"), validNone[envelopeLen:]...),
		"not-a-bundle":   []byte("hello"),
		"empty":          {},
		"sparse":         envelope(tb, CompGzip, sparseTar(tb, raw, transcript)),
		"bad-hostname":   gz([]tarEntry{manifestEntry(badHostRaw)}),
		"bad-json":       gz([]tarEntry{manifestEntry([]byte("{not json"))}),
		"truncated":      validGzip[:len(validGzip)-40],
		"dir-unlisted":   gz(withAll(tarEntry{hdr: tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeDir, Name: "projects/" + slugA + "/" + sidB, Mode: 0o700}})),
		"dir-escape":     gz(withAll(tarEntry{hdr: tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeDir, Name: "../out", Mode: 0o700}})),
		"dir-duplicate":  gz(withAll(tarEntry{hdr: tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeDir, Name: "projects", Mode: 0o700}}, tarEntry{hdr: tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeDir, Name: "projects", Mode: 0o700}})),
		"fifo":           envelope(tb, CompNone, typeflagTar(raw, transcript, src[transcript], tar.TypeFifo)),
		"pax-flood":      envelope(tb, CompGzip, paxFloodTar(raw, fileEntries, 32)),
	}
	return seeds
}

// typeflagTar is manifest + one hand-built entry for name with the given
// typeflag (archive/tar refuses to write a fifo with data).
func typeflagTar(raw []byte, name string, data []byte, typeflag byte) []byte {
	out := padBlock(append(ustarBlock(manifestName, tar.TypeReg, int64(len(raw)), ""), raw...))
	out = append(out, ustarBlock(name, typeflag, int64(len(data)), "")...)
	out = append(out, padBlock(data)...)
	return append(out, make([]byte, 1024)...)
}

// paxFloodTar is a well-formed archive with n bare PAX extended headers
// (4 KiB of records each, applying to nothing) between the manifest and
// the files: the header bomb the tar byte cap must stop.
func paxFloodTar(raw []byte, fileEntries []tarEntry, n int) []byte {
	out := padBlock(append(ustarBlock(manifestName, tar.TypeReg, int64(len(raw)), ""), raw...))
	recs := paxRecords("comment", strings.Repeat("x", 4000))
	one := append(ustarBlock("PaxHeaders.0/x", tar.TypeXHeader, int64(len(recs)), ""), padBlock(recs)...)
	out = append(out, bytes.Repeat(one, n)...)
	for _, e := range fileEntries {
		path := paxRecords("path", e.hdr.Name)
		out = append(out, ustarBlock("PaxHeaders.0/f", tar.TypeXHeader, int64(len(path)), "")...)
		out = append(out, padBlock(path)...)
		out = append(out, ustarBlock(e.hdr.Name, tar.TypeReg, e.hdr.Size, "")...)
		out = append(out, padBlock(e.data)...)
	}
	return append(out, make([]byte, 1024)...)
}

// bombHeaderTar is the archive with the transcript's header claiming 4 GiB
// and no body behind it: the size check must refuse before reading any of
// it (archive/tar itself refuses to write such a header, hence by hand).
func bombHeaderTar(tb testing.TB, raw []byte, fileEntries []tarEntry, transcript string) []byte {
	tb.Helper()
	entries := []tarEntry{manifestEntry(raw)}
	for _, e := range fileEntries {
		if e.hdr.Name != transcript {
			entries = append(entries, e)
		}
	}
	out := makeTar(tb, entries...)
	out = out[:len(out)-1024] // drop the end-of-archive marker
	out = append(out, ustarBlock(transcript, tar.TypeReg, 1<<32, "")...)
	return append(out, make([]byte, 1024)...)
}
