package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestFetchRefusesNonLANWithoutDialling(t *testing.T) {
	dialled := false
	var rec recorder
	fo := FetchOptions{
		Addr:   netip.MustParseAddrPort("203.0.113.5:7345"),
		Code:   func() (Code, error) { t.Fatal("code prompted"); return Code{}, nil },
		Sink:   func(context.Context, []byte, io.Reader) (Done, error) { return Done{}, nil },
		Events: rec.add,
		Local:  testLocal(),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			dialled = true
			return nil, errors.New("must not dial")
		},
	}
	_, err := Fetch(context.Background(), fo)
	if !errors.Is(err, ErrNotLAN) {
		t.Fatalf("err = %v, want ErrNotLAN", err)
	}
	want := "refusing to pair with 203.0.113.5: not on a local network of this machine (--allow-routed for multi-VLAN offices). Use bffs export --out file.bffs, or bffs export --out - | ssh host bffs import --from -"
	if err.Error() != want {
		t.Fatalf("err text = %q\nwant %q", err, want)
	}
	if dialled {
		t.Fatal("dialled a non-LAN address")
	}
	// Private but off-link is refused too; AllowRouted admits it (and then
	// the injected dialer answers).
	fo.Addr = netip.MustParseAddrPort("10.8.0.5:7345")
	if _, err := Fetch(context.Background(), fo); !errors.Is(err, ErrNotLAN) {
		t.Fatalf("off-link private: %v", err)
	}
	fo.LAN.AllowRouted = true
	_, err = Fetch(context.Background(), fo)
	if !dialled || errors.Is(err, ErrNotLAN) {
		t.Fatalf("AllowRouted: dialled=%v err=%v", dialled, err)
	}
	// The denylist holds even when routed.
	fo.Addr = netip.MustParseAddrPort("100.64.1.1:7345")
	if _, err := Fetch(context.Background(), fo); !errors.Is(err, ErrNotLAN) {
		t.Fatalf("denylist with AllowRouted: %v", err)
	}
}

func TestFetchConnectRefused(t *testing.T) {
	withTimeouts(t, func(ts *timeoutSet) { ts.Dial = 2 * time.Second })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddrPort(ln.Addr().String())
	ln.Close() // nobody listens there now
	var rec recorder
	fo := FetchOptions{
		Addr:   addr,
		Code:   func() (Code, error) { t.Fatal("code prompted before a connection"); return Code{}, nil },
		Sink:   func(context.Context, []byte, io.Reader) (Done, error) { return Done{}, nil },
		Events: rec.add,
		LAN:    LANOptions{AllowLoopback: true},
		Local:  loopbackLocal(),
	}
	_, err = Fetch(context.Background(), fo)
	if err == nil || !strings.Contains(err.Error(), "could not reach "+addr.String()+" within 2s") || !strings.Contains(err.Error(), "firewall") {
		t.Fatalf("err = %v", err)
	}
	if len(rec.events) != 0 {
		t.Fatalf("events before a connection: %v", rec.kinds())
	}
}

func TestFetchValidation(t *testing.T) {
	sink := func(context.Context, []byte, io.Reader) (Done, error) { return Done{}, nil }
	code := func() (Code, error) { return Code{}, nil }
	addr := netip.MustParseAddrPort("127.0.0.1:1")
	for _, tc := range []struct {
		name string
		o    FetchOptions
		want string
	}{
		{"no addr", FetchOptions{Code: code, Sink: sink}, "address not set"},
		{"no code", FetchOptions{Addr: addr, Sink: sink}, "code prompt not set"},
		{"no sink", FetchOptions{Addr: addr, Code: code}, "sink not set"},
	} {
		_, err := Fetch(context.Background(), tc.o)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestFetchCodePromptedAfterConnectAndErrorsPropagate(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	var srec recorder
	o := baseServe(t, code, testBody(1024), &srec)
	o.TTL = time.Second
	addr, out := startServe(t, context.Background(), o)

	var crec recorder
	fo := baseFetch(addr, code, &bytes.Buffer{}, &crec)
	promptErr := errors.New("stdin closed")
	fo.Code = func() (Code, error) {
		if !crec.has(kindConnect) {
			t.Error("code prompted before the connect event")
		}
		return Code{}, promptErr
	}
	_, err := Fetch(context.Background(), fo)
	if !errors.Is(err, promptErr) {
		t.Fatalf("err = %v, want the prompt's error", err)
	}
	// A zero code from the prompt is refused locally, never sent.
	fo.Code = func() (Code, error) { return Code{}, nil }
	if _, err := Fetch(context.Background(), fo); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("zero code: %v", err)
	}
	r := waitServe(t, out, 10*time.Second)
	if !errors.Is(r.err, ErrExpired) || r.res.Attempts != 0 {
		t.Fatalf("Serve: %v attempts %d (a closed prompt must not burn attempts)", r.err, r.res.Attempts)
	}
}

func TestFetchConfirmErrorDeclines(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	var srec recorder
	o := baseServe(t, code, testBody(1024), &srec)
	o.TTL = time.Second
	addr, out := startServe(t, context.Background(), o)

	var crec recorder
	fo := baseFetch(addr, code, &bytes.Buffer{}, &crec)
	boom := errors.New("prompt failed")
	fo.Confirm = func([]byte) (bool, error) { return true, boom }
	_, err := Fetch(context.Background(), fo)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	r := waitServe(t, out, 10*time.Second)
	if !errors.Is(r.err, ErrExpired) || !strings.Contains(srec.text(), "declined") {
		t.Fatalf("Serve: %v %s", r.err, srec.text())
	}
}

func TestFetchNilConfirmAccepts(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(8192)
	var srec recorder
	addr, out := startServe(t, context.Background(), baseServe(t, code, body, &srec))
	var got bytes.Buffer
	var crec recorder
	fo := baseFetch(addr, code, &got, &crec)
	fo.Confirm = nil
	d, err := Fetch(context.Background(), fo)
	if err != nil || !d.OK || !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("%+v %v", d, err)
	}
	if r := waitServe(t, out, 10*time.Second); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestFetchUIFallbackNeverShowsCode(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(8192)
	var sui, cui bytes.Buffer
	o := baseServe(t, code, body, &recorder{})
	o.Events = nil
	o.UI = &sui
	addr, out := startServe(t, context.Background(), o)
	var got bytes.Buffer
	fo := baseFetch(addr, code, &got, &recorder{})
	fo.Events = nil
	fo.UI = &cui
	if _, err := Fetch(context.Background(), fo); err != nil {
		t.Fatal(err)
	}
	if r := waitServe(t, out, 10*time.Second); r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.Contains(sui.String(), "listening on") || !strings.Contains(sui.String(), "mac-b accepted") {
		t.Fatalf("server UI: %q", sui.String())
	}
	if !strings.Contains(cui.String(), "connected to") || !strings.Contains(cui.String(), "received 3 entries") {
		t.Fatalf("client UI: %q", cui.String())
	}
	assertNoCode(t, code, "UI", sui.String(), cui.String())
}

func TestFetchManifestChecksumIsOnBytes(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	body := testBody(8192)
	var srec recorder
	o := baseServe(t, code, body, &srec)
	// An indented manifest: the same JSON, different bytes; the sha travels
	// over the bytes actually sent, so B must see exactly those.
	o.Manifest = []byte("{\n  \"format\": 1,\n  \"bytes\": 8192,\n  \"entries\": 3\n}\n")
	addr, out := startServe(t, context.Background(), o)
	var got bytes.Buffer
	var seen []byte
	fo := baseFetch(addr, code, &got, &recorder{})
	fo.Confirm = func(m []byte) (bool, error) { seen = m; return true, nil }
	if _, err := Fetch(context.Background(), fo); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seen, o.Manifest) {
		t.Fatalf("manifest bytes differ: %q", seen)
	}
	if r := waitServe(t, out, 10*time.Second); r.err != nil || !bytes.Equal(got.Bytes(), body) {
		t.Fatal(r.err)
	}
}
