package transfer

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// FetchOptions configures one receive.
type FetchOptions struct {
	// Addr is the literal address to dial (already resolved).
	Addr netip.AddrPort
	// Code is asked for after the connection is up, so the human only
	// types it once the peer is proven reachable.
	Code func() (Code, error)
	// Confirm reviews the manifest and decides; nil accepts. It runs in
	// DryRun too (to print the plan) but its answer is then ignored.
	Confirm func(manifest []byte) (bool, error)
	// Sink consumes the body; it must honour ctx and return its report.
	Sink func(ctx context.Context, manifest []byte, body io.Reader) (Done, error)
	// UI receives one line per event when Events is nil. May be nil.
	UI io.Writer
	// Events receives every event. May be nil.
	Events func(Event)
	// LAN tunes what counts as on-link.
	LAN LANOptions
	// Dial is the injection seam; nil means a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// DryRun pairs and reviews the manifest, then sends reject "dry-run".
	DryRun bool
	// Version, Host and User identify B in accept.
	Version, Host, User string
	// Local is B's own address set for the pre-dial check; nil means
	// LANAddrs(LAN).
	Local []LinkAddr
}

type client struct {
	o      FetchOptions
	emitMu sync.Mutex
}

func (c *client) emit(kind, peer, text string) {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	ev := Event{Time: time.Now(), Kind: kind, Peer: peer, Text: text}
	if c.o.Events != nil {
		c.o.Events(ev)
	} else if c.o.UI != nil {
		fmt.Fprintln(c.o.UI, text)
	}
}

// Fetch connects to A, pairs, reviews the manifest and receives the body.
// Nothing is dialled unless Addr is on a local network of this machine;
// nothing is accepted unless A proves it knows the code.
func Fetch(ctx context.Context, o FetchOptions) (Done, error) {
	if !o.Addr.IsValid() {
		return Done{}, errors.New("transfer: address not set")
	}
	if o.Code == nil {
		return Done{}, errors.New("transfer: code prompt not set")
	}
	if o.Sink == nil && !o.DryRun {
		return Done{}, errors.New("transfer: sink not set")
	}
	c := &client{o: o}
	ip := o.Addr.Addr().String()
	peerS := o.Addr.String()

	local := o.Local
	if local == nil {
		var err error
		local, err = LANAddrs(o.LAN)
		if err != nil {
			return Done{}, err
		}
	}
	if !IsLAN(o.Addr.Addr(), local, o.LAN) {
		return Done{}, withDetail(ErrNotLAN, fmt.Sprintf("refusing to pair with %s: %v. Use bffs export --out file.bffs, or bffs export --out - | ssh host bffs import --from -", ip, ErrNotLAN))
	}

	dial := o.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	dctx, dcancel := context.WithTimeout(ctx, timeouts.Dial)
	raw, err := dial(dctx, "tcp", o.Addr.String())
	dcancel()
	if err != nil {
		if ctx.Err() != nil {
			return Done{}, ctx.Err()
		}
		return Done{}, fmt.Errorf("could not reach %s within %s (%v) — is bffs export --serve still running there (codes expire after 10 minutes), and is port %d allowed by its firewall?", peerS, timeouts.Dial, err, o.Addr.Port())
	}
	defer raw.Close()

	// Ctrl-C cuts the connection whatever it is doing.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			raw.Close()
		case <-stop:
		}
	}()
	defer close(stop)

	var spki [32]byte
	tconn := tls.Client(raw, ClientTLS(func(f [32]byte) { spki = f }))
	_ = raw.SetDeadline(time.Now().Add(timeouts.Dial))
	hctx, hcancel := context.WithTimeout(ctx, timeouts.Dial)
	err = tconn.HandshakeContext(hctx)
	hcancel()
	if err != nil {
		if ctx.Err() != nil {
			return Done{}, ctx.Err()
		}
		return Done{}, fmt.Errorf("TLS handshake with %s failed: %s", peerS, errText(err))
	}
	_ = raw.SetDeadline(time.Time{})
	cs := tconn.ConnectionState()
	fp := hex.EncodeToString(spki[:])[:8]
	c.emit(kindConnect, peerS, fmt.Sprintf("connected to %s (TLS 1.3, peer key %s)", peerS, fp))
	ekm, err := exportKeyingMaterial(cs)
	if err != nil {
		return Done{}, fmt.Errorf("TLS exporter unavailable: %w", err)
	}

	code, err := o.Code()
	if err != nil {
		return Done{}, err
	}
	if code.isZero() {
		return Done{}, ErrInvalidCode
	}
	k := DeriveKey(code, ekm)
	proofB := ClientProof(k, ekm)

	_ = raw.SetWriteDeadline(time.Now().Add(timeouts.Dial))
	if err := writeMsg(tconn, helloMsg{T: tHello, V: protocolVersion, Formats: []int{bundleFormat}, Proof: base64.StdEncoding.EncodeToString(proofB)}); err != nil {
		return Done{}, c.connErr(ctx, fmt.Errorf("sending hello to %s: %w", peerS, err))
	}
	_ = raw.SetReadDeadline(time.Now().Add(timeouts.Reply))
	m, err := readMsg(tconn)
	if err != nil {
		return Done{}, c.connErr(ctx, fmt.Errorf("waiting for %s to check the code: %w", peerS, err))
	}
	switch m.T {
	case tBadCode:
		var text string
		if m.AttemptsLeft > 0 {
			text = fmt.Sprintf("%v (%d attempts left there). Run bffs import again.", ErrBadCode, m.AttemptsLeft)
		} else {
			text = fmt.Sprintf("%v; it gave up after too many wrong codes — ask for a new one.", ErrBadCode)
		}
		c.emit(kindBadCode, peerS, text)
		return Done{}, withDetail(ErrBadCode, text)
	case tAuthOK:
	default:
		return Done{}, fmt.Errorf("unexpected %q from %s while waiting for the code check", cleanText(m.T, 32), peerS)
	}
	if m.V != protocolVersion {
		return Done{}, fmt.Errorf("%s speaks transfer protocol v%d; this bffs speaks v%d", peerS, m.V, protocolVersion)
	}
	proofA, err := base64.StdEncoding.DecodeString(m.Proof)
	if err != nil || !VerifyProof(ServerProof(k, ekm), proofA) {
		text := fmt.Sprintf("the peer at %s is not the machine that showed the code — refusing to receive anything. Someone on this network may be interfering; regenerate the code.", ip)
		c.emit(kindError, peerS, text)
		return Done{}, withDetail(ErrPeerNotOwner, text)
	}
	peerHost := cleanText(m.Host, 64)
	name := peerHost
	if name == "" {
		name = ip
	}
	c.emit(kindCodeOK, peerS, fmt.Sprintf("%s accepted the code (bffs %s, user %s, account %s, compression %d)", name, cleanText(m.Bffs, 32), cleanText(m.User, 64), cleanText(m.Account, 64), m.Compression))

	manifest, err := readFrame(&deadlineReader{r: tconn, conn: raw}, maxManifestFrame)
	if err != nil {
		return Done{}, c.connErr(ctx, fmt.Errorf("receiving the manifest from %s: %w", name, err))
	}
	sum := sha256.Sum256(manifest)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), m.ManifestSHA256) {
		return Done{}, fmt.Errorf("manifest from %s does not match its announced checksum; refusing to continue", name)
	}
	c.emit(kindManifestSent, peerS, fmt.Sprintf("manifest received from %s (%d bytes)", name, len(manifest)))
	_ = raw.SetReadDeadline(time.Time{})

	// The human decides; pings keep A's consent deadline alive meanwhile.
	var writeMu sync.Mutex
	pingStop := make(chan struct{})
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		t := time.NewTicker(timeouts.Ping)
		defer t.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-t.C:
				writeMu.Lock()
				_ = raw.SetWriteDeadline(time.Now().Add(timeouts.Dial))
				err := writeMsg(tconn, pingMsg{T: tPing})
				writeMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
	ok := true
	var cerr error
	if o.Confirm != nil {
		ok, cerr = o.Confirm(manifest)
	}
	close(pingStop)
	<-pingDone
	if ctx.Err() != nil {
		return Done{}, ctx.Err()
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	_ = raw.SetWriteDeadline(time.Now().Add(timeouts.Dial))
	reject := func(reason string) {
		_ = writeMsg(tconn, rejectMsg{T: tReject, Reason: reason, Host: o.Host, User: o.User})
		tconn.Close()
	}
	if cerr != nil {
		reject(reasonDeclined)
		return Done{}, cerr
	}
	if o.DryRun {
		reject(reasonDryRun)
		c.emit(kindReject, peerS, fmt.Sprintf("dry run: nothing requested from %s", name))
		return Done{OK: true, Reason: reasonDryRun}, nil
	}
	if !ok {
		reject(reasonDeclined)
		c.emit(kindReject, peerS, fmt.Sprintf("declined; nothing requested from %s", name))
		return Done{}, ErrRejected
	}
	if err := writeMsg(tconn, acceptMsg{T: tAccept, V: protocolVersion, Bffs: o.Version, Host: o.Host, User: o.User}); err != nil {
		return Done{}, c.connErr(ctx, fmt.Errorf("sending accept to %s: %w", name, err))
	}
	c.emit(kindAccept, peerS, fmt.Sprintf("accepted; receiving from %s", name))

	dr := &deadlineReader{r: tconn, conn: raw, onProgress: func(n int64) {
		c.emit(kindProgress, peerS, fmt.Sprintf("received %d MiB from %s", n/mib, name))
	}}
	d, serr := o.Sink(ctx, manifest, dr)
	_ = raw.SetWriteDeadline(time.Now().Add(timeouts.Dial))
	if serr != nil {
		if ctx.Err() != nil {
			return Done{}, ctx.Err()
		}
		d.OK = false
		if d.Reason == "" {
			d.Reason = serr.Error()
		}
		d.Reason = cleanText(d.Reason, 1024)
		// Report, then let A close first: closing with unread body bytes
		// in our receive buffer would send a reset, and some stacks drop
		// the report along with it. Nothing is drained beyond that wait.
		_ = writeMsg(tconn, doneMsg{T: tDone, Done: d})
		lingerUntilClosed(raw, timeouts.Grace)
		raw.Close()
		c.emit(kindError, peerS, fmt.Sprintf("import failed: %s", d.Reason))
		return d, serr
	}
	d.OK = true
	d.Reason = cleanText(d.Reason, 1024)
	if err := writeMsg(tconn, doneMsg{T: tDone, Done: d}); err != nil {
		return d, c.connErr(ctx, fmt.Errorf("reporting completion to %s: %w", name, err))
	}
	tconn.Close()
	c.emit(kindDone, peerS, fmt.Sprintf("received %d entries (%d bytes) from %s", d.Entries, d.Bytes, name))
	return d, nil
}

// lingerUntilClosed discards raw bytes until the peer closes or the wait
// runs out.
func lingerUntilClosed(conn net.Conn, wait time.Duration) {
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	var scratch [32 * 1024]byte
	for {
		if _, err := conn.Read(scratch[:]); err != nil {
			return
		}
	}
}

// connErr prefers the context's error when the connection failed because
// the caller cancelled.
func (c *client) connErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// deadlineReader refreshes the read deadline per MiB received.
type deadlineReader struct {
	r          io.Reader
	conn       net.Conn
	n          int64
	since      int64
	nextReport int64
	onProgress func(n int64)
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	if d.n == 0 {
		_ = d.conn.SetReadDeadline(time.Now().Add(timeouts.ReadSlice))
		d.nextReport = progressStep
	}
	n, err := d.r.Read(p)
	d.n += int64(n)
	d.since += int64(n)
	if d.since >= mib {
		d.since = 0
		_ = d.conn.SetReadDeadline(time.Now().Add(timeouts.ReadSlice))
	}
	if d.n >= d.nextReport && d.onProgress != nil {
		d.nextReport += progressStep
		d.onProgress(d.n)
	}
	return n, err
}
