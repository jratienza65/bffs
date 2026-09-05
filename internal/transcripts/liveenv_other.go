//go:build !darwin && !linux

package transcripts

// readProcEnviron has no implementation here (Windows and the BSDs): the
// account of a live claude stays unknown.
func readProcEnviron(int) ([]byte, error) { return nil, errProcEnvironUnsupported }
