package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestBuildEmitsPAXAndManifestFirst(t *testing.T) {
	m, src := sampleManifest()
	for _, comp := range []Compression{CompNone, CompGzip} {
		data, raw := build(t, m, src, comp)
		if string(data[:len(Magic)]) != Magic || data[len(Magic)] != byte(comp) {
			t.Fatalf("comp %d: envelope %q", comp, data[:envelopeLen])
		}
		want, _ := json.Marshal(m)
		if !bytes.Equal(raw, want) {
			t.Fatalf("comp %d: returned manifest differs from json.Marshal(m)", comp)
		}
		if bytes.Contains(raw, []byte("\n")) {
			t.Fatalf("comp %d: manifest is not compact", comp)
		}

		payload := tarPayload(t, data)
		tr := tar.NewReader(bytes.NewReader(payload))
		h0, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h0.Name != manifestName || h0.Typeflag != tar.TypeReg {
			t.Fatalf("entry 0 = %q type %q", h0.Name, h0.Typeflag)
		}
		got, _ := io.ReadAll(tr)
		if !bytes.Equal(got, raw) {
			t.Fatalf("comp %d: entry 0 bytes differ from the returned manifest", comp)
		}

		hdrs := tarHeaders(t, payload)
		if len(hdrs) != 1+m.Totals.Files {
			t.Fatalf("comp %d: %d headers, want %d", comp, len(hdrs), 1+m.Totals.Files)
		}
		var sawLong bool
		for i, h := range hdrs[1:] {
			if h.Typeflag != tar.TypeReg || h.Mode != 0o600 || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
				t.Errorf("comp %d: header %q = type %q mode %o uid %d gid %d uname %q gname %q", comp, h.Name, h.Typeflag, h.Mode, h.Uid, h.Gid, h.Uname, h.Gname)
			}
			if len(h.Xattrs) != 0 { //nolint:staticcheck // asserting the deprecated field stays empty
				t.Errorf("xattrs on %q", h.Name)
			}
			if h.Name == longName {
				sawLong = true
				if h.Format != tar.FormatPAX {
					t.Errorf("comp %d: %d-byte name %q written as %v, want PAX", comp, len(h.Name), h.Name, h.Format)
				}
				want := m.Entries[0].Files[1].ModTime
				if !h.ModTime.Equal(want) {
					t.Errorf("comp %d: sub-second mtime lost: %v != %v", comp, h.ModTime, want)
				}
			}
			_ = i
		}
		if !sawLong {
			t.Fatalf("comp %d: long entry %q missing", comp, longName)
		}
	}
}

func TestBuildCopiesExactlySize(t *testing.T) {
	m, src := sampleManifest()
	transcript := "projects/" + slugA + "/" + sidA + ".jsonl"
	grown := memOpener{}
	for k, v := range src {
		grown[k] = v
	}
	grown[transcript] = append(append([]byte{}, src[transcript]...), []byte(`{"type":"assistant","late":true}`+"\n")...)

	data, _ := build(t, m, grown, CompGzip)
	u, staging, err := unpack(t, data, DefaultLimits, true)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	got := readStaged(t, staging, transcript)
	if !bytes.Equal(got, src[transcript]) {
		t.Fatalf("staged transcript = %q, want the manifest-sized prefix", got)
	}
	if u.Files[transcript].Size != int64(len(src[transcript])) {
		t.Fatal("Files entry lost")
	}
}

func TestBuildShortSource(t *testing.T) {
	m, src := sampleManifest()
	transcript := "projects/" + slugA + "/" + sidA + ".jsonl"
	short := memOpener{}
	for k, v := range src {
		short[k] = v
	}
	short[transcript] = src[transcript][:len(src[transcript])-3]
	var buf bytes.Buffer
	_, err := Build(context.Background(), &buf, m, short, CompNone, nil)
	if err == nil || !strings.Contains(err.Error(), "short read") {
		t.Fatalf("Build = %v, want short read", err)
	}
}

func TestBuildDetectsChangedContent(t *testing.T) {
	m, src := sampleManifest()
	transcript := "projects/" + slugA + "/" + sidA + ".jsonl"
	changed := memOpener{}
	for k, v := range src {
		changed[k] = v
	}
	changed[transcript] = []byte(strings.ToUpper(string(src[transcript])))
	var buf bytes.Buffer
	_, err := Build(context.Background(), &buf, m, changed, CompNone, nil)
	if err == nil || !strings.Contains(err.Error(), "changed during export") {
		t.Fatalf("Build = %v, want sha mismatch", err)
	}
}

func TestBuildMissingSource(t *testing.T) {
	m, src := sampleManifest()
	delete(src, "plans/quirky-lemur.md")
	var buf bytes.Buffer
	if _, err := Build(context.Background(), &buf, m, src, CompNone, nil); err == nil {
		t.Fatal("Build succeeded without a source")
	}
}

func TestBuildRejectsBadManifestAndCompression(t *testing.T) {
	m, src := sampleManifest()
	var buf bytes.Buffer
	if _, err := Build(context.Background(), &buf, m, src, CompZstd, nil); err == nil || !strings.Contains(err.Error(), "zstd") {
		t.Fatalf("zstd: %v", err)
	}
	if _, err := Build(context.Background(), &buf, m, src, Compression(7), nil); err == nil {
		t.Fatal("compression 7 accepted")
	}
	if buf.Len() != 0 {
		t.Fatal("bytes written before validation failed")
	}
	m.Source.Hostname = "../x"
	if _, err := Build(context.Background(), &buf, m, src, CompNone, nil); err == nil || !strings.Contains(err.Error(), "source.hostname") {
		t.Fatalf("bad hostname: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatal("bytes written for an invalid manifest")
	}
}

func TestBuildContextAndProgress(t *testing.T) {
	m, src := sampleManifest()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if _, err := Build(ctx, &buf, m, src, CompNone, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Build with cancelled ctx = %v", err)
	}

	var calls []Progress
	buf.Reset()
	if _, err := Build(context.Background(), &buf, m, src, CompNone, func(p Progress) { calls = append(calls, p) }); err != nil {
		t.Fatal(err)
	}
	if len(calls) != m.Totals.Files {
		t.Fatalf("%d progress calls, want %d", len(calls), m.Totals.Files)
	}
	last := calls[len(calls)-1]
	if last.Phase != "build" || last.Files != m.Totals.Files || last.TotalFiles != m.Totals.Files || last.Bytes != m.Totals.Bytes || last.TotalBytes != m.Totals.Bytes || last.Current != "memory/"+slugA+"/topic.md" {
		t.Fatalf("last progress = %+v", last)
	}
}

// TestBuildContextCancelMidFile: cancelling while one source streams stops
// the copy on its next read rather than after the file.
func TestBuildContextCancelMidFile(t *testing.T) {
	m, src := sampleManifest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transcript := "projects/" + slugA + "/" + sidA + ".jsonl"
	stalling := stallOpener{memOpener: src, stallOn: transcript, cancel: cancel}
	var buf bytes.Buffer
	_, err := Build(ctx, &buf, m, stalling, CompNone, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Build = %v, want context.Canceled", err)
	}
}

// stallOpener serves one path through a reader that hands out a few bytes,
// cancels, then stalls (0-byte reads, then its own error).
type stallOpener struct {
	memOpener
	stallOn string
	cancel  context.CancelFunc
}

func (s stallOpener) Open(p string) (io.ReadCloser, error) {
	rc, err := s.memOpener.Open(p)
	if err != nil || p != s.stallOn {
		return rc, err
	}
	return io.NopCloser(&stallReader{r: rc, cancel: s.cancel}), nil
}

type stallReader struct {
	r      io.Reader
	n      int
	stalls int
	cancel context.CancelFunc
}

func (s *stallReader) Read(p []byte) (int, error) {
	if s.n >= 5 {
		s.cancel()
		s.stalls++
		if s.stalls > 1000 {
			return 0, errors.New("stalled source gave up")
		}
		return 0, nil
	}
	if len(p) > 5-s.n {
		p = p[:5-s.n]
	}
	n, err := s.r.Read(p)
	s.n += n
	return n, err
}

func TestBuildZeroTimesFormat(t *testing.T) {
	m, src := sampleManifest()
	m.Created = time.Time{}
	m.Entries[1].Files[0].ModTime = time.Time{}
	data, _ := build(t, m, src, CompNone)
	if _, _, err := unpack(t, data, DefaultLimits, true); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
}
