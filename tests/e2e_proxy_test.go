package integration

import (
	"bytes"
	"io"
	"net"
	netsmtp "net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	esmtp "github.com/emersion/go-smtp"
)

// This test models the first MailShield E2E relay slice:
// SMTP client -> inbound SMTP server/session -> outbound SMTP client -> upstream SMTP server.
// It is intentionally local-only and uses no TLS or external services.

type e2eStore struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (s *e2eStore) add(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, append([]byte(nil), body...))
}

func (s *e2eStore) snapshot() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.bodies))
	for i := range s.bodies {
		out[i] = append([]byte(nil), s.bodies[i]...)
	}
	return out
}

type e2eUpstreamBackend struct{ store *e2eStore }

func (b *e2eUpstreamBackend) NewSession(*esmtp.Conn) (esmtp.Session, error) {
	return &e2eUpstreamSession{store: b.store}, nil
}

type e2eUpstreamSession struct{ store *e2eStore }

func (*e2eUpstreamSession) Mail(string, *esmtp.MailOptions) error { return nil }
func (*e2eUpstreamSession) Rcpt(string, *esmtp.RcptOptions) error { return nil }
func (s *e2eUpstreamSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.store.add(body)
	return nil
}
func (*e2eUpstreamSession) Reset()        {}
func (*e2eUpstreamSession) Logout() error { return nil }

type e2eRelayBackend struct {
	upstreamAddr string
	outboundRaw  *capture
}

func (b *e2eRelayBackend) NewSession(*esmtp.Conn) (esmtp.Session, error) {
	return &e2eRelaySession{backend: b}, nil
}

type e2eRelaySession struct{ backend *e2eRelayBackend }

func (*e2eRelaySession) Mail(string, *esmtp.MailOptions) error { return nil }
func (*e2eRelaySession) Rcpt(string, *esmtp.RcptOptions) error { return nil }
func (s *e2eRelaySession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", s.backend.upstreamAddr, 3*time.Second)
	if err != nil {
		return err
	}
	if s.backend.outboundRaw != nil {
		conn = &tappedConn{Conn: conn, c: s.backend.outboundRaw}
	}
	client, err := netsmtp.NewClient(conn, "upstream.test")
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer client.Close()

	if err := client.Mail("sender@example.test"); err != nil {
		return err
	}
	if err := client.Rcpt("recipient@example.test"); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}
func (*e2eRelaySession) Reset()        {}
func (*e2eRelaySession) Logout() error { return nil }

func startE2ESMTPServer(t *testing.T, server *esmtp.Server) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Log("SMTP server shutdown timed out")
		}
	})
	return listener.Addr().String()
}

func TestSMTPProxyEndToEndRelaysMessageAndCapturesBothLegs(t *testing.T) {
	upstreamStore := &e2eStore{}
	upstreamServer := esmtp.NewServer(&e2eUpstreamBackend{store: upstreamStore})
	upstreamServer.Domain = "upstream.test"
	upstreamServer.ReadTimeout = 5 * time.Second
	upstreamServer.WriteTimeout = 5 * time.Second
	upstreamAddr := startE2ESMTPServer(t, upstreamServer)

	outboundRaw := &capture{}
	relayBackend := &e2eRelayBackend{upstreamAddr: upstreamAddr, outboundRaw: outboundRaw}
	observer := &plainCapture{}
	inboundServer := esmtp.NewServer(relayBackend)
	inboundServer.Domain = "mailshield.test"
	inboundServer.TrafficObserver = observer.observe
	inboundServer.ReadTimeout = 5 * time.Second
	inboundServer.WriteTimeout = 5 * time.Second
	inboundAddr := startE2ESMTPServer(t, inboundServer)

	body := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: MailShield E2E\r\n\r\nmailshield-e2e-body-marker\r\n")
	if err := netsmtp.SendMail(inboundAddr, nil, "sender@example.test", []string{"recipient@example.test"}, body); err != nil {
		t.Fatalf("send message through inbound MailShield SMTP endpoint: %v", err)
	}

	got := upstreamStore.snapshot()
	if len(got) != 1 {
		t.Fatalf("upstream received %d messages; want 1", len(got))
	}
	if !bytes.Contains(got[0], []byte("mailshield-e2e-body-marker")) {
		t.Fatalf("upstream message missing body marker: %q", got[0])
	}

	clientToProxy := observer.joined(esmtp.TrafficClientToServer)
	proxyToClient := observer.joined(esmtp.TrafficServerToClient)
	if !strings.Contains(clientToProxy, "MAIL FROM:") || !strings.Contains(clientToProxy, "mailshield-e2e-body-marker") {
		t.Fatalf("inbound plaintext capture missing SMTP command or body marker; captured=%q", clientToProxy)
	}
	if !strings.Contains(proxyToClient, "220 ") || !strings.Contains(proxyToClient, "250 ") {
		t.Fatalf("inbound plaintext capture missing SMTP responses; captured=%q", proxyToClient)
	}

	reads, writes := outboundRaw.directionCounts()
	if reads == 0 || writes == 0 {
		t.Fatalf("outbound raw capture incomplete: upstream->proxy reads=%d proxy->upstream writes=%d", reads, writes)
	}
	t.Logf("E2E relay passed: inbound plaintext client->proxy=%d bytes, proxy->client=%d bytes; outbound raw upstream->proxy events=%d, proxy->upstream events=%d; upstream message body verified", len(clientToProxy), len(proxyToClient), reads, writes)
}
