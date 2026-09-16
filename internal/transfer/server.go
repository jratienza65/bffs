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
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Event is one line of what happened, for the caller's printer. Kind is one
// of: listen, connect, reject, bad-code, code-ok, manifest-sent, accept,
// sending, progress, done, error, expired. Peer is the other side's ip:port;
// for listen events it is the address that was bound (the banner's
// "--from" target), and it is empty for expired. Text never contains the
// pairing code, the derived key or a proof. Events may be delivered from
// several goroutines but never concurrently.
type Event struct {
	Time time.Time
	Kind string
	Peer string
	Text string
}

// Event kinds.
const (
	kindListen       = "listen"
	kindConnect      = "connect"
	kindReject       = "reject"
	kindBadCode      = "bad-code"
	kindCodeOK       = "code-ok"
	kindManifestSent = "manifest-sent"
	kindAccept       = "accept"
	kindSending      = "sending"
	kindProgress     = "progress"
	kindDone         = "done"
	kindError        = "error"
	kindExpired      = "expired"
)

// Listener is the injection seam for binding; the default is net.Listen.
type Listener func(network, addr string) (net.Listener, error)

// ServeOptions configures one serve: one code, one bundle, one delivery.
type ServeOptions struct {
	// Code is the pairing code the other side must prove (required).
	Code Code
	// Port to bind on every local address; 0 lets the kernel choose once
	// and every listener then shares that port.
	Port int
	// TTL bounds pairing: the accept loop, proof checks and the certificate
	// (default 10 min, at most 30). An authenticated transfer is never cut
	// by it.
	TTL time.Duration
	// MaxAttempts wrong codes end the serve (default 3).
	MaxAttempts int
	// Manifest is sent verbatim right after auth-ok (≤ 16 MiB).
	Manifest []byte
	// Compression is the bundle's envelope byte, echoed in auth-ok.
	Compression byte
	// Body streams the bundle bytes; it must honour ctx.
	Body func(ctx context.Context, w io.Writer) error
	// UI receives one line per event when Events is nil, and hints (the
	// firewall note) always. May be nil.
	UI io.Writer
	// Events receives every event. May be nil.
	Events func(Event)
	// LAN tunes what counts as on-link.
	LAN LANOptions
	// Listen binds; nil means net.Listen.
	Listen Listener
	// Now is the clock for event times and Duration; nil means time.Now.
	// Deadlines always use the real clock.
	Now func() time.Time
	// Version, Host, User and Account identify A in auth-ok.
	Version, Host, User, Account string
	// Local is the address set to bind; nil means LANAddrs(LAN).
	Local []LinkAddr
}

// Done is B's completion report.
type Done struct {
	OK      bool   `json:"ok"`
	Entries int    `json:"entries,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ServeResult describes what a serve did.
type ServeResult struct {
	// Peer, PeerHost and PeerUser identify the authenticated receiver.
	Peer               netip.AddrPort
	PeerHost, PeerUser string
	// Done is the receiver's report.
	Done Done
	// Attempts counts every proof verified, right or wrong.
	Attempts int
	// Bytes is what A wrote to the connection.
	Bytes int64
	// Duration is the whole serve, on the Now clock.
	Duration time.Duration
	// Listeners are the addresses that were bound, for the banner.
	Listeners []netip.AddrPort
	// SPKI is sha256 of the ephemeral certificate's public key.
	SPKI [32]byte
}

const (
	defaultTTL         = 10 * time.Minute
	maxTTL             = 30 * time.Minute
	defaultMaxAttempts = 3
	maxInflight        = 8
	mib                = 1 << 20
	progressStep       = 16 * mib
	acceptBackoffMin   = 5 * time.Millisecond
	acceptBackoffMax   = time.Second
)

// timeoutSet holds the protocol's real-clock deadlines.
type timeoutSet struct {
	Handshake  time.Duration // TLS handshake per connection
	Hello      time.Duration // HELLO after the handshake (a human is typing)
	Consent    time.Duration // accept/reject after auth-ok, refreshed by pings
	Done       time.Duration // DONE after the last body byte
	WriteSlice time.Duration // write deadline, refreshed per MiB
	ReadSlice  time.Duration // read deadline while receiving, refreshed per MiB
	Ping       time.Duration // B's ping interval while the human decides
	Dial       time.Duration // B's dial and handshake
	Reply      time.Duration // B's wait for A's answer to HELLO
	Hint       time.Duration // A's "no connection yet" hint
	Grace      time.Duration // A's wait for B's report after its own writer failed
}

// timeouts are the deadlines in force; tests lower them through
// export_test.go.
var timeouts = timeoutSet{
	Handshake:  10 * time.Second,
	Hello:      90 * time.Second,
	Consent:    15 * time.Minute,
	Done:       15 * time.Minute,
	WriteSlice: 60 * time.Second,
	ReadSlice:  60 * time.Second,
	Ping:       30 * time.Second,
	Dial:       10 * time.Second,
	Reply:      90 * time.Second,
	Hint:       30 * time.Second,
	Grace:      5 * time.Second,
}

const firewallHint = "no connection yet — if a firewall prompt appeared, allow it; both machines must be on the same network"

type boundListener struct {
	ln   net.Listener
	link LinkAddr
	addr netip.AddrPort
}

type server struct {
	o          ServeOptions
	now        func() time.Time
	maxAttempt int
	tlsCfg     *tls.Config
	manifestHx string
	ctx        context.Context // the caller's: Ctrl-C cuts everything
	pairCtx    context.Context // ctx + TTL: pairing only
	cancelPair context.CancelFunc
	listeners  []boundListener
	sem        chan struct{}
	handlers   sync.WaitGroup
	emitMu     sync.Mutex

	mu        sync.Mutex
	active    bool // a connection has authenticated and is not finished
	attempts  int
	failures  int
	exhausted bool
	connected bool
	finished  bool
	result    ServeResult
	finalErr  error
}

// Serve runs one transfer server until a body is delivered, the attempts are
// exhausted, the TTL passes without a delivery, or ctx is cancelled.
func Serve(ctx context.Context, o ServeOptions) (ServeResult, error) {
	if o.Code.isZero() {
		return ServeResult{}, errors.New("transfer: pairing code not set")
	}
	if o.Body == nil {
		return ServeResult{}, errors.New("transfer: body writer not set")
	}
	if len(o.Manifest) > maxManifestFrame {
		return ServeResult{}, fmt.Errorf("transfer: manifest of %d bytes exceeds the %d-byte cap", len(o.Manifest), maxManifestFrame)
	}
	if o.Port < 0 || o.Port > 65535 {
		return ServeResult{}, fmt.Errorf("transfer: invalid port %d", o.Port)
	}
	ttl := o.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if ttl > maxTTL {
		return ServeResult{}, fmt.Errorf("transfer: TTL %s exceeds the %s maximum", ttl, maxTTL)
	}
	s := &server{
		o:          o,
		now:        o.Now,
		maxAttempt: o.MaxAttempts,
		ctx:        ctx,
		sem:        make(chan struct{}, maxInflight),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxAttempt <= 0 {
		s.maxAttempt = defaultMaxAttempts
	}
	listen := o.Listen
	if listen == nil {
		listen = net.Listen
	}
	start := s.now()

	cert, spki, err := EphemeralCert(ttl)
	if err != nil {
		return ServeResult{}, err
	}
	s.tlsCfg = ServerTLS(cert)
	sum := sha256.Sum256(o.Manifest)
	s.manifestHx = hex.EncodeToString(sum[:])

	local := o.Local
	if local == nil {
		local, err = LANAddrs(o.LAN)
		if err != nil {
			return ServeResult{}, err
		}
	}
	if len(local) == 0 {
		return ServeResult{}, errors.New("no local network address to listen on (check --iface; --allow-loopback for a same-machine trial)")
	}
	s.listeners, err = bindAll(local, o.Port, listen)
	if err != nil {
		return ServeResult{}, err
	}
	s.result.SPKI = spki
	for _, b := range s.listeners {
		s.result.Listeners = append(s.result.Listeners, b.addr)
	}

	s.pairCtx, s.cancelPair = context.WithTimeout(ctx, ttl)
	defer s.cancelPair()
	key := hex.EncodeToString(spki[:])[:8]
	for _, b := range s.listeners {
		s.emit(kindListen, b.addr.String(), fmt.Sprintf("listening on %s (%s), key %s", b.addr, b.link.Iface, key))
	}

	// When the pairing window ends, stop accepting.
	go func() {
		<-s.pairCtx.Done()
		s.closeListeners()
	}()
	if o.UI != nil {
		hint := time.AfterFunc(timeouts.Hint, func() {
			if s.pairCtx.Err() == nil && !s.hasConnected() {
				s.ui(firewallHint)
			}
		})
		defer hint.Stop()
	}

	var accepting sync.WaitGroup
	for i := range s.listeners {
		b := &s.listeners[i]
		accepting.Add(1)
		go func() {
			defer accepting.Done()
			s.acceptLoop(b)
		}()
	}
	accepting.Wait()
	s.handlers.Wait()

	s.mu.Lock()
	res, ferr, finished := s.result, s.finalErr, s.finished
	res.Attempts = s.attempts
	s.mu.Unlock()
	res.Duration = s.now().Sub(start)
	if finished {
		return res, ferr
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	text := fmt.Sprintf("code expired after %s; no pairing happened", fmtTTL(ttl))
	if res.Attempts > 0 {
		text = fmt.Sprintf("code expired after %s; nothing was delivered", fmtTTL(ttl))
	}
	s.emit(kindExpired, "", text)
	return res, withDetail(ErrExpired, text)
}

// bindAll binds one listener per address. With port 0 the first listener's
// kernel-chosen port is reused for the rest; addresses that refuse are
// dropped, and only a total failure is an error.
func bindAll(local []LinkAddr, port int, listen Listener) ([]boundListener, error) {
	var out []boundListener
	var errs []string
	for _, la := range local {
		if !la.Addr.IsValid() {
			continue
		}
		want := netip.AddrPortFrom(la.Addr, uint16(port))
		ln, err := listen("tcp", want.String())
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", want, err))
			continue
		}
		got, err := netip.ParseAddrPort(ln.Addr().String())
		if err != nil {
			ln.Close()
			errs = append(errs, fmt.Sprintf("%s: listener reports %q", want, ln.Addr().String()))
			continue
		}
		if port == 0 {
			port = int(got.Port())
		}
		out = append(out, boundListener{ln: ln, link: la, addr: got})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("could not listen on any local address: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

func fmtTTL(d time.Duration) string {
	if d < time.Second {
		return d.String()
	}
	secs := int(d.Round(time.Second).Seconds())
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

func (s *server) emit(kind, peer, text string) {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	ev := Event{Time: s.now(), Kind: kind, Peer: peer, Text: text}
	if s.o.Events != nil {
		s.o.Events(ev)
	} else if s.o.UI != nil {
		fmt.Fprintln(s.o.UI, text)
	}
}

// ui writes one line to the UI writer, serialised with events.
func (s *server) ui(text string) {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.o.UI != nil {
		fmt.Fprintln(s.o.UI, text)
	}
}

func (s *server) hasConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected
}

// closeListeners stops accepting on every address; accepted connections
// are unaffected.
func (s *server) closeListeners() {
	for i := range s.listeners {
		s.listeners[i].ln.Close()
	}
}

// finish records the serve's outcome once and ends pairing.
func (s *server) finish(mutate func(*ServeResult), err error) {
	s.mu.Lock()
	if !s.finished {
		s.finished = true
		if mutate != nil {
			mutate(&s.result)
		}
		s.finalErr = err
	}
	s.mu.Unlock()
	s.cancelPair()
}

// recordFailure counts a wrong code and reports the attempts left and
// whether the budget is spent.
func (s *server) recordFailure() (left int, exhausted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	s.failures++
	left = max(s.maxAttempt-s.failures, 0)
	if s.failures >= s.maxAttempt {
		s.exhausted = true
	}
	return left, s.exhausted
}

// exhaust stops accepting. With no transfer in flight the serve ends now;
// otherwise the in-flight transfer decides the outcome.
func (s *server) exhaust() {
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active {
		s.closeListeners()
		return
	}
	s.finish(nil, s.tooMany())
}

func (s *server) tooMany() error {
	return withDetail(ErrTooManyAttempts, fmt.Sprintf("%d failed pairing attempts; run bffs export --serve again for a new code", s.maxAttempt))
}

// claim marks a connection as the one authenticated transfer. It fails when
// another transfer is in flight, when the serve already finished, or when
// the pairing window closed meanwhile (expired reports the last case). The
// connection's claimed flag is set under s.mu, the same lock the pairing
// watcher checks before it closes the connection, so a TTL that fires during
// proof verification can never cut a connection that just authenticated.
func (s *server) claim(peer netip.AddrPort, claimed *bool) (ok, expired bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.pairCtx.Err() != nil && !s.finished {
		return false, true
	}
	if s.active || s.finished {
		return false, false
	}
	s.active = true
	*claimed = true
	s.result.Peer = peer
	s.result.PeerHost, s.result.PeerUser = "", ""
	return true, false
}

// release gives the transfer slot back after a decline or an abort so the
// listeners keep serving until the TTL.
func (s *server) release() {
	s.mu.Lock()
	s.active = false
	exhausted := s.exhausted && !s.finished
	s.mu.Unlock()
	if exhausted {
		s.finish(nil, s.tooMany())
	}
}

// acceptLoop accepts on one listener until it is closed or the pairing
// window ends. A transient accept error (EMFILE under a connection flood,
// ECONNABORTED) is retried with backoff rather than ending the listener: a
// hostile host may delay a transfer, never silently end the serve.
func (s *server) acceptLoop(b *boundListener) {
	backoff := acceptBackoffMin
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || s.pairCtx.Err() != nil {
				return
			}
			s.emit(kindError, b.addr.String(), fmt.Sprintf("accept on %s failed, retrying: %s", b.addr, errText(err)))
			select {
			case <-time.After(backoff):
			case <-s.pairCtx.Done():
				return
			}
			backoff = min(backoff*2, acceptBackoffMax)
			continue
		}
		backoff = acceptBackoffMin
		peer, ok := remoteAddrPort(conn)
		if !ok || !IsLAN(peer.Addr(), []LinkAddr{b.link}, s.o.LAN) {
			conn.Close()
			s.emit(kindReject, addrString(conn), fmt.Sprintf("refused %s: not on this listener's network", addrHost(conn)))
			continue
		}
		select {
		case s.sem <- struct{}{}:
		case <-s.pairCtx.Done():
			conn.Close()
			return
		}
		s.handlers.Add(1)
		go func() {
			defer s.handlers.Done()
			var once sync.Once
			release := func() { once.Do(func() { <-s.sem }) }
			defer release()
			s.handle(conn, peer, release)
		}()
	}
}

func remoteAddrPort(c net.Conn) (netip.AddrPort, bool) {
	if c.RemoteAddr() == nil {
		return netip.AddrPort{}, false
	}
	ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return netip.AddrPort{}, false
	}
	return ap, true
}

func addrString(c net.Conn) string {
	if c.RemoteAddr() == nil {
		return ""
	}
	return c.RemoteAddr().String()
}

func addrHost(c net.Conn) string {
	if ap, ok := remoteAddrPort(c); ok {
		return ap.Addr().String()
	}
	return addrString(c)
}

// handle drives one accepted connection through pairing and, if it
// authenticates, through consent and the transfer.
func (s *server) handle(raw net.Conn, peer netip.AddrPort, releaseSlot func()) {
	defer raw.Close()
	peerS := peer.String()
	ip := peer.Addr().String()

	// Pairing-phase watcher: the pairing window closing kills this
	// connection until it authenticates and detaches. claimed is only ever
	// set by claim under s.mu; the watcher reads it under the same lock, so
	// either claim sees the window still open and the watcher then sees
	// claimed, or the watcher closes first and claim sees the window shut.
	detach := make(chan struct{})
	watcherDone := make(chan struct{})
	claimed := false
	go func() {
		defer close(watcherDone)
		select {
		case <-s.pairCtx.Done():
			s.mu.Lock()
			c := claimed
			s.mu.Unlock()
			if !c {
				raw.Close()
			}
		case <-detach:
		}
	}()
	detached := false
	defer func() {
		if !detached {
			close(detach)
		}
		<-watcherDone
	}()

	tconn := tls.Server(raw, s.tlsCfg)
	_ = raw.SetDeadline(time.Now().Add(timeouts.Handshake))
	hctx, hcancel := context.WithTimeout(s.pairCtx, timeouts.Handshake)
	err := tconn.HandshakeContext(hctx)
	hcancel()
	releaseSlot()
	if err != nil {
		if s.pairCtx.Err() == nil {
			// The error may echo bytes the peer sent (its ALPN list, for
			// one), so it goes through cleanText like any peer text.
			s.emit(kindError, peerS, fmt.Sprintf("%s: TLS handshake failed: %s", ip, errText(err)))
		}
		return
	}
	cs := tconn.ConnectionState()
	if cs.NegotiatedProtocol != alpn {
		s.emit(kindError, peerS, fmt.Sprintf("%s: did not negotiate %s", ip, alpn))
		return
	}
	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
	s.emit(kindConnect, peerS, fmt.Sprintf("%s connected (TLS 1.3)", ip))

	_ = raw.SetDeadline(time.Now().Add(timeouts.Hello))
	m, err := readMsg(tconn)
	if err != nil {
		if s.pairCtx.Err() == nil {
			s.emit(kindError, peerS, fmt.Sprintf("%s: no valid hello: %s", ip, errText(err)))
		}
		return
	}
	if m.T != tHello || m.V != protocolVersion || !slices.Contains(m.Formats, bundleFormat) {
		s.emit(kindError, peerS, fmt.Sprintf("%s: unsupported hello (v=%d, formats=%v)", ip, m.V, m.Formats))
		return
	}
	proofB, err := base64.StdEncoding.DecodeString(m.Proof)
	if err != nil || len(proofB) != sha256.Size {
		s.emit(kindError, peerS, fmt.Sprintf("%s: malformed proof in hello", ip))
		return
	}
	ekm, err := exportKeyingMaterial(cs)
	if err != nil {
		s.emit(kindError, peerS, fmt.Sprintf("%s: exporter unavailable: %v", ip, err))
		return
	}
	// Serialised argon2; a serve that ended while this connection queued
	// pays nothing more.
	k, err := deriveKey(s.pairCtx, s.o.Code, ekm)
	if err != nil {
		return
	}
	if !VerifyProof(ClientProof(k, ekm), proofB) {
		left, exhausted := s.recordFailure()
		_ = raw.SetWriteDeadline(time.Now().Add(timeouts.Handshake))
		_ = writeMsg(tconn, badCodeMsg{T: tBadCode, AttemptsLeft: left})
		s.emit(kindBadCode, peerS, fmt.Sprintf("%s tried a wrong code (%d attempts left)", ip, left))
		if exhausted {
			s.exhaust()
		}
		return
	}
	if ok, expired := s.claim(peer, &claimed); !ok {
		if expired {
			s.emit(kindReject, peerS, fmt.Sprintf("%s gave the right code after the code expired; refused", ip))
		} else {
			s.emit(kindReject, peerS, fmt.Sprintf("%s gave the right code while another transfer is in progress; refused", ip))
		}
		return
	}

	// Authenticated: detach from the pairing window (the TTL never cuts an
	// authenticated transfer) and watch the caller's ctx instead. The
	// listeners stay open: a decline or an abort hands the slot back and A
	// keeps serving on every bound address until the TTL.
	close(detach)
	detached = true
	<-watcherDone
	stop := make(chan struct{})
	go func() {
		select {
		case <-s.ctx.Done():
			raw.Close()
		case <-stop:
		}
	}()
	defer close(stop)

	proofA := ServerProof(k, ekm)
	_ = raw.SetWriteDeadline(time.Now().Add(timeouts.WriteSlice))
	auth := authOKMsg{
		T:              tAuthOK,
		V:              protocolVersion,
		Bffs:           s.o.Version,
		Host:           s.o.Host,
		User:           s.o.User,
		Account:        s.o.Account,
		Proof:          base64.StdEncoding.EncodeToString(proofA),
		Compression:    s.o.Compression,
		ManifestSHA256: s.manifestHx,
	}
	if err := writeMsg(tconn, auth); err != nil {
		s.release()
		s.emit(kindError, peerS, fmt.Sprintf("%s: sending auth-ok: %s", ip, errText(err)))
		return
	}
	s.emit(kindCodeOK, peerS, fmt.Sprintf("%s gave the right code", ip))
	mw := &deadlineWriter{w: tconn, conn: raw, ctx: s.ctx}
	if err := writeFrame(mw, s.o.Manifest, maxManifestFrame); err != nil {
		s.release()
		if s.ctx.Err() == nil {
			s.emit(kindError, peerS, fmt.Sprintf("%s: sending manifest: %s", ip, errText(err)))
		}
		return
	}
	s.emit(kindManifestSent, peerS, fmt.Sprintf("manifest sent to %s (%d bytes)", ip, len(s.o.Manifest)))

	// Consent: wait for accept or reject; pings refresh the deadline.
	var host, user string
consent:
	for {
		_ = raw.SetReadDeadline(time.Now().Add(timeouts.Consent))
		m, err := readMsg(tconn)
		if err != nil {
			s.release()
			if s.ctx.Err() == nil {
				s.emit(kindError, peerS, fmt.Sprintf("%s: connection closed before it answered: %s", ip, errText(err)))
			}
			return
		}
		switch m.T {
		case tPing:
		case tReject:
			s.release()
			name := cleanText(m.Host, 64)
			if name == "" {
				name = ip
			}
			if m.Reason == reasonDryRun {
				s.emit(kindReject, peerS, fmt.Sprintf("%s: dry run", name))
			} else {
				s.emit(kindReject, peerS, fmt.Sprintf("%s declined", name))
			}
			_ = raw.SetWriteDeadline(time.Now().Add(timeouts.Handshake))
			tconn.Close()
			return
		case tAccept:
			if m.V != protocolVersion {
				s.release()
				s.emit(kindError, peerS, fmt.Sprintf("%s: unsupported accept (v=%d)", ip, m.V))
				return
			}
			host, user = cleanText(m.Host, 64), cleanText(m.User, 64)
			break consent
		default:
			s.release()
			s.emit(kindError, peerS, fmt.Sprintf("%s: unexpected %q while waiting for consent", ip, cleanText(m.T, 32)))
			return
		}
	}
	name := host
	if name == "" {
		name = ip
	}
	s.mu.Lock()
	s.result.PeerHost, s.result.PeerUser = host, user
	s.mu.Unlock()
	s.emit(kindAccept, peerS, fmt.Sprintf("%s accepted", name))

	// Stream the body while a reader watches for an early DONE.
	bodyCtx, bodyCancel := context.WithCancel(s.ctx)
	defer bodyCancel()
	_ = raw.SetReadDeadline(time.Time{})
	doneCh := make(chan Done, 1)
	readErr := make(chan error, 1)
	go func() {
		for {
			m, err := readMsg(tconn)
			if err != nil {
				readErr <- err
				bodyCancel()
				return
			}
			switch m.T {
			case tDone:
				doneCh <- Done{OK: m.OK, Entries: m.Entries, Bytes: m.Bytes, Reason: cleanText(m.Reason, 512)}
				if !m.OK {
					bodyCancel()
				}
				return
			case tPing:
			default:
				// Strict schema: anything else while streaming ends the
				// transfer, like an unknown message anywhere else.
				readErr <- &protocolErr{fmt.Sprintf("unexpected %q while streaming", cleanText(m.T, 32))}
				bodyCancel()
				return
			}
		}
	}()
	dw := &deadlineWriter{w: tconn, conn: raw, ctx: bodyCtx, onProgress: func(n int64) {
		s.emit(kindProgress, peerS, fmt.Sprintf("sent %d MiB to %s", n/mib, name))
	}}
	s.emit(kindSending, peerS, fmt.Sprintf("sending to %s", name))
	berr := s.o.Body(bodyCtx, dw)
	written := dw.n

	if berr == nil {
		// Everything written; wait for the report.
		_ = raw.SetReadDeadline(time.Now().Add(timeouts.Done))
		select {
		case d := <-doneCh:
			s.reported(name, peerS, d, written)
			_ = raw.SetWriteDeadline(time.Now().Add(timeouts.Handshake))
			tconn.Close()
		case err := <-readErr:
			if s.ctx.Err() != nil {
				return
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				s.finish(func(r *ServeResult) { r.Bytes = written }, fmt.Errorf("sent %d bytes but %s never reported completion", written, name))
				s.emit(kindError, peerS, fmt.Sprintf("no completion report from %s", name))
				return
			}
			s.aborted(name, peerS, err)
		case <-s.ctx.Done():
		}
		return
	}
	// The body writer stopped early: because the peer reported a failure,
	// because the peer vanished, or because of A's own error. The peer's
	// report, if any, is already in flight, so give the reader a moment.
	if s.ctx.Err() != nil {
		return
	}
	grace := time.NewTimer(timeouts.Grace)
	defer grace.Stop()
	select {
	case d := <-doneCh:
		s.reported(name, peerS, d, written)
	case err := <-readErr:
		s.aborted(name, peerS, err)
	case <-s.ctx.Done():
	case <-grace.C:
		// A's own failure, not the peer's.
		s.finish(func(r *ServeResult) { r.Bytes = written }, fmt.Errorf("sending the bundle: %w", berr))
		s.emit(kindError, peerS, fmt.Sprintf("sending to %s failed: %v", name, berr))
	}
}

// reported ends the serve with B's report.
func (s *server) reported(name, peerS string, d Done, written int64) {
	if d.OK {
		s.finish(func(r *ServeResult) { r.Done = d; r.Bytes = written }, nil)
		s.emit(kindDone, peerS, fmt.Sprintf("%s received %d entries (%d bytes)", name, d.Entries, d.Bytes))
		return
	}
	reason := d.Reason
	if reason == "" {
		reason = "no reason given"
	}
	s.finish(func(r *ServeResult) { r.Done = d; r.Bytes = written }, fmt.Errorf("%s reported: %s", name, reason))
	s.emit(kindError, peerS, fmt.Sprintf("%s reported: %s", name, reason))
}

// aborted keeps serving after the peer vanished mid-transfer, or broke the
// protocol (err is then a *protocolErr and named in the event).
func (s *server) aborted(name, peerS string, err error) {
	s.release()
	text := fmt.Sprintf("transfer aborted by peer %s", name)
	var pe *protocolErr
	if errors.As(err, &pe) {
		text += " (" + pe.msg + ")"
	}
	s.emit(kindError, peerS, text)
}

// protocolErr marks a message the peer should never have sent.
type protocolErr struct{ msg string }

func (e *protocolErr) Error() string { return e.msg }

// errText renders an error for an event: control characters stripped and the
// length capped, because some errors echo bytes the peer chose.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return cleanText(err.Error(), 200)
}

// deadlineWriter refreshes the write deadline per MiB and stops when its
// context is cancelled (an early done{ok:false} or Ctrl-C).
type deadlineWriter struct {
	w          io.Writer
	conn       net.Conn
	ctx        context.Context
	n          int64
	since      int64
	nextReport int64
	onProgress func(n int64)
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if err := d.ctx.Err(); err != nil {
		return 0, err
	}
	if d.n == 0 {
		_ = d.conn.SetWriteDeadline(time.Now().Add(timeouts.WriteSlice))
		d.nextReport = progressStep
	}
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > mib {
			chunk = p[:mib]
		}
		n, err := d.w.Write(chunk)
		total += n
		d.n += int64(n)
		d.since += int64(n)
		if err != nil {
			if cerr := d.ctx.Err(); cerr != nil {
				return total, cerr
			}
			return total, err
		}
		p = p[n:]
		if d.since >= mib {
			d.since = 0
			_ = d.conn.SetWriteDeadline(time.Now().Add(timeouts.WriteSlice))
		}
		if d.n >= d.nextReport && d.onProgress != nil {
			d.nextReport += progressStep
			d.onProgress(d.n)
		}
		if err := d.ctx.Err(); err != nil {
			return total, err
		}
	}
	return total, nil
}

// cleanText strips control characters from peer-supplied text and caps its
// length, so a hostile peer cannot smuggle terminal escapes into events.
func cleanText(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n == maxRunes {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
