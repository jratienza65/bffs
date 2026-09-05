package transfer

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// maxControlFrame caps every JSON control message.
	maxControlFrame = 4 * 1024
	// maxManifestFrame caps the one raw manifest frame.
	maxManifestFrame = 16 << 20
)

// readFrame reads one length-prefixed frame. The length is checked against
// max before anything is allocated; a short body is an error.
func readFrame(r io.Reader, max int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("truncated frame header: %w", io.ErrUnexpectedEOF)
		}
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > uint32(max) {
		return nil, fmt.Errorf("frame of %d bytes exceeds the %d-byte cap", n, max)
	}
	buf := make([]byte, n)
	if got, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("truncated frame (%d of %d bytes): %w", got, n, io.ErrUnexpectedEOF)
		}
		return nil, err
	}
	return buf, nil
}

// writeFrame writes header and payload in one Write call.
func writeFrame(w io.Writer, payload []byte, max int) error {
	if len(payload) > max {
		return fmt.Errorf("frame of %d bytes exceeds the %d-byte cap", len(payload), max)
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[4:], payload)
	_, err := w.Write(buf)
	return err
}

// msg is the union of every control message's fields; each side reads into
// it and switches on T. Unknown fields are ignored (a newer bffs may add
// some); a missing "t" is an error.
type msg struct {
	T              string `json:"t"`
	V              int    `json:"v,omitempty"`
	Formats        []int  `json:"formats,omitempty"`
	Proof          string `json:"proof,omitempty"`
	AttemptsLeft   int    `json:"attempts_left"`
	Bffs           string `json:"bffs,omitempty"`
	Host           string `json:"host,omitempty"`
	User           string `json:"user,omitempty"`
	Account        string `json:"account,omitempty"`
	Compression    byte   `json:"compression"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
	Reason         string `json:"reason,omitempty"`
	OK             bool   `json:"ok"`
	Entries        int    `json:"entries,omitempty"`
	Bytes          int64  `json:"bytes,omitempty"`
}

// Wire shapes for the messages each side writes. Kept separate from msg so
// every message carries exactly the documented fields.
type helloMsg struct {
	T       string `json:"t"`
	V       int    `json:"v"`
	Formats []int  `json:"formats"`
	Proof   string `json:"proof"`
}

type badCodeMsg struct {
	T            string `json:"t"`
	AttemptsLeft int    `json:"attempts_left"`
}

type authOKMsg struct {
	T              string `json:"t"`
	V              int    `json:"v"`
	Bffs           string `json:"bffs"`
	Host           string `json:"host"`
	User           string `json:"user"`
	Account        string `json:"account"`
	Proof          string `json:"proof"`
	Compression    byte   `json:"compression"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

type pingMsg struct {
	T string `json:"t"`
}

type acceptMsg struct {
	T    string `json:"t"`
	V    int    `json:"v"`
	Bffs string `json:"bffs"`
	Host string `json:"host"`
	User string `json:"user"`
}

type rejectMsg struct {
	T      string `json:"t"`
	Reason string `json:"reason"`
	Host   string `json:"host,omitempty"`
	User   string `json:"user,omitempty"`
}

type doneMsg struct {
	T string `json:"t"`
	Done
}

// writeMsg marshals v and writes it as a control frame.
func writeMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, b, maxControlFrame)
}

// readMsg reads one control frame and decodes it.
func readMsg(r io.Reader) (msg, error) {
	b, err := readFrame(r, maxControlFrame)
	if err != nil {
		return msg{}, err
	}
	var m msg
	if err := json.Unmarshal(b, &m); err != nil {
		return msg{}, fmt.Errorf("malformed control message: %w", err)
	}
	if m.T == "" {
		return msg{}, errors.New("malformed control message: missing \"t\"")
	}
	return m, nil
}

// Message type tags.
const (
	tHello   = "hello"
	tBadCode = "bad-code"
	tAuthOK  = "auth-ok"
	tPing    = "ping"
	tAccept  = "accept"
	tReject  = "reject"
	tDone    = "done"
)

const (
	protocolVersion = 1
	bundleFormat    = 1
	reasonDeclined  = "declined"
	reasonDryRun    = "dry-run"
)
