package transfer

import (
	"bytes"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func TestDeriveKeyDependsOnEKM(t *testing.T) {
	fastKDF(t)
	c, err := ParseCode("7K3Q-M9XD")
	if err != nil {
		t.Fatal(err)
	}
	ekm1 := bytes.Repeat([]byte{1}, 32)
	ekm2 := bytes.Repeat([]byte{2}, 32)
	k1 := DeriveKey(c, ekm1)
	k1again := DeriveKey(c, ekm1)
	k2 := DeriveKey(c, ekm2)
	if len(k1) != 32 {
		t.Fatalf("key length %d, want 32", len(k1))
	}
	if !bytes.Equal(k1, k1again) {
		t.Fatal("DeriveKey is not deterministic")
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("same code, different ekm: keys must differ")
	}
	other, _ := ParseCode("7K3Q-M9XE")
	if bytes.Equal(k1, DeriveKey(other, ekm1)) {
		t.Fatal("different code, same ekm: keys must differ")
	}
}

func TestKDFParamsRestoredAfterFastKDF(t *testing.T) {
	// fastKDF only lowers params for its own test; the production values
	// must be intact here (each test restores through t.Cleanup).
	if kdfParams.Time != 3 || kdfParams.MemoryKiB != 64*1024 || kdfParams.Threads != 1 {
		t.Fatalf("kdfParams = %+v, want {3 65536 1}", kdfParams)
	}
}

func TestProofsDifferPerEKM(t *testing.T) {
	k := bytes.Repeat([]byte{7}, 32)
	ekm1 := bytes.Repeat([]byte{1}, 32)
	ekm2 := bytes.Repeat([]byte{2}, 32)
	cp1, cp2 := ClientProof(k, ekm1), ClientProof(k, ekm2)
	sp1 := ServerProof(k, ekm1)
	if bytes.Equal(cp1, cp2) {
		t.Fatal("client proofs must differ per ekm")
	}
	if bytes.Equal(cp1, sp1) {
		t.Fatal("client and server proofs must differ")
	}
	if !VerifyProof(cp1, ClientProof(k, ekm1)) {
		t.Fatal("VerifyProof rejected an equal proof")
	}
	if VerifyProof(cp1, cp2) {
		t.Fatal("VerifyProof accepted a different proof")
	}
	if VerifyProof(cp1, cp1[:31]) || VerifyProof(nil, cp1) || VerifyProof(nil, nil) == false {
		t.Fatal("VerifyProof length handling: mismatch must be false, two empty proofs equal")
	}
}

// handshakePair runs one loopback TLS 1.3 handshake with the package's
// configs and returns both connection states.
func handshakePair(t *testing.T, srv, cli *tls.Config) (tls.ConnectionState, tls.ConnectionState) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		cs  tls.ConnectionState
		err error
	}
	srvCh := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			srvCh <- res{err: err}
			return
		}
		defer c.Close()
		tc := tls.Server(c, srv)
		if err := tc.Handshake(); err != nil {
			srvCh <- res{err: err}
			return
		}
		srvCh <- res{cs: tc.ConnectionState()}
		// Keep the connection open until the client is done.
		time.Sleep(50 * time.Millisecond)
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tc := tls.Client(raw, cli)
	if err := tc.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	r := <-srvCh
	if r.err != nil {
		t.Fatalf("server handshake: %v", r.err)
	}
	return r.cs, tc.ConnectionState()
}

func TestSameCodeTwoConnectionsDifferentKeys(t *testing.T) {
	fastKDF(t)
	cert, _, err := EphemeralCert(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := ParseCode("7K3Q-M9XD")
	var keys [][]byte
	for range 2 {
		scs, ccs := handshakePair(t, ServerTLS(cert), ClientTLS(nil))
		if scs.Version != tls.VersionTLS13 || ccs.Version != tls.VersionTLS13 {
			t.Fatalf("versions %x/%x, want TLS 1.3", scs.Version, ccs.Version)
		}
		if scs.NegotiatedProtocol != alpn || ccs.NegotiatedProtocol != alpn {
			t.Fatalf("ALPN %q/%q, want %q", scs.NegotiatedProtocol, ccs.NegotiatedProtocol, alpn)
		}
		sekm, err := exportKeyingMaterial(scs)
		if err != nil {
			t.Fatal(err)
		}
		cekm, err := exportKeyingMaterial(ccs)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sekm, cekm) {
			t.Fatal("both ends must export the same keying material")
		}
		keys = append(keys, DeriveKey(code, sekm))
	}
	if bytes.Equal(keys[0], keys[1]) {
		t.Fatal("same code on two connections must derive different keys")
	}
}

func TestEphemeralCert(t *testing.T) {
	cert, fp, err := EphemeralCert(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Leaf == nil || cert.Leaf.Subject.CommonName != "bffs-transfer" {
		t.Fatalf("leaf %+v, want CN bffs-transfer", cert.Leaf)
	}
	now := time.Now()
	if cert.Leaf.NotBefore.After(now.Add(-30*time.Second)) || cert.Leaf.NotAfter.Before(now.Add(4*time.Minute)) {
		t.Fatalf("validity %s..%s, want now-1m..now+5m", cert.Leaf.NotBefore, cert.Leaf.NotAfter)
	}
	if fp == ([32]byte{}) {
		t.Fatal("fingerprint is zero")
	}
	var seen [32]byte
	_, seen = handshakePairFP(t, cert)
	if seen != fp {
		t.Fatal("client-side SPKI fingerprint differs from the server's")
	}
	_, fp2, _ := EphemeralCert(time.Minute)
	if fp2 == fp {
		t.Fatal("two ephemeral certs share a fingerprint")
	}
}

func handshakePairFP(t *testing.T, cert tls.Certificate) (tls.ConnectionState, [32]byte) {
	t.Helper()
	var seen [32]byte
	scs, _ := handshakePair(t, ServerTLS(cert), ClientTLS(func(f [32]byte) { seen = f }))
	return scs, seen
}

func TestClientTLSRequiresALPN(t *testing.T) {
	cert, _, err := EphemeralCert(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A server that does not speak our protocol: no NextProtos.
	srv := ServerTLS(cert)
	srv.NextProtos = nil
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
		_ = tls.Server(c, srv).Handshake()
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	called := false
	err = tls.Client(raw, ClientTLS(func([32]byte) { called = true })).Handshake()
	if err == nil {
		t.Fatal("handshake without ALPN succeeded")
	}
	if called {
		t.Fatal("onLeaf called for a rejected connection")
	}
	if got := ServerTLS(cert); got.MinVersion != tls.VersionTLS13 || !got.SessionTicketsDisabled || got.ClientAuth != tls.NoClientCert {
		t.Fatalf("ServerTLS = %+v", got)
	}
	if got := ClientTLS(nil); got.MinVersion != tls.VersionTLS13 || !got.InsecureSkipVerify || got.ClientSessionCache != nil || got.VerifyConnection == nil {
		t.Fatalf("ClientTLS = %+v", got)
	}
}
