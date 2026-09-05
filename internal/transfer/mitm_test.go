package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// relay terminates B's TLS with its own certificate, opens its own TLS
// connection to A, and copies bytes both ways. It is exactly what an
// ARP-spoofing host on the LAN could do.
func relay(t *testing.T, target netip.AddrPort) netip.AddrPort {
	t.Helper()
	cert, _, err := EphemeralCert(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				front := tls.Server(c, ServerTLS(cert))
				if err := front.Handshake(); err != nil {
					return
				}
				raw, err := net.Dial("tcp", target.String())
				if err != nil {
					return
				}
				defer raw.Close()
				back := tls.Client(raw, ClientTLS(nil))
				if err := back.Handshake(); err != nil {
					return
				}
				done := make(chan struct{}, 2)
				go func() { io.Copy(back, front); done <- struct{}{} }()
				go func() { io.Copy(front, back); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

func TestMITMRelayCannotForwardProof(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	var srec recorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startServe(t, ctx, baseServe(t, code, testBody(4096), &srec))
	mitm := relay(t, addr)

	var crec recorder
	sinkCalled := false
	fo := baseFetch(mitm, code, &bytes.Buffer{}, &crec)
	fo.Sink = func(context.Context, []byte, io.Reader) (Done, error) { sinkCalled = true; return Done{}, nil }
	_, err := Fetch(context.Background(), fo)
	if !errors.Is(err, ErrBadCode) {
		t.Fatalf("Fetch through a relay: %v, want ErrBadCode (A must reject the relayed proof)", err)
	}
	if sinkCalled {
		t.Fatal("Sink ran through a relay")
	}
	deadline := time.Now().Add(5 * time.Second)
	for srec.count(kindBadCode) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("A never logged the bad code: %s", srec.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The right code straight to A still works: the relay burned one
	// attempt, nothing more.
	var got bytes.Buffer
	d, err := Fetch(context.Background(), baseFetch(addr, code, &got, &recorder{}))
	if err != nil || !d.OK {
		t.Fatalf("direct fetch after the relay attempt: %+v %v", d, err)
	}
	r := waitServe(t, out, 10*time.Second)
	if r.err != nil || r.res.Attempts != 2 {
		t.Fatalf("Serve: %v attempts %d", r.err, r.res.Attempts)
	}
	assertNoCode(t, code, "mitm", srec.text(), crec.text())
}

// forgedServer accepts B's TLS and answers HELLO with an auth-ok carrying a
// random proof and a well-formed manifest frame — a fake A that never knew
// the code.
func forgedServer(t *testing.T) netip.AddrPort {
	t.Helper()
	cert, _, err := EphemeralCert(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tc := tls.Server(c, ServerTLS(cert))
				if err := tc.Handshake(); err != nil {
					return
				}
				if _, err := readMsg(tc); err != nil {
					return
				}
				manifest := []byte(`{"format":1,"bytes":4,"entries":1}`)
				sum := sha256.Sum256(manifest)
				fake := make([]byte, 32)
				rand.Read(fake)
				_ = writeMsg(tc, authOKMsg{
					T: tAuthOK, V: protocolVersion, Bffs: "evil", Host: "evil", User: "evil", Account: "evil",
					Proof: base64.StdEncoding.EncodeToString(fake), Compression: 1, ManifestSHA256: hex.EncodeToString(sum[:]),
				})
				_ = writeFrame(tc, manifest, maxManifestFrame)
				_, _ = readMsg(tc) // would be accept — must never arrive
			}()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

func TestForgedAuthOKRejected(t *testing.T) {
	fastKDF(t)
	code := mustCode(t)
	fake := forgedServer(t)
	var crec recorder
	sinkCalled, confirmCalled := false, false
	fo := baseFetch(fake, code, &bytes.Buffer{}, &crec)
	fo.Confirm = func([]byte) (bool, error) { confirmCalled = true; return true, nil }
	fo.Sink = func(context.Context, []byte, io.Reader) (Done, error) { sinkCalled = true; return Done{}, nil }
	_, err := Fetch(context.Background(), fo)
	if !errors.Is(err, ErrPeerNotOwner) {
		t.Fatalf("err = %v, want ErrPeerNotOwner", err)
	}
	want := "the peer at 127.0.0.1 is not the machine that showed the code — refusing to receive anything. Someone on this network may be interfering; regenerate the code."
	if err.Error() != want {
		t.Fatalf("err text = %q", err)
	}
	if sinkCalled || confirmCalled {
		t.Fatalf("forged auth-ok reached Confirm=%v Sink=%v", confirmCalled, sinkCalled)
	}
	if !strings.Contains(crec.text(), "not the machine that showed the code") {
		t.Fatalf("client events: %s", crec.text())
	}
	assertNoCode(t, code, "forged", err.Error(), crec.text())
}
