# MailShield HTTP CONNECT E2E validation

Copy `http_connect_test.go` into the existing `mailshield-go-smtp-integration` directory. Do not replace existing files or reapply the go-smtp patches.

Run the test:

```bash
go test -v -run '^TestSMTPProxyThroughHTTPConnect$' -count=1
```

Then run the race detector:

```bash
go test -race -run '^TestSMTPProxyThroughHTTPConnect$' -count=1
```

This local-only test starts a fake HTTP CONNECT proxy and a fake upstream SMTP server. It verifies the CONNECT tunnel is used, the upstream receives the complete message body, inbound SMTP plaintext capture remains available, and outbound raw capture observes both directions. It does not test proxy authentication, TLS certificate validation, or a production HTTP proxy implementation.
