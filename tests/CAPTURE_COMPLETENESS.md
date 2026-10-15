# MailShield capture validation notes

## Confirmed by the inbound STARTTLS test

- `go-smtp` v0.25.0's `Server.Debug` contains observable SMTP commands/responses and post-STARTTLS application content in this test.
- `Server.Debug` is one combined stream; it does not provide explicit per-direction labels.
- Wrapping the raw accepted `net.Conn` captures TLS records in both directions, with direction inferred from server-side `Read` versus `Write`.
- The server backend's `smtp.Conn.TLSConnectionState()` reports handshake completion and TLS version. `PeerCertificates` is zero in this server-side test because the client did not present a client certificate; this is expected and is not the MailShield server certificate.
- The MailShield server certificate is observed on the TLS client side in `tls.ConnectionState().PeerCertificates`.

## Added outbound TLS test

`TestOutboundImplicitTLSCapturesUpstreamCertificateAndRawDirections` models a TLS connection from MailShield to an upstream SMTP server:

- The outbound TLS client's `ConnectionState().PeerCertificates` exposes the upstream server certificate.
- Wrapping the underlying raw `net.Conn` captures TLS records in both directions and distinguishes them by local `Read`/`Write` direction.

This added test uses Go's standard TLS APIs and a local fake SMTP server. It is not a test of `go-smtp`'s SMTP client implementation, and it does not yet prove that a complete two-leg MailShield proxy preserves and correlates all data.

## Current engineering conclusion

- Raw TLS bytes can be captured directionally at each TCP connection's `net.Conn` boundary.
- TLS peer certificates are available from the TLS endpoint that acts as a client; the local server's certificate is available from its TLS configuration / certificate selection path, not from server-side `PeerCertificates` unless mutual TLS is used.
- `Server.Debug` is useful for diagnostics but insufficient as the sole production capture API because it does not provide stable direction metadata or a structured per-connection event model.
- Before choosing a fork, next validate whether a small adapter/hook can be added around the SMTP application read/write path without modifying protocol behavior. If `go-smtp` offers no suitable hook, compare a minimal fork against implementing the SMTP session layer ourselves.

## Proposed directional SMTP plaintext observer

The patch in `patches/go-smtp-v0.25.0-traffic-observer.patch` adds a small callback at the protocol reader/writer boundary. The test harness asserts that client commands and DATA content are labelled `client_to_server`, while server responses are labelled `server_to_client`, across STARTTLS. This is a proposed patch and must be run by the user against the exact v0.25.0 source before treating it as validated.

## Multi-transaction and concurrent connection test

`transaction_test.go` adds a test with four SMTP transactions: two on one TCP connection, plus one transaction on each of two concurrent connections. It checks that the backend receives all four DATA bodies and that the observer sees each unique body marker.

This test also documents a critical design gap: a global callback shaped as `TrafficObserver(direction, data)` carries no connection ID. Under concurrent connections, the bytes are visible but cannot reliably be attributed to a specific TCP connection or SMTP session. Before production, the observer should be connection-scoped (for example, an observer factory receiving a stable connection context/ID), and transaction association should be implemented above that layer.
