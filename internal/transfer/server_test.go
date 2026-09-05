package transfer

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

var testClock = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// recorder collects events from either side.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) add(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func (r *recorder) has(kind string) bool {
	for _, k := range r.kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func (r *recorder) count(kind string) int {
	n := 0
	for _, k := range r.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

func (r *recorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.events {
		fmt.Fprintf(&b, "%s %s %s\n", e.Kind, e.Peer, e.Text)
	}
	return b.String()
}

// assertNoCode fails when any of the strings carries the code in either
// form.
func assertNoCode(t *testing.T, code Code, what string, strs ...string) {
	t.Helper()
	for _, s := range strs {
		if strings.Contains(s, code.s) || strings.Contains(s, code.Display()) {
			t.Errorf("%s leaks the pairing code: %q", what, s)
		}
	}
}

func loopbackLocal() []LinkAddr {
	return []LinkAddr{{Addr: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8"), Iface: "lo"}}
}

// testBody is a deterministic pseudo-random bundle of n bytes.
func testBody(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{1, 2, 3}).Read(b)
	return b
}

// testManifest announces the body size so a Sink knows where it ends.
func testManifest(body []byte) []byte {
	m, _ := json.Marshal(map[string]any{"format": 1, "bytes": len(body), "entries": 3})
	return m
}

func manifestBytes(t *testing.T, manifest []byte) int64 {
	t.Helper()
	var m struct {
		Bytes int64 `json:"bytes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return m.Bytes
}

// writeBody streams body in chunks, honouring ctx.
func writeBody(body []byte, chunk int) func(ctx context.Context, w io.Writer) error {
	return func(ctx context.Context, w io.Writer) error {
		for off := 0; off < len(body); off += chunk {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := min(off+chunk, len(body))
			if _, err := w.Write(body[off:end]); err != nil {
				return err
			}
		}
		return nil
	}
}

// bufferSink reads exactly the announced number of bytes.
func bufferSink(buf *bytes.Buffer) func(context.Context, []byte, io.Reader) (Done, error) {
	return func(ctx context.Context, manifest []byte, body io.Reader) (Done, error) {
		var m struct {
			Bytes   int64 `json:"bytes"`
			Entries int   `json:"entries"`
		}
		if err := json.Unmarshal(manifest, &m); err != nil {
			return Done{}, err
		}
		n, err := io.CopyN(buf, body, m.Bytes)
		if err != nil {
			return Done{Bytes: n}, err
		}
		return Done{Entries: m.Entries, Bytes: n}, nil
	}
}

type serveOut struct {
	res ServeResult
	err error
}

// loopbackListen honours the requested port on 127.0.0.1 and reports the
// bound address on addrCh.
func loopbackListen(addrCh chan<- netip.AddrPort) Listener {
	return func(network, addr string) (net.Listener, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
		if err != nil {
			return nil, err
		}
		select {
		case addrCh <- netip.MustParseAddrPort(ln.Addr().String()):
		default:
		}
		return ln, nil
	}
}

// startServe runs Serve in the background on a loopback listener and returns
// the bound address and the result channel.
func startServe(t *testing.T, ctx context.Context, o ServeOptions) (netip.AddrPort, <-chan serveOut) {
	t.Helper()
	addrCh := make(chan netip.AddrPort, 1)
	o.Listen = loopbackListen(addrCh)
	if o.Local == nil {
		o.Local = loopbackLocal()
	}
	if o.Now == nil {
		o.Now = func() time.Time { return testClock }
	}
	o.LAN.AllowLoopback = true
	out := make(chan serveOut, 1)
	go func() {
		res, err := Serve(ctx, o)
		out <- serveOut{res, err}
	}()
	select {
	case addr := <-addrCh:
		return addr, out
	case r := <-out:
		t.Fatalf("Serve returned before listening: %+v %v", r.res, r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never bound a listener")
	}
	return netip.AddrPort{}, nil
}

func waitServe(t *testing.T, out <-chan serveOut, within time.Duration) serveOut {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(within):
		t.Fatal("Serve did not return in time")
	}
	return serveOut{}
}

func baseServe(t *testing.T, code Code, body []byte, rec *recorder) ServeOptions {
	t.Helper()
	return ServeOptions{
		Code:        code,
		TTL:         time.Minute,
		Manifest:    testManifest(body),
		Compression: 1,
		Body:        writeBody(body, 256*1024),
		Events:      rec.add,
		Version:     "test",
		Host:        "mac-a",
		User:        "jonas",
		Account:     "aviate",
	}
}

func baseFetch(addr netip.AddrPort, code Code, buf *bytes.Buffer, rec *recorder) FetchOptions {
	return FetchOptions{
		Addr:    addr,
		Code:    func() (Code, error) { return code, nil },
		Confirm: func([]byte) (bool, error) { return true, nil },
		Sink:    bufferSink(buf),
		Events:  rec.add,
		LAN:     LANOptions{AllowLoopback: true},
		Local:   loopbackLocal(),
		Version: "test",
		Host:    "mac-b",
		User:    "jonas",
	}
}

func mustCode(t *testing.T) Code {
	t.Helper()
	c, err := GenerateCode()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wrongCode(t *testing.T, right Code) Code {
	t.Helper()
	for {
		c := mustCode(t)
		if c != right {
			return c
		}
	}
}

func TestLoopbackRightCode(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(3 << 20)
	var srec, crec recorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startServe(t, ctx, baseServe(t, code, body, &srec))

	var got bytes.Buffer
	var gotManifest []byte
	fo := baseFetch(addr, code, &got, &crec)
	fo.Confirm = func(m []byte) (bool, error) { gotManifest = m; return true, nil }
	d, err := Fetch(context.Background(), fo)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !d.OK || d.Entries != 3 || d.Bytes != int64(len(body)) {
		t.Fatalf("Fetch done = %+v", d)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("Sink received %d bytes, differs from the %d-byte body", got.Len(), len(body))
	}
	if !bytes.Equal(gotManifest, testManifest(body)) {
		t.Fatalf("manifest differs: %s", gotManifest)
	}

	r := waitServe(t, out, 10*time.Second)
	if r.err != nil {
		t.Fatalf("Serve: %v", r.err)
	}
	res := r.res
	if !res.Done.OK || res.Done.Entries != 3 || res.Done.Bytes != int64(len(body)) {
		t.Fatalf("Serve done = %+v", res.Done)
	}
	if res.Bytes != int64(len(body)) || res.Attempts != 1 || res.PeerHost != "mac-b" || res.PeerUser != "jonas" {
		t.Fatalf("ServeResult = %+v", res)
	}
	if res.Peer.Addr() != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("peer %s", res.Peer)
	}
	if len(res.Listeners) != 1 || res.Listeners[0] != addr {
		t.Fatalf("Listeners = %v, want [%s]", res.Listeners, addr)
	}
	if res.SPKI == ([32]byte{}) {
		t.Fatal("SPKI not reported")
	}
	// The listen event carries what the banner needs: the bound address
	// and the key fingerprint, before any peer connects.
	listen := srec.events[0]
	if listen.Kind != kindListen || listen.Peer != addr.String() || !strings.Contains(listen.Text, "key "+hex.EncodeToString(res.SPKI[:])[:8]) {
		t.Fatalf("listen event = %+v", listen)
	}
	if res.Duration != 0 {
		t.Fatalf("Duration on a fixed clock = %s, want 0", res.Duration)
	}
	for _, k := range []string{kindListen, kindConnect, kindCodeOK, kindManifestSent, kindAccept, kindSending, kindDone} {
		if !srec.has(k) {
			t.Errorf("server events lack %q: %v", k, srec.kinds())
		}
	}
	for _, k := range []string{kindConnect, kindCodeOK, kindManifestSent, kindAccept, kindDone} {
		if !crec.has(k) {
			t.Errorf("client events lack %q: %v", k, crec.kinds())
		}
	}
	if !strings.Contains(crec.text(), "peer key ") {
		t.Errorf("client connect event lacks the peer key: %s", crec.text())
	}
	if !strings.Contains(crec.text(), "mac-a accepted the code") {
		t.Errorf("client code-ok event lacks A's host: %s", crec.text())
	}
	for _, e := range srec.events {
		if e.Time != testClock {
			t.Fatalf("event time %s, want the fixed clock", e.Time)
		}
	}
	assertNoCode(t, code, "events", srec.text(), crec.text())
}

func TestWrongCodeThreeTimes(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(64 << 10)
	var srec recorder
	var ui bytes.Buffer
	o := baseServe(t, code, body, &srec)
	o.UI = &ui
	addr, out := startServe(t, context.Background(), o)

	sinkCalled := false
	for i, wantLeft := range []int{2, 1, 0} {
		var crec recorder
		fo := baseFetch(addr, wrongCode(t, code), &bytes.Buffer{}, &crec)
		fo.Sink = func(context.Context, []byte, io.Reader) (Done, error) { sinkCalled = true; return Done{}, nil }
		_, err := Fetch(context.Background(), fo)
		if !errors.Is(err, ErrBadCode) {
			t.Fatalf("attempt %d: err = %v, want ErrBadCode", i+1, err)
		}
		want := fmt.Sprintf("(%d attempts left there)", wantLeft)
		if wantLeft == 0 {
			want = "gave up"
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("attempt %d: err = %q, want %q", i+1, err, want)
		}
		if !crec.has(kindBadCode) {
			t.Fatalf("attempt %d: client events %v", i+1, crec.kinds())
		}
		assertNoCode(t, code, "fetch error", err.Error(), crec.text())
	}
	if sinkCalled {
		t.Fatal("Sink ran on a rejected code")
	}

	r := waitServe(t, out, 10*time.Second)
	if !errors.Is(r.err, ErrTooManyAttempts) {
		t.Fatalf("Serve err = %v, want ErrTooManyAttempts", r.err)
	}
	if r.err.Error() != "3 failed pairing attempts; run bffs export --serve again for a new code" {
		t.Fatalf("Serve err text = %q", r.err)
	}
	if r.res.Attempts != 3 {
		t.Fatalf("Attempts = %d, want 3", r.res.Attempts)
	}
	if srec.count(kindBadCode) != 3 {
		t.Fatalf("bad-code events = %d: %s", srec.count(kindBadCode), srec.text())
	}
	if !strings.Contains(srec.text(), "tried a wrong code (2 attempts left)") {
		t.Fatalf("server events: %s", srec.text())
	}
	assertNoCode(t, code, "serve", r.err.Error(), srec.text(), ui.String())

	// A fourth connection is refused: the listeners are gone.
	var crec recorder
	_, err := Fetch(context.Background(), baseFetch(addr, code, &bytes.Buffer{}, &crec))
	if err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Fatalf("4th connection: err = %v, want a connect failure", err)
	}
	assertNoCode(t, code, "4th fetch error", err.Error())
}

func TestRightCodeOnThirdAttempt(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(256 << 10)
	var srec recorder
	addr, out := startServe(t, context.Background(), baseServe(t, code, body, &srec))

	for i := range 2 {
		var crec recorder
		_, err := Fetch(context.Background(), baseFetch(addr, wrongCode(t, code), &bytes.Buffer{}, &crec))
		if !errors.Is(err, ErrBadCode) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	var got bytes.Buffer
	var crec recorder
	d, err := Fetch(context.Background(), baseFetch(addr, code, &got, &crec))
	if err != nil || !d.OK {
		t.Fatalf("third attempt: %+v %v", d, err)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Fatal("body differs")
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil || !r.res.Done.OK || r.res.Attempts != 3 {
		t.Fatalf("Serve: %+v %v", r.res, r.err)
	}
}

func TestTTLExpiresWithoutClient(t *testing.T) {
	fastKDF(t)
	withTimeouts(t, func(ts *timeoutSet) { ts.Hint = 50 * time.Millisecond })
	code := mustCode(t)
	var srec recorder
	var ui bytes.Buffer
	o := baseServe(t, code, testBody(1024), &srec)
	o.TTL = 300 * time.Millisecond
	o.UI = &ui
	_, out := startServe(t, context.Background(), o)
	r := waitServe(t, out, 5*time.Second)
	if !errors.Is(r.err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", r.err)
	}
	if r.err.Error() != "code expired after 300ms; no pairing happened" {
		t.Fatalf("err text = %q", r.err)
	}
	if !srec.has(kindExpired) {
		t.Fatalf("events %v lack expired", srec.kinds())
	}
	if !strings.Contains(ui.String(), "no connection yet") {
		t.Fatalf("UI lacks the firewall hint: %q", ui.String())
	}
	if got := fmtTTL(10 * time.Minute); got != "10:00" {
		t.Fatalf("fmtTTL(10m) = %q", got)
	}
	assertNoCode(t, code, "expiry", r.err.Error(), srec.text(), ui.String())
}

func TestTTLNeverCutsAuthenticatedTransfer(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(512 << 10)
	var srec recorder
	o := baseServe(t, code, body, &srec)
	o.TTL = 300 * time.Millisecond
	addr, out := startServe(t, context.Background(), o)

	var got bytes.Buffer
	var crec recorder
	fo := baseFetch(addr, code, &got, &crec)
	fo.Confirm = func([]byte) (bool, error) {
		time.Sleep(600 * time.Millisecond) // the TTL passes while the human thinks
		return true, nil
	}
	d, err := Fetch(context.Background(), fo)
	if err != nil || !d.OK {
		t.Fatalf("Fetch after TTL: %+v %v", d, err)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Fatal("body differs")
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil || !r.res.Done.OK {
		t.Fatalf("Serve: %+v %v", r.res, r.err)
	}
	if srec.has(kindExpired) {
		t.Fatalf("an authenticated transfer must not report expiry: %s", srec.text())
	}
}

func TestDeclineKeepsServingUntilTTL(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	var srec recorder
	o := baseServe(t, code, testBody(4096), &srec)
	o.TTL = 700 * time.Millisecond
	addr, out := startServe(t, context.Background(), o)

	var crec recorder
	fo := baseFetch(addr, code, &bytes.Buffer{}, &crec)
	fo.Confirm = func([]byte) (bool, error) { return false, nil }
	sinkCalled := false
	fo.Sink = func(context.Context, []byte, io.Reader) (Done, error) { sinkCalled = true; return Done{}, nil }
	_, err := Fetch(context.Background(), fo)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("Fetch: %v, want ErrRejected", err)
	}
	if err.Error() != "import cancelled; nothing written" {
		t.Fatalf("Fetch err text = %q", err)
	}
	if sinkCalled {
		t.Fatal("Sink ran after a decline")
	}
	if !crec.has(kindReject) {
		t.Fatalf("client events %v", crec.kinds())
	}

	// A is still serving on the surviving listener.
	if _, err := net.DialTimeout("tcp", addr.String(), time.Second); err != nil {
		t.Fatalf("A stopped listening after a decline: %v", err)
	}
	r := waitServe(t, out, 10*time.Second)
	if !errors.Is(r.err, ErrExpired) {
		t.Fatalf("Serve: %v, want ErrExpired after a decline", r.err)
	}
	if !strings.Contains(srec.text(), "mac-b declined") || r.res.Attempts != 1 {
		t.Fatalf("Serve events %s attempts %d", srec.text(), r.res.Attempts)
	}
	if !strings.Contains(r.err.Error(), "nothing was delivered") {
		t.Fatalf("expiry after a pairing: %q", r.err)
	}
	assertNoCode(t, code, "decline", err.Error(), r.err.Error(), srec.text(), crec.text())
}

func TestDryRunKeepsServing(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(100 << 10)
	var srec recorder
	addr, out := startServe(t, context.Background(), baseServe(t, code, body, &srec))

	var crec recorder
	fo := baseFetch(addr, code, &bytes.Buffer{}, &crec)
	fo.DryRun = true
	confirmSeen := false
	fo.Confirm = func(m []byte) (bool, error) { confirmSeen = len(m) > 0; return false, nil }
	fo.Sink = nil
	d, err := Fetch(context.Background(), fo)
	if err != nil || !d.OK || d.Reason != "dry-run" {
		t.Fatalf("dry run: %+v %v", d, err)
	}
	if !confirmSeen {
		t.Fatal("Confirm must see the manifest in a dry run")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(srec.text(), "mac-b: dry run") {
		if time.Now().After(deadline) {
			t.Fatalf("Serve never reported the dry run: %s", srec.text())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The same serve then delivers for real: no attempt was burned.
	var got bytes.Buffer
	var crec2 recorder
	d, err = Fetch(context.Background(), baseFetch(addr, code, &got, &crec2))
	if err != nil || !d.OK || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("real fetch after dry run: %+v %v (%d bytes)", d, err, got.Len())
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil || !r.res.Done.OK {
		t.Fatalf("Serve: %+v %v", r.res, r.err)
	}
}

func TestEarlyDoneCancelsWriter(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(16 << 20)
	var srec recorder
	o := baseServe(t, code, body, &srec)
	bodyCtxErr := make(chan error, 1)
	// A producer that writes a little and then waits for more data: only a
	// cancelled ctx can bring it back.
	o.Body = func(ctx context.Context, w io.Writer) error {
		if _, err := w.Write(body[:2<<20]); err != nil {
			bodyCtxErr <- ctx.Err()
			return err
		}
		<-ctx.Done()
		bodyCtxErr <- ctx.Err()
		return ctx.Err()
	}
	addr, out := startServe(t, context.Background(), o)

	var crec recorder
	fo := baseFetch(addr, code, &bytes.Buffer{}, &crec)
	fo.Sink = func(ctx context.Context, manifest []byte, body io.Reader) (Done, error) {
		if _, err := io.CopyN(io.Discard, body, 1<<20); err != nil {
			return Done{}, err
		}
		return Done{}, errors.New("sha256 mismatch on projects/x/y.jsonl")
	}
	_, err := Fetch(context.Background(), fo)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(crec.text(), "import failed: sha256 mismatch") {
		t.Fatalf("client events: %s", crec.text())
	}
	r := waitServe(t, out, 15*time.Second)
	if r.err == nil || !strings.Contains(r.err.Error(), "mac-b reported: sha256 mismatch on projects/x/y.jsonl") {
		t.Fatalf("Serve err = %v", r.err)
	}
	if r.res.Done.OK || r.res.Done.Reason != "sha256 mismatch on projects/x/y.jsonl" {
		t.Fatalf("Serve done = %+v", r.res.Done)
	}
	if r.res.Bytes != 2<<20 {
		t.Fatalf("A wrote %d bytes, want the 2 MiB the producer managed", r.res.Bytes)
	}
	select {
	case cerr := <-bodyCtxErr:
		if cerr == nil {
			t.Fatal("Body's ctx was not cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Body never returned")
	}
	if !strings.Contains(srec.text(), "reported: sha256 mismatch") {
		t.Fatalf("server events: %s", srec.text())
	}
	assertNoCode(t, code, "early done", err.Error(), r.err.Error(), srec.text(), crec.text())
}

func TestEarlyDoneWhileWriting(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(48 << 20)
	var srec recorder
	o := baseServe(t, code, body, &srec)
	o.Body = writeBody(body, 1<<20)
	addr, out := startServe(t, context.Background(), o)

	fo := baseFetch(addr, code, &bytes.Buffer{}, &recorder{})
	fo.Sink = func(ctx context.Context, manifest []byte, body io.Reader) (Done, error) {
		if _, err := io.CopyN(io.Discard, body, 1<<20); err != nil {
			return Done{}, err
		}
		return Done{Reason: "sha256 mismatch on projects/x/y.jsonl"}, errors.New("import aborted")
	}
	start := time.Now()
	if _, err := Fetch(context.Background(), fo); err == nil {
		t.Fatal("Fetch succeeded")
	}
	r := waitServe(t, out, 15*time.Second)
	if r.err == nil || !strings.Contains(r.err.Error(), "mac-b reported: sha256 mismatch on projects/x/y.jsonl") {
		t.Fatalf("Serve err = %v", r.err)
	}
	if r.res.Bytes >= int64(len(body)) {
		t.Fatalf("A wrote the whole %d-byte body despite the early report", len(body))
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("the early report took %s to stop the stream", took)
	}
}

func TestIdleConnectionsDoNotBlockGoodPeer(t *testing.T) {
	fastKDF(t)
	withTimeouts(t, func(ts *timeoutSet) { ts.Handshake = 300 * time.Millisecond })
	code := mustCode(t)
	body := testBody(64 << 10)
	var srec recorder
	addr, out := startServe(t, context.Background(), baseServe(t, code, body, &srec))

	var idle []net.Conn
	for range maxInflight {
		c, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, c)
	}
	defer func() {
		for _, c := range idle {
			c.Close()
		}
	}()

	var got bytes.Buffer
	var crec recorder
	start := time.Now()
	d, err := Fetch(context.Background(), baseFetch(addr, code, &got, &crec))
	if err != nil || !d.OK {
		t.Fatalf("Fetch behind idle connections: %+v %v", d, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("good peer waited %s behind idle connections", took)
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("Serve: %v", r.err)
	}
	if r.res.Attempts != 1 {
		t.Fatalf("idle connections burned attempts: %d", r.res.Attempts)
	}
	if srec.count(kindError) < maxInflight {
		t.Fatalf("expected %d handshake failures logged, got %d: %s", maxInflight, srec.count(kindError), srec.text())
	}
}

func TestPeerCancelMidStreamKeepsServing(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(48 << 20)
	var srec recorder
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	o := baseServe(t, code, body, &srec)
	o.Body = writeBody(body, 1<<20)
	addr, out := startServe(t, sctx, o)

	fctx, fcancel := context.WithCancel(context.Background())
	defer fcancel()
	var crec recorder
	fo := baseFetch(addr, code, &bytes.Buffer{}, &crec)
	fo.Sink = func(ctx context.Context, manifest []byte, body io.Reader) (Done, error) {
		if _, err := io.CopyN(io.Discard, body, 1<<20); err != nil {
			return Done{}, err
		}
		fcancel() // Ctrl-C on B
		_, err := io.Copy(io.Discard, body)
		return Done{}, err
	}
	_, err := Fetch(fctx, fo)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch after cancel: %v, want context.Canceled", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(srec.text(), "transfer aborted by peer") {
		if time.Now().After(deadline) {
			t.Fatalf("Serve never reported the abort: %s", srec.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case r := <-out:
		t.Fatalf("Serve returned after a peer abort: %+v %v", r.res, r.err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := net.DialTimeout("tcp", addr.String(), time.Second); err != nil {
		t.Fatalf("A stopped listening after a peer abort: %v", err)
	}
	scancel()
	r := waitServe(t, out, 10*time.Second)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Serve after ctx cancel: %v", r.err)
	}
}

func TestServeCtxCancelMidStream(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(48 << 20)
	var srec recorder
	sctx, scancel := context.WithCancel(context.Background())
	defer scancel()
	o := baseServe(t, code, body, &srec)
	o.Body = writeBody(body, 1<<20)
	addr, out := startServe(t, sctx, o)

	var crec recorder
	fo := baseFetch(addr, code, &bytes.Buffer{}, &crec)
	fo.Sink = func(ctx context.Context, manifest []byte, body io.Reader) (Done, error) {
		if _, err := io.CopyN(io.Discard, body, 1<<20); err != nil {
			return Done{}, err
		}
		scancel() // Ctrl-C on A
		// A real sink knows the size from the manifest and fails on a
		// short stream.
		_, err := io.CopyN(io.Discard, body, manifestBytes(t, manifest)-1<<20)
		return Done{}, err
	}
	start := time.Now()
	_, err := Fetch(context.Background(), fo)
	if err == nil {
		t.Fatal("Fetch succeeded although A was cancelled mid-stream")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Fetch took %s to notice A's cancellation", took)
	}
	r := waitServe(t, out, 10*time.Second)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Serve: %v, want context.Canceled", r.err)
	}
	assertNoCode(t, code, "cancel", err.Error(), r.err.Error(), srec.text(), crec.text())
}

func TestServeRejectsNonLANPeer(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	var srec recorder
	o := baseServe(t, code, testBody(1024), &srec)
	o.TTL = 3 * time.Second
	addrCh := make(chan netip.AddrPort, 1)
	o.Listen = loopbackListen(addrCh)
	o.Local = loopbackLocal()
	o.Now = func() time.Time { return testClock }
	// AllowLoopback deliberately false: the loopback peer is then off-link.
	out := make(chan serveOut, 1)
	go func() {
		res, err := Serve(context.Background(), o)
		out <- serveOut{res, err}
	}()
	addr := <-addrCh
	var crec recorder
	_, err := Fetch(context.Background(), baseFetch(addr, code, &bytes.Buffer{}, &crec))
	if err == nil {
		t.Fatal("Fetch succeeded against a listener that must refuse the peer")
	}
	r := waitServe(t, out, 10*time.Second)
	if !errors.Is(r.err, ErrExpired) {
		t.Fatalf("Serve: %v", r.err)
	}
	if !strings.Contains(srec.text(), "refused 127.0.0.1: not on this listener's network") {
		t.Fatalf("server events: %s", srec.text())
	}
}

func TestServeValidation(t *testing.T) {
	ctx := context.Background()
	body := func(context.Context, io.Writer) error { return nil }
	code := mustCode(t)
	cases := []struct {
		name string
		o    ServeOptions
		want string
	}{
		{"no code", ServeOptions{Body: body}, "pairing code not set"},
		{"no body", ServeOptions{Code: code}, "body writer not set"},
		{"bad port", ServeOptions{Code: code, Body: body, Port: 70000}, "invalid port"},
		{"ttl above the ceiling", ServeOptions{Code: code, Body: body, TTL: 31 * time.Minute}, "TTL 31m0s exceeds the 30m0s maximum"},
		{"manifest too big", ServeOptions{Code: code, Body: body, Manifest: make([]byte, maxManifestFrame+1)}, "exceeds"},
		{"no addresses", ServeOptions{Code: code, Body: body, Local: []LinkAddr{}}, "no local network address"},
		{"bind fails", ServeOptions{Code: code, Body: body, Local: loopbackLocal(), Listen: func(string, string) (net.Listener, error) {
			return nil, errors.New("EADDRNOTAVAIL")
		}}, "could not listen on any local address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Serve(ctx, tc.o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			assertNoCode(t, code, "validation", err.Error())
		})
	}
}

// fakeListener is a net.Listener that never accepts; Addr reports what was
// bound, with a fixed port standing in for the kernel's choice of port 0.
type fakeListener struct {
	addr   netip.AddrPort
	closed chan struct{}
}

func (f *fakeListener) Accept() (net.Conn, error) {
	<-f.closed
	return nil, net.ErrClosed
}

func (f *fakeListener) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

func (f *fakeListener) Addr() net.Addr { return net.TCPAddrFromAddrPort(f.addr) }

func TestBindAllSharesOnePort(t *testing.T) {
	var asked []string
	listen := func(network, addr string) (net.Listener, error) {
		asked = append(asked, addr)
		ap, err := netip.ParseAddrPort(addr)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(addr, "10.") {
			return nil, errors.New("EADDRNOTAVAIL")
		}
		if ap.Port() == 0 {
			ap = netip.AddrPortFrom(ap.Addr(), 40000)
		}
		return &fakeListener{addr: ap, closed: make(chan struct{})}, nil
	}
	local := []LinkAddr{
		{Addr: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8"), Iface: "lo"},
		{Addr: netip.MustParseAddr("10.0.0.5"), Prefix: netip.MustParsePrefix("10.0.0.0/24"), Iface: "en0"},
		{Addr: netip.MustParseAddr("::1"), Prefix: netip.MustParsePrefix("::1/128"), Iface: "lo"},
	}
	bound, err := bindAll(local, 0, listen)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, b := range bound {
			b.ln.Close()
		}
	}()
	if len(bound) != 2 {
		t.Fatalf("bound %d listeners, want 2 (the 10.x refusal is dropped)", len(bound))
	}
	if bound[0].addr.Port() != 40000 || bound[1].addr.Port() != 40000 {
		t.Fatalf("ports differ: %s vs %s", bound[0].addr, bound[1].addr)
	}
	if len(asked) != 3 || asked[0] != "127.0.0.1:0" || asked[1] != "10.0.0.5:40000" || asked[2] != "[::1]:40000" {
		t.Fatalf("asked %v", asked)
	}
	// A fixed port is asked for as-is on every address.
	asked = nil
	fixed, err := bindAll(local[:1], 7345, listen)
	if err != nil || len(fixed) != 1 || asked[0] != "127.0.0.1:7345" {
		t.Fatalf("fixed port: %v %v", err, asked)
	}
	if bound[1].link.Iface != "lo" || bound[1].link.Addr != netip.MustParseAddr("::1") {
		t.Fatalf("second listener keeps its LinkAddr: %+v", bound[1].link)
	}
}
