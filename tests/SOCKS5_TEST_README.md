# MailShield SOCKS5 E2E validation

This is an incremental test for the existing `mailshield-go-smtp-integration` lab. It does not modify `go.mod` or the vendored `go-smtp` patch.

## Install

Copy `socks5_test.go` into the root of the existing test project. Keep all existing test files.

## Run

```bash
go test -v -run '^TestSMTPProxyThroughSOCKS5($|UsernamePassword$)' -count=1
go test -race -run '^TestSMTPProxyThroughSOCKS5($|UsernamePassword$)' -count=1
```

The tests start local SMTP and SOCKS5 fixtures. They verify SOCKS5 negotiation, CONNECT target, optional RFC 1929 username/password authentication, SMTP message delivery, inbound plaintext capture, and outbound bidirectional raw-byte capture. This is a test fixture, not a production SOCKS5 implementation.
