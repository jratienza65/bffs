package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests in this file attack the implementation against plan §8: what a
// stranger, a hostile LAN host or a broken peer can do to A or B, and what
// they must never achieve.

// rawDial opens a TLS connection to A with cfg (ClientTLS(nil) for a
// well-behaved peer) and returns it; the test drives the protocol by hand.
func rawDial(t *testing.T, addr netip.AddrPort, cfg *tls.Config) (*tls.Conn, net.Conn) {
	t.Helper()
	raw, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	tconn := tls.Client(raw, cfg)
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tconn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = raw.SetDeadline(time.Time{})
	return tconn, raw
}

// rawPair runs the pairing steps a well-behaved B would: HELLO with the
// right proof, auth-ok verified, manifest received.
func rawPair(t *testing.T, tconn *tls.Conn, code Code) (msg, []byte) {
	t.Helper()
	ekm, err := exportKeyingMaterial(tconn.ConnectionState())
	if err != nil {
		t.Fatal(err)
	}
	k := DeriveKey(code, ekm)
	hello := helloMsg{T: tHello, V: protocolVersion, Formats: []int{bundleFormat}, Proof: base64.StdEncoding.EncodeToString(ClientProof(k, ekm))}
	if err := writeMsg(tconn, hello); err != nil {
		t.Fatal(err)
	}
	m, err := readMsg(tconn)
	if err != nil {
		t.Fatalf("reading A's answer: %v", err)
	}
	if m.T != tAuthOK {
		t.Fatalf("A answered %q, want auth-ok", m.T)
	}
	proofA, _ := base64.StdEncoding.DecodeString(m.Proof)
	if !VerifyProof(ServerProof(k, ekm), proofA) {
		t.Fatal("A's proof does not verify")
	}
	manifest, err := readFrame(tconn, maxManifestFrame)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return m, manifest
}

// expectClosed reads once and requires the peer to have gone away.
func expectClosed(t *testing.T, tconn *tls.Conn, what string) {
	t.Helper()
	_ = tconn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := readMsg(tconn); err == nil {
		t.Fatalf("%s: A kept the connection open and answered", what)
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%s: A neither answered nor closed within 5 s", what)
	}
}

func waitEvent(t *testing.T, rec *recorder, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(rec.text(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("no event containing %q: %s", substr, rec.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestOnlyWellFormedHelloBurnsAttempts drives A by hand: it must say nothing
// before HELLO, close on every malformed HELLO without spending an attempt,
// verify exactly one proof per connection, and present a certificate whose
// NotAfter is the TTL.
func TestOnlyWellFormedHelloBurnsAttempts(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	var srec recorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := baseServe(t, code, testBody(1024), &srec)
	o.TTL = 2 * time.Minute
	started := time.Now()
	addr, out := startServe(t, ctx, o)

	// A is silent after the handshake: B speaks first.
	tconn, raw := rawDial(t, addr, ClientTLS(nil))
	leaf := tconn.ConnectionState().PeerCertificates[0]
	if d := leaf.NotAfter.Sub(started.Add(o.TTL)); d < -5*time.Second || d > 5*time.Second {
		t.Fatalf("certificate NotAfter %s is not now+TTL (%s off)", leaf.NotAfter, d)
	}
	_ = raw.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var one [1]byte
	if _, err := tconn.Read(one[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("A sent something before HELLO (err=%v)", err)
	}
	_ = raw.SetReadDeadline(time.Time{})
	raw.Close()

	malformed := []struct {
		name string
		send func(*tls.Conn) error
		want string
	}{
		{"not json", func(c *tls.Conn) error { return writeFrame(c, []byte("not json"), maxControlFrame) }, "no valid hello"},
		{"no t", func(c *tls.Conn) error { return writeFrame(c, []byte(`{"v":1}`), maxControlFrame) }, "no valid hello"},
		{"wrong type", func(c *tls.Conn) error { return writeMsg(c, pingMsg{T: tPing}) }, "unsupported hello"},
		{"future version", func(c *tls.Conn) error {
			return writeMsg(c, helloMsg{T: tHello, V: 2, Formats: []int{1}, Proof: base64.StdEncoding.EncodeToString(make([]byte, 32))})
		}, "unsupported hello"},
		{"no shared format", func(c *tls.Conn) error {
			return writeMsg(c, helloMsg{T: tHello, V: 1, Formats: []int{7}, Proof: base64.StdEncoding.EncodeToString(make([]byte, 32))})
		}, "unsupported hello"},
		{"short proof", func(c *tls.Conn) error {
			return writeMsg(c, helloMsg{T: tHello, V: 1, Formats: []int{1}, Proof: "AAAA"})
		}, "malformed proof"},
		{"proof not base64", func(c *tls.Conn) error {
			return writeMsg(c, helloMsg{T: tHello, V: 1, Formats: []int{1}, Proof: "***"})
		}, "malformed proof"},
		{"oversize frame", func(c *tls.Conn) error {
			return writeFrame(c, bytes.Repeat([]byte("x"), maxControlFrame+1), maxManifestFrame)
		}, "no valid hello"},
	}
	for _, tc := range malformed {
		tconn, _ := rawDial(t, addr, ClientTLS(nil))
		if err := tc.send(tconn); err != nil {
			t.Fatalf("%s: send: %v", tc.name, err)
		}
		expectClosed(t, tconn, tc.name)
		waitEvent(t, &srec, tc.want)
		if srec.count(kindBadCode) != 0 {
			t.Fatalf("%s burned an attempt: %s", tc.name, srec.text())
		}
	}

	// A well-formed HELLO with a wrong proof burns one attempt and ends the
	// connection: a second HELLO on it is never verified.
	tconn, _ = rawDial(t, addr, ClientTLS(nil))
	wrong := make([]byte, 32)
	rand.Read(wrong)
	hello := helloMsg{T: tHello, V: 1, Formats: []int{1}, Proof: base64.StdEncoding.EncodeToString(wrong)}
	if err := writeMsg(tconn, hello); err != nil {
		t.Fatal(err)
	}
	m, err := readMsg(tconn)
	if err != nil || m.T != tBadCode || m.AttemptsLeft != 2 {
		t.Fatalf("wrong proof: %+v %v, want bad-code with 2 attempts left", m, err)
	}
	_ = writeMsg(tconn, hello)
	expectClosed(t, tconn, "second hello on one connection")

	cancel()
	r := waitServe(t, out, 10*time.Second)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Serve: %v", r.err)
	}
	if r.res.Attempts != 1 || srec.count(kindBadCode) != 1 {
		t.Fatalf("attempts %d, bad-code events %d, want 1 and 1: %s", r.res.Attempts, srec.count(kindBadCode), srec.text())
	}
	assertNoCode(t, code, "raw hello", srec.text())
}

// TestHelloCarriesNoIdentity captures what B sends first: only the proof
// and the protocol facts, never who B is (that travels in accept, after A
// proved itself).
func TestHelloCarriesNoIdentity(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	cert, _, err := EphemeralCert(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	helloCh := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		tc := tls.Server(c, ServerTLS(cert))
		if err := tc.Handshake(); err != nil {
			return
		}
		b, err := readFrame(tc, maxControlFrame)
		if err != nil {
			return
		}
		helloCh <- b
	}()
	fo := baseFetch(netip.MustParseAddrPort(ln.Addr().String()), code, &bytes.Buffer{}, &recorder{})
	fo.Host, fo.User, fo.Version = "mac-b-secret-host", "jonas-secret-user", "9.9.9-secret"
	if _, err := Fetch(context.Background(), fo); err == nil {
		t.Fatal("Fetch succeeded against a server that never answered")
	}
	var raw []byte
	select {
	case raw = <-helloCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no HELLO captured")
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"t", "v", "formats", "proof"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("HELLO lacks %q: %s", k, raw)
		}
	}
	if len(fields) != 4 {
		t.Errorf("HELLO carries extra fields: %s", raw)
	}
	for _, secret := range []string{"secret", "mac-b", "jonas", "host", "user", "bffs"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("HELLO carries %q: %s", secret, raw)
		}
	}
	assertNoCode(t, code, "hello", string(raw))
}

func TestServerRequiresALPN(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	var srec recorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startServe(t, ctx, baseServe(t, code, testBody(1024), &srec))

	// A TLS 1.3 client that never offers our protocol: the handshake may
	// complete (Go servers tolerate a client without ALPN) but A must then
	// hang up before reading anything.
	raw, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tconn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tconn.Handshake(); err == nil {
		wrong := make([]byte, 32)
		rand.Read(wrong)
		_ = writeMsg(tconn, helloMsg{T: tHello, V: 1, Formats: []int{1}, Proof: base64.StdEncoding.EncodeToString(wrong)})
		if _, err := readMsg(tconn); err == nil {
			t.Fatal("A answered a HELLO on a connection without ALPN")
		}
		waitEvent(t, &srec, "did not negotiate bffs-transfer/1")
	} else {
		waitEvent(t, &srec, "TLS handshake failed")
	}

	// A hostile ALPN list (Go's own error echoes it) never reaches an
	// event unsanitised.
	raw2, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	_ = raw2.SetDeadline(time.Now().Add(5 * time.Second))
	evil := tls.Client(raw2, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, NextProtos: []string{"\x1b]52;c;ZXZpbA==\x07"}})
	if err := evil.Handshake(); err == nil {
		t.Fatal("handshake with a foreign ALPN succeeded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for srec.count(kindError) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("second failure not logged: %s", srec.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.ContainsAny(srec.text(), "\x1b\x07") {
		t.Fatalf("terminal escapes reached the events: %q", srec.text())
	}

	cancel()
	r := waitServe(t, out, 10*time.Second)
	if r.res.Attempts != 0 {
		t.Fatalf("connections without ALPN burned %d attempts", r.res.Attempts)
	}
}

func TestClientRejectsCertChainOfTwo(t *testing.T) {
	c1, _, err := EphemeralCert(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c2, _, err := EphemeralCert(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	chain := tls.Certificate{Certificate: [][]byte{c1.Certificate[0], c2.Certificate[0]}, PrivateKey: c1.PrivateKey, Leaf: c1.Leaf}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = tls.Server(c, ServerTLS(chain)).Handshake()
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	seen := false
	err = tls.Client(raw, ClientTLS(func([32]byte) { seen = true })).Handshake()
	if err == nil || !strings.Contains(err.Error(), "exactly one peer certificate") {
		t.Fatalf("handshake with a two-certificate chain: %v", err)
	}
	if seen {
		t.Fatal("onLeaf called for a rejected chain")
	}
}

func TestHelloStallClosedAtDeadline(t *testing.T) {
	fastKDF(t)
	withTimeouts(t, func(ts *timeoutSet) { ts.Hello = 300 * time.Millisecond })
	code := mustCode(t)
	body := testBody(4096)
	var srec recorder
	addr, out := startServe(t, context.Background(), baseServe(t, code, body, &srec))

	// A peer that handshakes and then says nothing is dropped at the HELLO
	// deadline, and the serve is not held by it.
	tconn, _ := rawDial(t, addr, ClientTLS(nil))
	start := time.Now()
	expectClosed(t, tconn, "stalled hello")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("stalled HELLO held the connection for %s", took)
	}
	waitEvent(t, &srec, "no valid hello")

	var got bytes.Buffer
	d, err := Fetch(context.Background(), baseFetch(addr, code, &got, &recorder{}))
	if err != nil || !d.OK || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("fetch after a stalled peer: %+v %v", d, err)
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil || r.res.Attempts != 1 {
		t.Fatalf("Serve: %v attempts %d", r.err, r.res.Attempts)
	}
}

func TestConsentDeadlineRefreshedByPings(t *testing.T) {
	fastKDF(t)
	withTimeouts(t, func(ts *timeoutSet) { ts.Consent = 400 * time.Millisecond; ts.Ping = 100 * time.Millisecond })
	code := mustCode(t)
	body := testBody(4096)
	var srec recorder
	addr, out := startServe(t, context.Background(), baseServe(t, code, body, &srec))
	var got bytes.Buffer
	fo := baseFetch(addr, code, &got, &recorder{})
	fo.Confirm = func([]byte) (bool, error) {
		time.Sleep(1200 * time.Millisecond) // three consent deadlines
		return true, nil
	}
	d, err := Fetch(context.Background(), fo)
	if err != nil || !d.OK || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("slow consent with pings: %+v %v", d, err)
	}
	if r := waitServe(t, out, 10*time.Second); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestConsentDeadlineWithoutPings(t *testing.T) {
	fastKDF(t)
	withTimeouts(t, func(ts *timeoutSet) { ts.Consent = 400 * time.Millisecond })
	code := mustCode(t)
	body := testBody(4096)
	var srec recorder
	addr, out := startServe(t, context.Background(), baseServe(t, code, body, &srec))

	// An authenticated peer that never answers (and never pings) is dropped
	// at the consent deadline; the slot goes back and A keeps serving.
	tconn, _ := rawDial(t, addr, ClientTLS(nil))
	rawPair(t, tconn, code)
	start := time.Now()
	expectClosed(t, tconn, "silent consent")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("silent consent held the connection for %s", took)
	}
	waitEvent(t, &srec, "connection closed before it answered")

	var got bytes.Buffer
	d, err := Fetch(context.Background(), baseFetch(addr, code, &got, &recorder{}))
	if err != nil || !d.OK || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("fetch after a silent peer: %+v %v", d, err)
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil || r.res.Attempts != 2 || !r.res.Done.OK {
		t.Fatalf("Serve: %v %+v", r.err, r.res)
	}
}

func TestDoneDeadline(t *testing.T) {
	fastKDF(t)
	withTimeouts(t, func(ts *timeoutSet) { ts.Done = 300 * time.Millisecond })
	code := mustCode(t)
	body := testBody(64 << 10)
	var srec recorder
	addr, out := startServe(t, context.Background(), baseServe(t, code, body, &srec))
	fo := baseFetch(addr, code, &bytes.Buffer{}, &recorder{})
	fo.Sink = func(ctx context.Context, manifest []byte, r io.Reader) (Done, error) {
		if _, err := io.CopyN(io.Discard, r, manifestBytes(t, manifest)); err != nil {
			return Done{}, err
		}
		time.Sleep(1200 * time.Millisecond) // the report comes far too late
		return Done{Entries: 3, Bytes: int64(len(body))}, nil
	}
	_, _ = Fetch(context.Background(), fo) // its late report may or may not get out
	r := waitServe(t, out, 10*time.Second)
	if r.err == nil || !strings.Contains(r.err.Error(), "never reported completion") {
		t.Fatalf("Serve: %v, want the DONE deadline", r.err)
	}
	if r.res.Bytes != int64(len(body)) || r.res.Done.OK {
		t.Fatalf("ServeResult: %+v", r.res)
	}
	waitEvent(t, &srec, "no completion report")
}

func TestUnknownMessageWhileStreamingAborts(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(8 << 20)
	var srec recorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := baseServe(t, code, body, &srec)
	o.Body = writeBody(body, 1<<20)
	addr, out := startServe(t, ctx, o)

	tconn, _ := rawDial(t, addr, ClientTLS(nil))
	rawPair(t, tconn, code)
	if err := writeMsg(tconn, acceptMsg{T: tAccept, V: protocolVersion, Bffs: "raw", Host: "mac-raw", User: "u"}); err != nil {
		t.Fatal(err)
	}
	// Keep reading so A's writer is never blocked on us, then break the
	// protocol mid-stream.
	go io.Copy(io.Discard, tconn)
	if err := writeMsg(tconn, pingMsg{T: "bogus"}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, &srec, `transfer aborted by peer mac-raw (unexpected "bogus" while streaming)`)
	select {
	case r := <-out:
		t.Fatalf("Serve ended on a protocol break: %+v %v", r.res, r.err)
	case <-time.After(200 * time.Millisecond):
	}
	// A kept serving: the next peer gets the bundle.
	var got bytes.Buffer
	d, err := Fetch(context.Background(), baseFetch(addr, code, &got, &recorder{}))
	if err != nil || !d.OK || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("fetch after a protocol break: %+v %v", d, err)
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil || !r.res.Done.OK {
		t.Fatalf("Serve: %v %+v", r.err, r.res.Done)
	}
}

func TestPeerTextSanitised(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(4096)
	var srec recorder
	var ui bytes.Buffer
	o := baseServe(t, code, body, &srec)
	o.UI = &ui
	addr, out := startServe(t, context.Background(), o)

	tconn, _ := rawDial(t, addr, ClientTLS(nil))
	_, manifest := rawPair(t, tconn, code)
	hostile := "mac-b\x1b]52;c;ZXZpbA==\x07\r\n" + strings.Repeat("h", 200)
	if err := writeMsg(tconn, acceptMsg{T: tAccept, V: protocolVersion, Bffs: "x", Host: hostile, User: hostile}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, tconn, manifestBytes(t, manifest)); err != nil {
		t.Fatal(err)
	}
	if err := writeMsg(tconn, doneMsg{T: tDone, Done: Done{OK: false, Reason: hostile}}); err != nil {
		t.Fatal(err)
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err == nil {
		t.Fatal("Serve succeeded on a failure report")
	}
	texts := map[string]string{"error": r.err.Error(), "PeerHost": r.res.PeerHost, "PeerUser": r.res.PeerUser, "Reason": r.res.Done.Reason}
	srec.mu.Lock()
	for i, e := range srec.events {
		texts[fmt.Sprintf("event %d text", i)] = e.Text
		texts[fmt.Sprintf("event %d peer", i)] = e.Peer
	}
	srec.mu.Unlock()
	for what, s := range texts {
		if strings.ContainsAny(s, "\x1b\x07\r\n\x00") {
			t.Errorf("%s carries control characters: %q", what, s)
		}
	}
	// The UI writer is line-oriented by design, so only the escapes matter.
	if strings.ContainsAny(ui.String(), "\x1b\x07\x00") {
		t.Errorf("ui carries control characters: %q", ui.String())
	}
	if len([]rune(r.res.PeerHost)) > 65 || !strings.HasPrefix(r.res.PeerHost, "mac-b]52;c;") {
		t.Fatalf("PeerHost = %q", r.res.PeerHost)
	}
}

// flakyListener fails the first n Accepts with a transient error, the way
// a kernel does under a connection flood (EMFILE) or an aborted SYN.
type flakyListener struct {
	net.Listener
	left atomic.Int32
}

func (f *flakyListener) Accept() (net.Conn, error) {
	if f.left.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: errors.New("too many open files")}
	}
	return f.Listener.Accept()
}

func TestAcceptLoopSurvivesTransientErrors(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(4096)
	var srec recorder
	o := baseServe(t, code, body, &srec)
	addrCh := make(chan netip.AddrPort, 1)
	inner := loopbackListen(addrCh)
	o.Listen = func(network, addr string) (net.Listener, error) {
		ln, err := inner(network, addr)
		if err != nil {
			return nil, err
		}
		f := &flakyListener{Listener: ln}
		f.left.Store(3)
		return f, nil
	}
	o.Local = loopbackLocal()
	o.LAN.AllowLoopback = true
	o.Now = func() time.Time { return testClock }
	out := make(chan serveOut, 1)
	go func() {
		res, err := Serve(context.Background(), o)
		out <- serveOut{res, err}
	}()
	addr := <-addrCh
	var got bytes.Buffer
	d, err := Fetch(context.Background(), baseFetch(addr, code, &got, &recorder{}))
	if err != nil || !d.OK || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("fetch behind transient accept errors: %+v %v", d, err)
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.Contains(srec.text(), "accept on "+addr.String()+" failed, retrying: accept tcp: too many open files") {
		t.Fatalf("events: %s", srec.text())
	}
}

func TestClaimRefusesAfterExpiry(t *testing.T) {
	peer := netip.MustParseAddrPort("127.0.0.1:1")
	s := &server{}
	s.pairCtx, s.cancelPair = context.WithCancel(context.Background())
	var c1, c2, c3 bool
	if ok, expired := s.claim(peer, &c1); !ok || expired || !c1 {
		t.Fatalf("first claim: ok=%v expired=%v flag=%v", ok, expired, c1)
	}
	if ok, expired := s.claim(peer, &c2); ok || expired || c2 {
		t.Fatalf("second claim while active: ok=%v expired=%v flag=%v", ok, expired, c2)
	}
	s.cancelPair()
	s.active = false
	if ok, expired := s.claim(peer, &c3); ok || !expired || c3 {
		t.Fatalf("claim after the window closed: ok=%v expired=%v flag=%v", ok, expired, c3)
	}
	s.finished = true
	if ok, expired := s.claim(peer, &c3); ok || expired {
		t.Fatalf("claim after finish: ok=%v expired=%v", ok, expired)
	}
	if s.attempts != 4 {
		t.Fatalf("attempts = %d, want every verified proof counted", s.attempts)
	}
}

func TestDeriveKeySerialisedAndStopsWhenDone(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	ekm := bytes.Repeat([]byte{9}, 32)

	kdfMu.Lock()
	done := make(chan struct{})
	go func() {
		DeriveKey(code, ekm)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("DeriveKey ran without the KDF lock")
	case <-time.After(150 * time.Millisecond):
	}
	kdfMu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DeriveKey never ran once the lock was free")
	}

	// A caller whose context ended while it queued pays nothing: with the
	// production parameters restored this would otherwise take ~0.2 s.
	kdfParams.Time, kdfParams.MemoryKiB, kdfParams.Threads = 3, 64*1024, 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	k, err := deriveKey(ctx, code, ekm)
	if err == nil || k != nil {
		t.Fatalf("deriveKey with a done ctx: key=%v err=%v", k != nil, err)
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Fatalf("deriveKey with a done ctx still computed (%s)", took)
	}
}

// TestNoGoroutineLeaks runs the main outcomes and requires every goroutine
// the package started to be gone once Serve and Fetch have returned.
func TestNoGoroutineLeaks(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(256 << 10)

	settle := func(before int, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if n := runtime.NumGoroutine(); n > before {
			var buf bytes.Buffer
			_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)
			t.Fatalf("%s: %d goroutines before, %d after:\n%s", what, before, n, buf.String())
		}
	}

	before := runtime.NumGoroutine()
	{
		var got bytes.Buffer
		addr, out := startServe(t, context.Background(), baseServe(t, code, body, &recorder{}))
		if d, err := Fetch(context.Background(), baseFetch(addr, code, &got, &recorder{})); err != nil || !d.OK {
			t.Fatalf("right code: %+v %v", d, err)
		}
		if r := waitServe(t, out, 10*time.Second); r.err != nil {
			t.Fatal(r.err)
		}
	}
	settle(before, "right code")

	before = runtime.NumGoroutine()
	{
		ctx, cancel := context.WithCancel(context.Background())
		addr, out := startServe(t, ctx, baseServe(t, code, body, &recorder{}))
		if _, err := Fetch(context.Background(), baseFetch(addr, wrongCode(t, code), &bytes.Buffer{}, &recorder{})); !errors.Is(err, ErrBadCode) {
			t.Fatalf("wrong code: %v", err)
		}
		cancel()
		if r := waitServe(t, out, 10*time.Second); !errors.Is(r.err, context.Canceled) {
			t.Fatal(r.err)
		}
	}
	settle(before, "wrong code then cancel")

	before = runtime.NumGoroutine()
	{
		o := baseServe(t, code, body, &recorder{})
		o.TTL = 200 * time.Millisecond
		o.UI = io.Discard
		_, out := startServe(t, context.Background(), o)
		if r := waitServe(t, out, 10*time.Second); !errors.Is(r.err, ErrExpired) {
			t.Fatal(r.err)
		}
	}
	settle(before, "expiry")

	before = runtime.NumGoroutine()
	{
		ctx, cancel := context.WithCancel(context.Background())
		addr, out := startServe(t, ctx, baseServe(t, code, body, &recorder{}))
		fo := baseFetch(addr, code, &bytes.Buffer{}, &recorder{})
		fo.Confirm = func([]byte) (bool, error) { return false, nil }
		if _, err := Fetch(context.Background(), fo); !errors.Is(err, ErrRejected) {
			t.Fatalf("decline: %v", err)
		}
		cancel()
		if r := waitServe(t, out, 10*time.Second); !errors.Is(r.err, context.Canceled) {
			t.Fatal(r.err)
		}
	}
	settle(before, "decline then cancel")

	before = runtime.NumGoroutine()
	{
		ctx, cancel := context.WithCancel(context.Background())
		addr, out := startServe(t, ctx, baseServe(t, code, testBody(32<<20), &recorder{}))
		fo := baseFetch(addr, code, &bytes.Buffer{}, &recorder{})
		fo.Sink = func(ctx context.Context, manifest []byte, r io.Reader) (Done, error) {
			if _, err := io.CopyN(io.Discard, r, 1<<20); err != nil {
				return Done{}, err
			}
			return Done{}, errors.New("sha256 mismatch")
		}
		if _, err := Fetch(context.Background(), fo); err == nil {
			t.Fatal("early failure: Fetch succeeded")
		}
		cancel()
		if r := waitServe(t, out, 15*time.Second); r.err == nil {
			t.Fatal("early failure: Serve succeeded")
		}
	}
	settle(before, "early done{ok:false}")
}

// dualLoopback binds 127.0.0.1 and ::1 on one port through the real
// net.Listen, so a serve has two listeners to close and to keep.
func dualLoopback(t *testing.T) ([]LinkAddr, Listener) {
	t.Helper()
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	probe.Close()
	local := []LinkAddr{
		{Addr: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8"), Iface: "lo"},
		{Addr: netip.MustParseAddr("::1"), Prefix: netip.MustParsePrefix("::1/128"), Iface: "lo"},
	}
	return local, net.Listen
}

func TestEveryListenerClosesOnExhaustionAndStaysOpenAfterDecline(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(4096)
	local, listen := dualLoopback(t)
	run := func(t *testing.T, ttl time.Duration) ([]netip.AddrPort, <-chan serveOut) {
		t.Helper()
		var srec recorder
		o := baseServe(t, code, body, &srec)
		o.TTL = ttl
		o.Local, o.Listen = local, listen
		o.LAN.AllowLoopback = true
		o.Now = func() time.Time { return testClock }
		out := make(chan serveOut, 1)
		var rec recorder
		o.Events = func(e Event) { rec.add(e); srec.add(e) }
		go func() {
			res, err := Serve(context.Background(), o)
			out <- serveOut{res, err}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for rec.count(kindListen) < 2 {
			if time.Now().After(deadline) {
				t.Fatalf("two listeners never came up: %s", rec.text())
			}
			time.Sleep(10 * time.Millisecond)
		}
		var addrs []netip.AddrPort
		rec.mu.Lock()
		for _, e := range rec.events {
			if e.Kind == kindListen {
				addrs = append(addrs, netip.MustParseAddrPort(e.Peer))
			}
		}
		rec.mu.Unlock()
		if len(addrs) != 2 || addrs[0].Port() != addrs[1].Port() {
			t.Fatalf("listeners %v, want two sharing one port", addrs)
		}
		return addrs, out
	}
	reachable := func(addr netip.AddrPort) bool {
		c, err := net.DialTimeout("tcp", addr.String(), time.Second)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}

	t.Run("decline keeps both", func(t *testing.T) {
		addrs, out := run(t, 800*time.Millisecond)
		fo := baseFetch(addrs[1], code, &bytes.Buffer{}, &recorder{})
		fo.Confirm = func([]byte) (bool, error) { return false, nil }
		if _, err := Fetch(context.Background(), fo); !errors.Is(err, ErrRejected) {
			t.Fatalf("decline over ::1: %v", err)
		}
		for _, a := range addrs {
			if !reachable(a) {
				t.Fatalf("%s stopped accepting after a decline on %s", a, addrs[1])
			}
		}
		// And the other address still delivers.
		var got bytes.Buffer
		if d, err := Fetch(context.Background(), baseFetch(addrs[0], code, &got, &recorder{})); err != nil || !d.OK || !bytes.Equal(got.Bytes(), body) {
			t.Fatalf("delivery over 127.0.0.1 after a decline over ::1: %+v %v", d, err)
		}
		if r := waitServe(t, out, 10*time.Second); r.err != nil {
			t.Fatal(r.err)
		}
	})

	t.Run("exhaustion closes both", func(t *testing.T) {
		addrs, out := run(t, time.Minute)
		for i := range 3 {
			if _, err := Fetch(context.Background(), baseFetch(addrs[i%2], wrongCode(t, code), &bytes.Buffer{}, &recorder{})); !errors.Is(err, ErrBadCode) {
				t.Fatalf("wrong code %d: %v", i, err)
			}
		}
		r := waitServe(t, out, 10*time.Second)
		if !errors.Is(r.err, ErrTooManyAttempts) {
			t.Fatalf("Serve: %v", r.err)
		}
		for _, a := range addrs {
			if reachable(a) {
				t.Fatalf("%s still accepts after the attempts were spent", a)
			}
		}
	})
}
