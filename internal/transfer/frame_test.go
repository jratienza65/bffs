package transfer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// countingReader records how many bytes a frame reader pulled.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func frameBytes(payload []byte) []byte {
	b := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(payload)))
	copy(b[4:], payload)
	return b
}

func TestReadFrameCapBeforeAlloc(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 1<<31)
	cr := &countingReader{r: bytes.NewReader(append(hdr[:], make([]byte, 64)...))}
	_, err := readFrame(cr, maxControlFrame)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize header: err = %v", err)
	}
	if cr.n != 4 {
		t.Fatalf("read %d bytes after an oversize header, want 4 (no body read, no allocation)", cr.n)
	}
	// Exactly the cap is fine; one more is not.
	ok := frameBytes(make([]byte, maxControlFrame))
	if _, err := readFrame(bytes.NewReader(ok), maxControlFrame); err != nil {
		t.Fatalf("frame at the cap: %v", err)
	}
	over := frameBytes(make([]byte, maxControlFrame+1))
	if _, err := readFrame(bytes.NewReader(over), maxControlFrame); err == nil {
		t.Fatal("frame over the cap accepted")
	}
}

func TestReadFrameTruncated(t *testing.T) {
	full := frameBytes([]byte(`{"t":"ping"}`))
	for cut := 0; cut < len(full); cut++ {
		_, err := readFrame(bytes.NewReader(full[:cut]), maxControlFrame)
		if err == nil {
			t.Fatalf("truncated at %d bytes: no error", cut)
		}
		if cut > 0 && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated at %d bytes: err = %v, want ErrUnexpectedEOF", cut, err)
		}
	}
	// An empty stream is a plain EOF (the peer hung up cleanly).
	if _, err := readFrame(bytes.NewReader(nil), maxControlFrame); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream: err = %v, want io.EOF", err)
	}
}

func TestReadMsgJunk(t *testing.T) {
	junk := [][]byte{
		[]byte(`not json`),
		[]byte(`[]`),
		[]byte(`"hello"`),
		[]byte(`123`),
		[]byte(`{}`),                    // no "t"
		[]byte(`{"t":""}`),              // empty "t"
		[]byte(`{"t":1}`),               // wrong type
		[]byte(`null`),                  // no "t"
		[]byte(`{"t":"x"`),              // unterminated
		[]byte("{\"t\":\"hello\"}\x00"), // trailing NUL
	}
	for _, j := range junk {
		if _, err := readMsg(bytes.NewReader(frameBytes(j))); err == nil {
			t.Errorf("readMsg(%q) accepted junk", j)
		}
	}
	m, err := readMsg(bytes.NewReader(frameBytes([]byte(`{"t":"hello","v":1,"formats":[1,2],"proof":"AA==","extra":true}`))))
	if err != nil {
		t.Fatal(err)
	}
	if m.T != "hello" || m.V != 1 || len(m.Formats) != 2 || m.Proof != "AA==" {
		t.Fatalf("decoded %+v", m)
	}
}

func TestWriteMsgRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeMsg(&buf, badCodeMsg{T: tBadCode, AttemptsLeft: 0}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"attempts_left":0`)) {
		t.Fatalf("attempts_left 0 must be sent explicitly: %s", buf.Bytes()[4:])
	}
	m, err := readMsg(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if m.T != tBadCode || m.AttemptsLeft != 0 {
		t.Fatalf("decoded %+v", m)
	}
	buf.Reset()
	if err := writeMsg(&buf, doneMsg{T: tDone, Done: Done{OK: true, Entries: 3, Bytes: 99}}); err != nil {
		t.Fatal(err)
	}
	if got := string(buf.Bytes()[4:]); got != `{"t":"done","ok":true,"entries":3,"bytes":99}` {
		t.Fatalf("done wire form %s", got)
	}
	buf.Reset()
	if err := writeMsg(&buf, doneMsg{T: tDone, Done: Done{OK: false, Reason: "boom"}}); err != nil {
		t.Fatal(err)
	}
	if got := string(buf.Bytes()[4:]); got != `{"t":"done","ok":false,"reason":"boom"}` {
		t.Fatalf("done wire form %s", got)
	}
	// A control message over the cap is refused before it hits the wire.
	buf.Reset()
	err = writeMsg(&buf, rejectMsg{T: tReject, Reason: strings.Repeat("x", maxControlFrame)})
	if err == nil || buf.Len() != 0 {
		t.Fatalf("oversize control message: err=%v written=%d", err, buf.Len())
	}
	// The manifest frame carries raw bytes up to 16 MiB.
	buf.Reset()
	raw := []byte("{\n  \"format\": 1\n}")
	if err := writeFrame(&buf, raw, maxManifestFrame); err != nil {
		t.Fatal(err)
	}
	back, err := readFrame(&buf, maxManifestFrame)
	if err != nil || !bytes.Equal(back, raw) {
		t.Fatalf("manifest frame round trip: %v %q", err, back)
	}
}

func TestCleanText(t *testing.T) {
	in := "mac-b\x1b]52;c;SGVsbG8=\x07\r\n​" + strings.Repeat("y", 100)
	got := cleanText(in, 64)
	if strings.ContainsAny(got, "\x1b\x07\r\n") || strings.Contains(got, "​") {
		t.Fatalf("control characters survived: %q", got)
	}
	if !strings.HasPrefix(got, "mac-b]52;c;SGVsbG8=") || !strings.HasSuffix(got, "…") {
		t.Fatalf("cleanText = %q", got)
	}
	if cleanText("plain", 10) != "plain" {
		t.Fatal("plain text altered")
	}
}
