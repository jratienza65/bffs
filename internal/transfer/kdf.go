package transfer

import (
	"context"
	"sync"

	"golang.org/x/crypto/argon2"
)

// kdfParams are the argon2id parameters for DeriveKey. They are a package
// variable so tests can lower them (export_test.go); nothing else touches
// them, and the package never uses t.Parallel.
var kdfParams = struct {
	Time      uint32
	MemoryKiB uint32
	Threads   uint8
}{3, 64 * 1024, 1}

// kdfMu serialises argon2 evaluations so a flood of connections cannot
// multiply the 64 MiB working set.
var kdfMu sync.Mutex

const keyLen = 32

// DeriveKey derives the per-connection key
// K = argon2id(password = canonical code bytes, salt = ekm, kdfParams, 32).
// The salt is the TLS exporter of this exact connection, so the same code
// yields a different K on every connection and nothing can be precomputed.
func DeriveKey(c Code, ekm []byte) []byte {
	k, _ := deriveKey(context.Background(), c, ekm)
	return k
}

// deriveKey is DeriveKey for a caller that may stop caring while it queues
// behind kdfMu: a serve that has already ended (attempts spent, TTL passed,
// Ctrl-C) must not keep paying for every guess a flood queued up. The check
// happens under the lock so nothing is computed once ctx is done.
func deriveKey(ctx context.Context, c Code, ekm []byte) ([]byte, error) {
	kdfMu.Lock()
	defer kdfMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := kdfParams
	return argon2.IDKey([]byte(c.s), ekm, p.Time, p.MemoryKiB, p.Threads, keyLen), nil
}
