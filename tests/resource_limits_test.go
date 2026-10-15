package integration

import (
	"strings"
	"testing"
	"time"

	netsmtp "net/smtp"

	esmtp "github.com/emersion/go-smtp"
)

func TestSMTPMessageSizeLimitRejectsOversizedAndKeepsServerHealthy(t *testing.T) {
	store := &e2eStore{}
	server := esmtp.NewServer(&e2eUpstreamBackend{store: store})
	server.Domain = "mailshield.test"
	server.MaxMessageBytes = 512
	server.ReadTimeout = 2 * time.Second
	server.WriteTimeout = 2 * time.Second
	addr := startE2ESMTPServer(t, server)

	// Deliberately exceed the configured message limit by a wide margin.
	// 每行 100 个字符，避免触发 SMTP 单行长度限制。
	// 正文总长度超过 512 字节。
	oversizedBody := "Subject: oversized\r\n\r\n" +
		strings.Repeat(strings.Repeat("X", 100)+"\r\n", 20)
	err := netsmtp.SendMail(
		addr,
		nil,
		"sender@example.test",
		[]string{"recipient@example.test"},
		[]byte(oversizedBody),
	)
	if err == nil {
		t.Fatal("expected oversized SMTP message to be rejected")
	}
	t.Logf("oversized message rejected as expected: %v", err)

	// A valid message after the rejected transaction must still be accepted.
	validBody := []byte("Subject: valid\r\n\r\nmailshield-after-size-limit-marker\r\n")
	if err := netsmtp.SendMail(
		addr,
		nil,
		"sender@example.test",
		[]string{"recipient@example.test"},
		validBody,
	); err != nil {
		t.Fatalf("valid message failed after oversized message rejection: %v", err)
	}

	bodies := store.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("upstream backend received %d messages; want exactly 1 valid message", len(bodies))
	}
	if !strings.Contains(string(bodies[0]), "mailshield-after-size-limit-marker") {
		t.Fatalf("unexpected message reached backend: %q", bodies[0])
	}
	if strings.Contains(string(bodies[0]), strings.Repeat("X", 128)) {
		t.Fatal("oversized message content unexpectedly reached backend")
	}

	t.Logf("message size limit passed: max=%d bytes; oversized message rejected; following valid message relayed", server.MaxMessageBytes)
}
