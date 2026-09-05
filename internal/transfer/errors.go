package transfer

import "errors"

// Sentinel errors. Serve and Fetch return errors that wrap these (errors.Is)
// while carrying the concrete message for the situation (the peer's address,
// the attempts left, the TTL). None of them ever contains the pairing code.
var (
	// ErrTooManyAttempts ends a serve after MaxAttempts wrong codes.
	ErrTooManyAttempts = errors.New("too many failed pairing attempts; run bffs export --serve again for a new code")
	// ErrExpired ends a serve whose TTL passed without a completed transfer.
	ErrExpired = errors.New("code expired; no pairing happened")
	// ErrRejected is returned by Fetch when Confirm declined the manifest.
	ErrRejected = errors.New("import cancelled; nothing written")
	// ErrBadCode is returned by Fetch when the other machine rejected the code.
	ErrBadCode = errors.New("the other machine rejected the code")
	// ErrNotLAN is returned by Fetch when the target is not on a local
	// network of this machine (nothing is dialled).
	ErrNotLAN = errors.New("not on a local network of this machine (--allow-routed for multi-VLAN offices)")
	// ErrPeerNotOwner is returned by Fetch when the peer could not prove it
	// is the machine that showed the code (nothing is accepted from it).
	ErrPeerNotOwner = errors.New("the peer is not the machine that showed the code — refusing to receive anything. Someone on this network may be interfering; regenerate the code.")
)

// detailErr carries a situation-specific message while matching a sentinel
// through errors.Is. It is used instead of fmt.Errorf("%w") so the message
// can be the exact text the user should see rather than "prefix: sentinel".
type detailErr struct {
	msg  string
	base error
}

func (e *detailErr) Error() string   { return e.msg }
func (e *detailErr) Unwrap() error   { return e.base }
func (e *detailErr) Is(t error) bool { return t == e.base }

func withDetail(base error, msg string) error {
	return &detailErr{msg: msg, base: base}
}
