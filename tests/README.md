# MailShield go-smtp capture validation kit

Tests against `github.com/emersion/go-smtp` v0.25.0. Requires Go 1.24+.

## What this kit validates

- Inbound SMTP with STARTTLS, including commands/responses and DATA content.
- Raw TCP/TLS capture in both directions.
- Inbound server certificate and outbound upstream certificate visibility.
- A proposed small `go-smtp` patch that exposes directional SMTP application bytes through `Server.TrafficObserver`.

The observer patch is an experiment, not production-ready code. The callback runs synchronously in protocol I/O and must be fast and concurrency-safe. A production implementation should enqueue bounded capture events, define overload/drop behavior, and redact or protect sensitive content as appropriate.

## Run on Windows PowerShell

1. Extract this ZIP.
2. From the extracted directory, clone the exact dependency version and apply the patch:

```powershell
git clone --depth 1 --branch v0.25.0 https://github.com/emersion/go-smtp.git third_party/go-smtp
git -C third_party/go-smtp apply ..\patches\go-smtp-v0.25.0-traffic-observer.patch
go mod edit -replace=github.com/emersion/go-smtp=./third_party/go-smtp
go test -v ./...
```

If `git -C ... apply` cannot resolve the patch path, run the apply command from inside `third_party/go-smtp` using the absolute path to the patch file.

The test should report directional plaintext capture and assert that client commands plus DATA content appear in `client_to_server`, while SMTP response codes appear in `server_to_client`, including after STARTTLS.

## Outbound TLS leg test

`TestOutboundImplicitTLSCapturesUpstreamCertificateAndRawDirections` models MailShield-to-upstream SMTP TLS using Go's standard TLS APIs and a local fake server. It is not a test of `go-smtp`'s outbound client API and not a full proxy end-to-end test. The self-signed certificate is accepted only in this local test harness.

## Multi-transaction / concurrent-connection validation

`transaction_test.go` checks two mail transactions on one SMTP connection and two additional connections sending mail concurrently. It asserts that the backend receives four distinct DATA bodies and that the directional observer sees all four body markers.

The next incremental patch changes the callback to `TrafficObserver(conn, direction, data)`, passing the stable `*smtp.Conn` for every event. The test groups the two transactions on one connection together and verifies that two concurrent connections remain isolated. This proves connection attribution at the library callback boundary; MailShield must still assign its own durable Connection ID and separately correlate inbound/outbound SMTP legs and transaction IDs.

Run after applying the local go-smtp observer patch:

```bash
go test -v ./...
```
