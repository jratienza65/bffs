package fsutil

// pathError is the error FreeSpace returns for a directory it could not
// query; it unwraps to the OS error.
type pathError struct {
	op   string
	path string
	err  error
}

func (e *pathError) Error() string { return e.op + " " + e.path + ": " + e.err.Error() }
func (e *pathError) Unwrap() error { return e.err }
