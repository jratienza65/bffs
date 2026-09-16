package transfer

import "testing"

// fastKDF lowers argon2id to t=1, m=8 MiB for one test. The package never
// uses t.Parallel, so mutating the package variable is safe.
func fastKDF(t *testing.T) {
	t.Helper()
	old := kdfParams
	kdfParams.Time, kdfParams.MemoryKiB, kdfParams.Threads = 1, 8*1024, 1
	t.Cleanup(func() { kdfParams = old })
}

// withTimeouts lets one test shorten protocol deadlines.
func withTimeouts(t *testing.T, mut func(*timeoutSet)) {
	t.Helper()
	old := timeouts
	mut(&timeouts)
	t.Cleanup(func() { timeouts = old })
}
