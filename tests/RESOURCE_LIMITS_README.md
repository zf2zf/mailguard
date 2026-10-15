# MailShield resource-limit validation: message size

This incremental test validates the `go-smtp` `MaxMessageBytes` limit and checks that a rejected oversized message does not prevent a subsequent valid message from being delivered.

Copy `resource_limits_test.go` into the existing `mailshield-go-smtp-integration` test module. Do not overwrite existing files.

Run:

```bash
go test -v -run '^TestSMTPMessageSizeLimit' -count=1
go test -race -run '^TestSMTPMessageSizeLimit' -count=1
```

The oversized message is expected to fail, while the following small message must succeed. This test covers the SMTP server's message-size limit only; it does not yet validate MailShield's future capture queue or total memory limits.
