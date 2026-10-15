package integration

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	netsmtp "net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	esmtp "github.com/emersion/go-smtp"
)

type tlsE2EStore struct {
	mu     sync.Mutex
	bodies [][]byte
	tlsOK  bool
	tlsVer uint16
	peerCN string
}

func (s *tlsE2EStore) add(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, append([]byte(nil), body...))
}

func (s *tlsE2EStore) snapshot() ([][]byte, bool, uint16, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bodies := make([][]byte, len(s.bodies))
	for i := range s.bodies {
		bodies[i] = append([]byte(nil), s.bodies[i]...)
	}
	return bodies, s.tlsOK, s.tlsVer, s.peerCN
}

type tlsE2EUpstreamBackend struct{ store *tlsE2EStore }

func (b *tlsE2EUpstreamBackend) NewSession(c *esmtp.Conn) (esmtp.Session, error) {
	return &tlsE2EUpstreamSession{conn: c, store: b.store}, nil
}

type tlsE2EUpstreamSession struct {
	conn  *esmtp.Conn
	store *tlsE2EStore
}

func (s *tlsE2EUpstreamSession) Mail(string, *esmtp.MailOptions) error {
	state, ok := s.conn.TLSConnectionState()
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.store.tlsOK = ok && state.HandshakeComplete
	if ok {
		s.store.tlsVer = state.Version
		if len(state.PeerCertificates) > 0 {
			s.store.peerCN = state.PeerCertificates[0].Subject.CommonName
		}
	}
	return nil
}
func (*tlsE2EUpstreamSession) Rcpt(string, *esmtp.RcptOptions) error { return nil }
func (s *tlsE2EUpstreamSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.store.add(body)
	return nil
}
func (*tlsE2EUpstreamSession) Reset()        {}
func (*tlsE2EUpstreamSession) Logout() error { return nil }

type tlsE2ERelayBackend struct {
	upstreamAddr string
	outboundRaw  *capture
}

func (b *tlsE2ERelayBackend) NewSession(*esmtp.Conn) (esmtp.Session, error) {
	return &tlsE2ERelaySession{backend: b}, nil
}

type tlsE2ERelaySession struct{ backend *tlsE2ERelayBackend }

func (*tlsE2ERelaySession) Mail(string, *esmtp.MailOptions) error { return nil }
func (*tlsE2ERelaySession) Rcpt(string, *esmtp.RcptOptions) error { return nil }
func (s *tlsE2ERelaySession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	raw, err := net.DialTimeout("tcp", s.backend.upstreamAddr, 3*time.Second)
	if err != nil {
		return err
	}
	if s.backend.outboundRaw != nil {
		raw = &tappedConn{Conn: raw, c: s.backend.outboundRaw}
	}
	_ = raw.SetDeadline(time.Now().Add(8 * time.Second))
	client, err := netsmtp.NewClient(raw, "upstream.test")
	if err != nil {
		_ = raw.Close()
		return err
	}
	defer client.Close()
	if err := client.StartTLS(&tls.Config{ServerName: "upstream.test", InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
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
func (*tlsE2ERelaySession) Reset()        {}
func (*tlsE2ERelaySession) Logout() error { return nil }

func startTLSCaptureServer(t *testing.T, server *esmtp.Server, raw *capture) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		listener = &tappedListener{Listener: listener, c: raw}
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Log("TLS E2E SMTP server shutdown timed out")
		}
	})
	return listener.Addr().String()
}

func TestSMTPProxyEndToEndSTARTTLSBothLegsAndCaptures(t *testing.T) {
	inCert, err := makeCertificateWithCN("mailshield.test")
	if err != nil {
		t.Fatal(err)
	}
	upCert, err := makeCertificateWithCN("upstream.test")
	if err != nil {
		t.Fatal(err)
	}

	upstreamStore := &tlsE2EStore{}
	upstreamServer := esmtp.NewServer(&tlsE2EUpstreamBackend{store: upstreamStore})
	upstreamServer.Domain = "upstream.test"
	upstreamServer.TLSConfig = &tls.Config{Certificates: []tls.Certificate{upCert}, MinVersion: tls.VersionTLS12}
	upstreamServer.ReadTimeout = 8 * time.Second
	upstreamServer.WriteTimeout = 8 * time.Second
	outboundRaw := &capture{}
	upstreamAddr := startTLSCaptureServer(t, upstreamServer, nil)

	observer := &plainCapture{}
	inboundRaw := &capture{}
	relayBackend := &tlsE2ERelayBackend{upstreamAddr: upstreamAddr, outboundRaw: outboundRaw}
	inboundServer := esmtp.NewServer(&tlsE2ERelayBackendAdapter{backend: relayBackend})
	inboundServer.Domain = "mailshield.test"
	inboundServer.TLSConfig = &tls.Config{Certificates: []tls.Certificate{inCert}, MinVersion: tls.VersionTLS12}
	inboundServer.TrafficObserver = observer.observe
	inboundServer.ReadTimeout = 8 * time.Second
	inboundServer.WriteTimeout = 8 * time.Second
	inboundAddr := startTLSCaptureServer(t, inboundServer, inboundRaw)

	conn, err := net.DialTimeout("tcp", inboundAddr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
	client, err := netsmtp.NewClient(conn, "mailshield.test")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.StartTLS(&tls.Config{ServerName: "mailshield.test", InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatalf("client STARTTLS to MailShield: %v", err)
	}
	clientTLS, ok := client.TLSConnectionState()
	if !ok || !clientTLS.HandshakeComplete || len(clientTLS.PeerCertificates) == 0 {
		t.Fatalf("client did not observe MailShield TLS certificate: ok=%v state=%+v", ok, clientTLS)
	}
	if got := clientTLS.PeerCertificates[0].Subject.CommonName; got != "mailshield.test" {
		t.Fatalf("MailShield certificate CN=%q, want mailshield.test", got)
	}

	body := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: MailShield TLS E2E\r\n\r\nmailshield-tls-e2e-body-marker\r\n")
	if err := client.Mail("sender@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("recipient@example.test"); err != nil {
		t.Fatal(err)
	}
	w, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Quit(); err != nil {
		t.Logf("SMTP QUIT returned: %v", err)
	}

	gotBodies, upstreamTLSOK, upstreamTLSVersion, upstreamPeerCN := upstreamStore.snapshot()
	if len(gotBodies) != 1 {
		t.Fatalf("upstream received %d messages; want 1", len(gotBodies))
	}
	if !bytes.Contains(gotBodies[0], []byte("mailshield-tls-e2e-body-marker")) {
		t.Fatalf("upstream message missing marker: %q", gotBodies[0])
	}
	if !upstreamTLSOK || upstreamTLSVersion < tls.VersionTLS12 {
		t.Fatalf("MailShield->upstream TLS not observed by upstream: ok=%v version=%#x", upstreamTLSOK, upstreamTLSVersion)
	}
	if upstreamPeerCN != "" {
		t.Logf("upstream observed client certificate CN=%q (normally empty without mTLS)", upstreamPeerCN)
	}

	inClientPlain := observer.joined(esmtp.TrafficClientToServer)
	inServerPlain := observer.joined(esmtp.TrafficServerToClient)
	if !strings.Contains(inClientPlain, "MAIL FROM:") || !strings.Contains(inClientPlain, "mailshield-tls-e2e-body-marker") {
		t.Fatalf("inbound plaintext observer missing SMTP command/body after STARTTLS: %q", inClientPlain)
	}
	if !strings.Contains(inServerPlain, "250 ") {
		t.Fatalf("inbound plaintext observer missing SMTP response after STARTTLS: %q", inServerPlain)
	}

	inboundRaw.mu.Lock()
	inReadTLS := inboundRaw.directionHasTLSRecord(inboundRaw.readEvents)
	inWriteTLS := inboundRaw.directionHasTLSRecord(inboundRaw.writeEvents)
	inReadCount, inWriteCount := len(inboundRaw.readEvents), len(inboundRaw.writeEvents)
	inboundRaw.mu.Unlock()
	outboundRaw.mu.Lock()
	outReadTLS := outboundRaw.directionHasTLSRecord(outboundRaw.readEvents)
	outWriteTLS := outboundRaw.directionHasTLSRecord(outboundRaw.writeEvents)
	outReadCount, outWriteCount := len(outboundRaw.readEvents), len(outboundRaw.writeEvents)
	outboundRaw.mu.Unlock()
	if !inReadTLS || !inWriteTLS {
		t.Fatalf("inbound raw TLS records missing by direction: client->proxy=%v proxy->client=%v", inReadTLS, inWriteTLS)
	}
	if !outReadTLS || !outWriteTLS {
		t.Fatalf("outbound raw TLS records missing by direction: upstream->proxy=%v proxy->upstream=%v", outReadTLS, outWriteTLS)
	}

	t.Logf("TLS E2E passed: client observed MailShield certificate CN=%s; upstream received complete message", clientTLS.PeerCertificates[0].Subject.CommonName)
	t.Logf("inbound TLS raw records: client->proxy=%v (%d events), proxy->client=%v (%d events)", inReadTLS, inReadCount, inWriteTLS, inWriteCount)
	t.Logf("outbound TLS raw records: upstream->proxy=%v (%d events), proxy->upstream=%v (%d events)", outReadTLS, outReadCount, outWriteTLS, outWriteCount)
	t.Logf("inbound plaintext capture after STARTTLS: client->proxy=%d bytes, proxy->client=%d bytes; outbound TLS state complete at upstream", len(inClientPlain), len(inServerPlain))
}

// Adapter keeps the relay session implementation local to this test and gives
// the SMTP server the backend type it expects.
type tlsE2ERelayBackendAdapter struct{ backend *tlsE2ERelayBackend }

func (a *tlsE2ERelayBackendAdapter) NewSession(c *esmtp.Conn) (esmtp.Session, error) {
	return a.backend.NewSession(c)
}
