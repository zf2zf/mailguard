# MailShield abnormal-disconnect and timeout validation

Copy `abnormal_test.go` into the existing `mailshield-go-smtp-integration` test module root. Keep all previously passing tests and the local `go-smtp` patch unchanged.

Run the focused tests:

```bash
go test -v -run 'TestSMTP(ClientDisconnectMidDataDoesNotAffectOtherClients|IdleReadTimeoutDoesNotAffectOtherClients|UpstreamDisconnectDoesNotCrashInboundServer)$' -count=1
```

Then run them with Go's race detector:

```bash
go test -race -run 'TestSMTP(ClientDisconnectMidDataDoesNotAffectOtherClients|IdleReadTimeoutDoesNotAffectOtherClients|UpstreamDisconnectDoesNotCrashInboundServer)$' -count=1
```

These are local-only tests. They verify that an interrupted DATA transaction, an idle read timeout, or an upstream connection drop does not prevent a subsequent inbound SMTP connection from being served. They do not yet prove production-grade resource leak freedom or every TLS handshake failure mode.
