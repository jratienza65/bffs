package transfer

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
)

const (
	exporterLabel    = "EXPORTER-bffs-transfer/v1"
	clientProofLabel = "bffs-transfer/v1 client-proof"
	serverProofLabel = "bffs-transfer/v1 server-proof"
	ekmLen           = 32
)

func proof(k []byte, label string, ekm []byte) []byte {
	m := hmac.New(sha256.New, k)
	m.Write([]byte(label))
	m.Write(ekm)
	return m.Sum(nil)
}

// ClientProof is what B sends in HELLO:
// HMAC-SHA256(k, "bffs-transfer/v1 client-proof" || ekm).
func ClientProof(k, ekm []byte) []byte { return proof(k, clientProofLabel, ekm) }

// ServerProof is what A sends in auth-ok:
// HMAC-SHA256(k, "bffs-transfer/v1 server-proof" || ekm).
func ServerProof(k, ekm []byte) []byte { return proof(k, serverProofLabel, ekm) }

// VerifyProof compares two proofs in constant time; a length mismatch is
// simply false.
func VerifyProof(want, got []byte) bool {
	return len(want) == len(got) && subtle.ConstantTimeCompare(want, got) == 1
}

// exportKeyingMaterial returns the 32-byte RFC 5705 exporter for this
// connection. It only works on TLS 1.3 connections, which both configs
// enforce.
func exportKeyingMaterial(cs tls.ConnectionState) ([]byte, error) {
	return cs.ExportKeyingMaterial(exporterLabel, nil, ekmLen)
}
