package transfer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// alpn is the application protocol both sides must negotiate.
const alpn = "bffs-transfer/1"

// EphemeralCert makes an in-memory self-signed ECDSA P-256 certificate with
// CN "bffs-transfer", valid from a minute ago until now+ttl, and returns it
// with sha256(SubjectPublicKeyInfo) — the fingerprint both banners show.
func EphemeralCert(ttl time.Duration) (tls.Certificate, [32]byte, error) {
	var fp [32]byte
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fp, fmt.Errorf("generating transfer key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fp, fmt.Errorf("generating certificate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "bffs-transfer"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fp, fmt.Errorf("creating transfer certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fp, fmt.Errorf("parsing transfer certificate: %w", err)
	}
	fp = sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, fp, nil
}

// ServerTLS is A's config: TLS 1.3 only, no session tickets, ALPN
// "bffs-transfer/1", no client certificates (the code is the client's
// credential).
func ServerTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{cert},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		NextProtos:             []string{alpn},
		ClientAuth:             tls.NoClientCert,
	}
}

// ClientTLS is B's config. Chain validation is skipped (A's certificate is
// ephemeral and self-signed; the pairing code authenticates A, not a CA) but
// VerifyConnection still requires TLS 1.3, exactly one peer certificate and
// the ALPN match, and hands sha256(SPKI) to onLeaf for the "peer key" line.
// No session cache: every connection is a fresh handshake with a fresh
// exporter.
func ClientTLS(onLeaf func(spki [32]byte)) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{alpn},
		ClientSessionCache: nil,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.Version != tls.VersionTLS13 {
				return errors.New("peer negotiated a TLS version below 1.3")
			}
			if n := len(cs.PeerCertificates); n != 1 {
				return fmt.Errorf("expected exactly one peer certificate, got %d", n)
			}
			if cs.NegotiatedProtocol != alpn {
				return fmt.Errorf("peer did not negotiate %q", alpn)
			}
			if onLeaf != nil {
				onLeaf(sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo))
			}
			return nil
		},
	}
}
