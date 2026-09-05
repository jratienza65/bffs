// Package transfer moves one bundle between two machines on the same local
// network, gated by a short pairing code the user reads off one screen and
// types on the other. It is deliberately bundle-agnostic: the sender supplies
// a manifest ([]byte) and a body writer, the receiver a sink (io.Reader), and
// nothing here knows what a .bffs file looks like.
//
// # Roles
//
// A (Serve) owns the data. It generates the code, listens on on-link
// addresses only, streams one body to the first peer that proves it knows the
// code, then returns. B (Fetch) connects, is asked for the code, proves
// first, verifies A's proof, reviews the manifest, receives, and reports.
//
// # Message sequence
//
// Every control message is a frame: uint32 big-endian length || JSON object,
// capped at 4 KiB. The manifest travels as one raw frame (same header, raw
// bytes, capped at 16 MiB). The body after "accept" is written straight to
// the connection with no framing; the receiver knows where it ends.
//
//	B → A  TLS 1.3 handshake (ALPN "bffs-transfer/1"; A's cert is ephemeral, B pins nothing)
//	B → A  {"t":"hello","v":1,"formats":[1],"proof":<b64 proofB>}
//	A → B  {"t":"bad-code","attempts_left":n}                      (mismatch: close; 3 strikes end the serve)
//	A → B  {"t":"auth-ok","v":1,"bffs":…,"host":…,"user":…,"account":…,
//	        "proof":<b64 proofA>,"compression":n,"manifest_sha256":<hex>}
//	A → B  raw manifest frame
//	B → A  {"t":"ping"} every 30 s while the human decides
//	B → A  {"t":"accept","v":1,"bffs":…,"host":…,"user":…} | {"t":"reject","reason":"declined"|"dry-run","host":…,"user":…}
//	A → B  body bytes
//	B → A  {"t":"done","ok":true,"entries":n,"bytes":n} | {"t":"done","ok":false,"reason":…}
//
// # What each side proves
//
// Both sides export 32 bytes of keying material from the TLS connection
// (RFC 5705, label "EXPORTER-bffs-transfer/v1") and derive
// K = argon2id(code, salt = ekm). proofB = HMAC(K, "client-proof" || ekm)
// tells A "I know the code, and I computed this on the connection whose
// exporter is ekm". proofA = HMAC(K, "server-proof" || ekm) tells B the same
// about A. Because ekm differs per connection, a relay sitting between the two
// has different exporters on its two legs and cannot forward either proof.
// B speaks first so that only a party already intercepting B's connection
// ever obtains a (proof, ekm) pair; a stranger connecting to A gets exactly
// one guess per connection and three per serve.
//
// # Residual risks (SECURITY.md restates these)
//
//   - An active interceptor of B's connection obtains one offline target per
//     intercepted connection: 2^40 codes × argon2id(t=3, m=64 MiB) each, with
//     no precomputation possible (the salt is the exporter) and nothing left
//     to attack once the code's TTL passes or the serve ends.
//   - A hostile host on the LAN can deny (not break) a transfer: eight
//     concurrent handshakes, ten seconds each, and three code guesses per
//     serve are the only budget.
//   - Anyone who can see A's screen within the TTL can pull the bundle; the
//     code is the whole secret.
//
// The code never crosses the wire, never appears in errors, events or the
// UI writer, and Code has no String method.
package transfer
